package openobserve

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// searchResponse 是 OpenObserve Search API 的成功响应子集。
type searchResponse struct {
	Hits  []map[string]json.RawMessage `json:"hits"`
	Total int64                        `json:"total"`
	From  int64                        `json:"from"`
	Size  int64                        `json:"size"`
	Took  int64                        `json:"took"`
}

// apiErrorResponse 是 OpenObserve 的错误响应子集。
type apiErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func parseSearchResponse(body []byte) (searchResponse, error) {
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return searchResponse{}, ErrParseResponse
	}
	if resp.Hits == nil {
		resp.Hits = []map[string]json.RawMessage{}
	}
	return resp, nil
}

func isSchemaError(status int, body []byte) bool {
	if status == 401 || status == 403 || status == 429 {
		return false
	}
	if status >= 400 {
		return true
	}
	var apiErr apiErrorResponse
	if err := json.Unmarshal(body, &apiErr); err != nil {
		return false
	}
	switch apiErr.Code {
	case 20002, 20003, 20004, 20005, 20008, 20011:
		return true
	default:
		return apiErr.Code >= 20000 && apiErr.Message != ""
	}
}

func recordTimestamp(raw map[string]json.RawMessage) time.Time {
	if value, ok := raw["_timestamp"]; ok {
		if ts, ok := parseTimestampValue(value); ok {
			return ts
		}
	}
	return time.Time{}
}

func parseTimestampValue(raw json.RawMessage) (time.Time, bool) {
	var micros int64
	if err := json.Unmarshal(raw, &micros); err == nil && micros > 0 {
		return time.UnixMicro(micros).UTC(), true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return time.Time{}, false
		}
		if parsed, err := strconv.ParseInt(asString, 10, 64); err == nil && parsed > 0 {
			return time.UnixMicro(parsed).UTC(), true
		}
	}
	return time.Time{}, false
}

func fieldString(raw map[string]json.RawMessage, field string) string {
	value, ok := raw[field]
	if !ok {
		return ""
	}
	var asString string
	if err := json.Unmarshal(value, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	return ""
}

func latestObservedAt(hits []map[string]json.RawMessage) time.Time {
	var latest time.Time
	for _, hit := range hits {
		ts := recordTimestamp(hit)
		if ts.After(latest) {
			latest = ts
		}
	}
	return latest
}

func hitsWithinRowLimit(hits []map[string]json.RawMessage, limit int) error {
	if limit > 0 && len(hits) > limit {
		return fmt.Errorf("%w: rows=%d limit=%d", ErrParseResponse, len(hits), limit)
	}
	return nil
}
