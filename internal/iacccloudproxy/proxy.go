// Package iacccloudproxy implements the official ccloud-cli's fixed IAC L4
// read-only subset. The broad CCloud credential never leaves the API process.
package iacccloudproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	Prefix        = "/api/v1/iac-ccloud"
	AppID         = "80beff42ab0b41819f9c1c3c3ac95413308e70eb"
	resourcesPath = "/cloud/api/v1/agent/meta/resources"
	actionsPath   = resourcesPath + "/app/actions"
	podsPath      = "/cloud/api/v1/agent/app/pods"
	upstream      = "https://ccloud.gz.cvte.cn" + podsPath
	maxResponse   = 2 << 20
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)

type Options struct {
	ProxyToken    string
	CloudAPIKey   string
	Client        *http.Client
	Timeout       time.Duration
	MaxConcurrent int
}
type Handler struct {
	token   [32]byte
	key     string
	client  *http.Client
	timeout time.Duration
	slots   chan struct{}
}

func New(o Options) (*Handler, error) {
	if len(o.ProxyToken) < 32 || strings.TrimSpace(o.ProxyToken) != o.ProxyToken {
		return nil, fmt.Errorf("iacccloudproxy: proxy token must contain at least 32 bytes without surrounding whitespace")
	}
	if o.CloudAPIKey == "" || strings.ContainsAny(o.CloudAPIKey, "\r\n") || strings.HasPrefix(o.CloudAPIKey, "Bearer ") {
		return nil, fmt.Errorf("iacccloudproxy: raw server-side CCloud API key is required")
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Timeout < time.Second || o.Timeout > 30*time.Second {
		return nil, fmt.Errorf("iacccloudproxy: timeout must be within [1s,30s]")
	}
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 2
	}
	if o.MaxConcurrent < 1 || o.MaxConcurrent > 8 {
		return nil, fmt.Errorf("iacccloudproxy: concurrent requests must be within [1,8]")
	}
	client := http.Client{Timeout: o.Timeout}
	if o.Client != nil {
		client = *o.Client
	}
	// The fixed trusted endpoint may not redirect a privileged request elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Handler{token: sha256.Sum256([]byte(o.ProxyToken)), key: o.CloudAPIKey, client: &client, timeout: o.Timeout, slots: make(chan struct{}, o.MaxConcurrent)}, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Authorization")
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	got := sha256.Sum256([]byte(auth))
	if !ok || subtle.ConstantTimeCompare(got[:], h.token[:]) != 1 {
		writeError(w, 401, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, 405, "read-only endpoint")
		return
	}
	// Official ccloud-cli sends an exact empty object on GET action requests.
	// It carries no query parameters; every other body/chunked request is denied.
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	if len(r.TransferEncoding) != 0 || (r.ContentLength != 0 && (path != podsPath || r.ContentLength != 2)) {
		writeError(w, 400, "request body is not allowed")
		return
	}
	if r.ContentLength == 2 {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(time.Second))
		body, err := io.ReadAll(io.LimitReader(r.Body, 3))
		_ = controller.SetReadDeadline(time.Time{})
		if err != nil || !bytes.Equal(body, []byte("{}")) {
			writeError(w, 400, "request body is not allowed")
			return
		}
	}
	if r.URL.RawPath != "" {
		writeError(w, 400, "encoded paths are not allowed")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, 400, "invalid query")
		return
	}
	switch path {
	case resourcesPath:
		if len(query) != 0 {
			writeError(w, 400, "query is not allowed")
			return
		}
		writeData(w, map[string]any{"resources": []map[string]any{{"name": "app", "summary": "fixed IAC PRO pods read-only"}}})
	case actionsPath:
		if len(query) != 0 {
			writeError(w, 400, "query is not allowed")
			return
		}
		writeData(w, map[string]any{"resource": "app", "version": 1, "actions": []any{podsDefinition()}})
	case podsPath:
		if len(query) != 2 || len(query["appId"]) != 1 || len(query["clusterId"]) != 1 || query.Get("appId") != AppID || query.Get("clusterId") != "60" {
			writeError(w, 403, "scope is not allowed")
			return
		}
		h.pods(w, r)
	default:
		writeError(w, 404, "not found")
	}
}
func podsDefinition() map[string]any {
	arg := func(name, kind string) map[string]any {
		return map[string]any{"name": name, "description": "fixed IAC PRO scope", "location": "query", "type": kind, "required": true, "defaultValue": nil, "itemType": nil, "fileInputEnabled": false}
	}
	return map[string]any{"name": "pods", "description": "read fixed IAC PRO pod snapshot", "method": "GET", "pathTemplate": podsPath, "requiresAuth": true, "dangerous": false, "executionMode": "HTTP_JSON", "arguments": []any{arg("appId", "string"), arg("clusterId", "number")}}
}

type Pod struct {
	Name     string `json:"podName"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int    `json:"restartCount"`
	Node     string `json:"nodeName"`
}

func (h *Handler) pods(w http.ResponseWriter, r *http.Request) {
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		writeError(w, 503, "observation busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"?appId="+AppID+"&clusterId=60", nil)
	if err != nil {
		writeError(w, 502, "observation unavailable")
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		writeError(w, 502, "observation unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeError(w, 502, "observation unavailable")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		writeError(w, 502, "invalid observation")
		return
	}
	pods, err := parsePods(raw)
	if err != nil {
		writeError(w, 502, "invalid observation")
		return
	}
	writeData(w, pods)
}
func parsePods(raw []byte) ([]Pod, error) {
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Code == nil || *envelope.Code != 0 {
		return nil, fmt.Errorf("upstream failure")
	}
	var rows []struct {
		Name     *string `json:"podName"`
		Phase    *string `json:"phase"`
		Ready    *bool   `json:"ready"`
		Restarts *int    `json:"restartCount"`
		Node     string  `json:"nodeName"`
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, fmt.Errorf("missing pods")
	}
	if err := json.Unmarshal(envelope.Data, &rows); err != nil || rows == nil || len(rows) > 500 {
		return nil, fmt.Errorf("invalid pods")
	}
	out := make([]Pod, 0, len(rows))
	for _, p := range rows {
		if p.Name == nil || p.Phase == nil || p.Ready == nil || p.Restarts == nil || !safeName.MatchString(*p.Name) || *p.Restarts < 0 || (p.Node != "" && !safeName.MatchString(p.Node)) {
			return nil, fmt.Errorf("incomplete pods")
		}
		switch *p.Phase {
		case "Running", "Pending", "Succeeded", "Failed", "Unknown":
		default:
			return nil, fmt.Errorf("invalid pod phase")
		}
		out = append(out, Pod{*p.Name, *p.Phase, *p.Ready, *p.Restarts, p.Node})
	}
	return out, nil
}
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "message": message, "data": nil})
}
func writeData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "", "data": data})
}
