// Package prom is a read-only client for the two Prometheus-compatible
// backends this CLI reads: thanos-inner (no auth) and OpenObserve (Basic auth
// from an environment variable, plus SQL log search). Error semantics mirror
// the Python skill: auth failures are fatal (AuthError), query failures are
// per-check unknowns (QueryError).
package prom

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/luoxw/iacctl-go/internal/series"
)

// AuthError is fatal: credentials are missing or rejected. The CLI exits
// non-zero without producing a report, never fabricating a green one.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// QueryError marks one failed query; the corresponding check becomes unknown.
type QueryError struct{ Msg string }

func (e *QueryError) Error() string { return e.Msg }

// LogEntry records one outgoing query for the report appendix.
type LogEntry struct {
	Kind  string         `json:"kind"`
	Query string         `json:"query,omitempty"`
	Start float64        `json:"start,omitempty"`
	End   float64        `json:"end,omitempty"`
	Step  int            `json:"step,omitempty"`
	At    float64        `json:"at,omitempty"`
	SQL   string         `json:"sql,omitempty"`
	Raw   map[string]any `json:"-"`
}

// Client talks to one backend. PromPath is the prefix the client appends
// "/api/v1/query[_range]" to: "" for thanos and "/api/<org>/prometheus" for
// OpenObserve. SearchPath is only set for OpenObserve ("/api/<org>/_search").
type Client struct {
	BaseURL    string
	PromPath   string
	SearchPath string
	AuthHeader string // "" or "Basic ..."
	HTTP       *http.Client
	Log        []LogEntry
}

// New builds a client. authEnv empty means no auth (thanos); otherwise the
// environment variable must hold user:pass (OpenObserve OBS_AUTH).
func New(baseURL, promPath, searchPath, authEnv string) (*Client, error) {
	c := &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		PromPath:   promPath,
		SearchPath: searchPath,
		HTTP:       &http.Client{Timeout: 130 * time.Second},
	}
	if authEnv == "" {
		return c, nil
	}
	auth := os.Getenv(authEnv)
	if auth == "" {
		return nil, &AuthError{fmt.Sprintf("缺少环境变量 %s（user:pass 形式，用 zsh -ic 运行）", authEnv)}
	}
	if strings.HasPrefix(auth, "Basic ") || strings.HasPrefix(auth, "oma_ph_") {
		// Ready-made header value, either a literal "Basic <base64>" or an
		// OMA vault placeholder: the egress proxy substitutes the placeholder
		// verbatim, so the Authorization header must carry it as-is.
		// Base64-encoding a placeholder would never match.
		c.AuthHeader = auth
		return c, nil
	}
	user, pass, ok := strings.Cut(auth, ":")
	if !ok || user == "" || pass == "" {
		return nil, &AuthError{fmt.Sprintf("%s 须是 user:pass 或 Basic <base64> 形式", authEnv)}
	}
	c.AuthHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
	return c, nil
}

type rawRow struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
	Value  []any             `json:"value"`
}

type promResp struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// parseResult decodes data.result. Vector and matrix share the row shape;
// scalar and string results (e.g. the literal `1` used by probes) carry no
// series and decode to zero rows without an error.
func parseResult(pr promResp, vector bool) ([]series.Row, error) {
	var raws []rawRow
	if err := json.Unmarshal(pr.Data.Result, &raws); err != nil {
		switch pr.Data.ResultType {
		case "scalar", "string":
			return nil, nil
		}
		return nil, &QueryError{"结果解析失败：" + err.Error()}
	}
	return toRows(raws, vector), nil
}

func parseVal(raw any) (float64, bool) {
	s, ok := raw.(string)
	if !ok {
		return 0, false
	}
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &v); err != nil {
		return 0, false
	}
	if v != v { // NaN
		return 0, false
	}
	return v, true
}

func toRows(raws []rawRow, vector bool) []series.Row {
	rows := make([]series.Row, 0, len(raws))
	for _, rr := range raws {
		out := series.Row{Metric: rr.Metric}
		if vector {
			if len(rr.Value) == 2 {
				if ts, ok1 := rr.Value[0].(float64); ok1 {
					if v, ok2 := parseVal(rr.Value[1]); ok2 {
						out.Values = append(out.Values, series.Sample{T: ts, V: v})
					}
				}
			}
		} else {
			for _, pair := range rr.Values {
				if len(pair) != 2 {
					continue
				}
				ts, ok1 := pair[0].(float64)
				if !ok1 {
					continue
				}
				v, ok2 := parseVal(pair[1])
				if !ok2 {
					continue
				}
				out.Values = append(out.Values, series.Sample{T: ts, V: v})
			}
		}
		if len(out.Values) > 0 {
			rows = append(rows, out)
		}
	}
	return rows
}

func (c *Client) do(ctx context.Context, method, rawURL string, body io.Reader, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, &QueryError{err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(ctx2)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &QueryError{"网络失败：" + err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, &QueryError{"读取响应失败：" + err.Error()}
	}
	switch resp.StatusCode {
	case 200:
		return data, nil
	case 401, 403:
		return nil, &AuthError{fmt.Sprintf("认证失败（HTTP %d）", resp.StatusCode)}
	default:
		return nil, &QueryError{fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
}

// Prom runs an instant query at the given unix time.
func (c *Client) Prom(ctx context.Context, expr string, at float64) ([]series.Row, error) {
	q := url.Values{}
	q.Set("query", expr)
	q.Set("time", fmt.Sprintf("%.3f", at))
	c.Log = append(c.Log, LogEntry{Kind: "promql", Query: expr, At: at})
	data, err := c.do(ctx, http.MethodGet, c.BaseURL+c.PromPath+"/api/v1/query?"+q.Encode(), nil, 90*time.Second)
	if err != nil {
		return nil, err
	}
	var pr promResp
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, &QueryError{"响应不是 JSON"}
	}
	if pr.Status != "success" {
		msg := pr.Error
		if msg == "" {
			msg = "promql 失败"
		}
		return nil, &QueryError{msg}
	}
	return parseResult(pr, true)
}

// PromRange runs a range query. step is seconds.
func (c *Client) PromRange(ctx context.Context, expr string, start, end float64, step int) ([]series.Row, error) {
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", fmt.Sprintf("%.3f", start))
	q.Set("end", fmt.Sprintf("%.3f", end))
	q.Set("step", fmt.Sprintf("%d", step))
	c.Log = append(c.Log, LogEntry{Kind: "promql_range", Query: expr, Start: start, End: end, Step: step})
	data, err := c.do(ctx, http.MethodGet, c.BaseURL+c.PromPath+"/api/v1/query_range?"+q.Encode(), nil, 120*time.Second)
	if err != nil {
		return nil, err
	}
	var pr promResp
	if err := json.Unmarshal(data, &pr); err != nil {
		return nil, &QueryError{"响应不是 JSON"}
	}
	if pr.Status != "success" {
		msg := pr.Error
		if msg == "" {
			msg = "query_range 失败"
		}
		return nil, &QueryError{msg}
	}
	return parseResult(pr, false)
}

// GetJSON runs a read-only GET on the backend (e.g. the OpenObserve streams
// API) and returns the parsed JSON object.
func (c *Client) GetJSON(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	u := c.BaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	c.Log = append(c.Log, LogEntry{Kind: "get", Query: path})
	data, err := c.do(ctx, http.MethodGet, u, nil, 60*time.Second)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &QueryError{"响应不是 JSON"}
	}
	return out, nil
}

// SearchSQL runs an OpenObserve SQL search over a log/traces stream.
// start/end are unix seconds; the API takes microseconds internally.
func (c *Client) SearchSQL(ctx context.Context, streamType, sql string, start, end float64, size int) (map[string]any, error) {
	if c.SearchPath == "" {
		return nil, &QueryError{"该后端不支持 SQL 检索"}
	}
	if size <= 0 {
		size = 500
	}
	body := map[string]any{
		"query": map[string]any{
			"sql":        sql,
			"start_time": int64(start * 1_000_000),
			"end_time":   int64(end * 1_000_000),
			"from":       0,
			"size":       size,
			"timezone":   "+08:00",
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, &QueryError{err.Error()}
	}
	c.Log = append(c.Log, LogEntry{Kind: "sql", SQL: sql, Start: start, End: end})
	data, err := c.do(ctx, http.MethodPost, c.BaseURL+c.SearchPath+"?type="+streamType, strings.NewReader(string(raw)), 120*time.Second)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &QueryError{"响应不是 JSON"}
	}
	return out, nil
}
