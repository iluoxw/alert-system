package openobserve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"alert-system/internal/serviceobs/integration"
)

const (
	maxIdentifierLen = 128
	maxLiteralLen    = 512
	maxSelectFields  = 32
)

// searchRequest 是 OpenObserve Search API 的请求体。
type searchRequest struct {
	Query struct {
		SQL       string `json:"sql"`
		StartTime int64  `json:"start_time"`
		EndTime   int64  `json:"end_time"`
		From      int64  `json:"from"`
		Size      int64  `json:"size"`
	} `json:"query"`
	SearchType string `json:"search_type"`
	Timeout    int    `json:"timeout"`
}

// queryPlan 是一次可执行的上游查询计划。
type queryPlan struct {
	Path        string
	Body        []byte
	Fingerprint string
	RowLimit    int
}

// validateIdentifier 校验 organization/stream/field 名称。
//
// 只允许 ASCII 字母、数字、下划线与点；首字符必须是字母或下划线。
// sandboxid 不会被 case folding 或 substring 接受——调用方必须传精确的 focus_field。
func validateIdentifier(name string) error {
	if name == "" || len(name) > maxIdentifierLen {
		return ErrInvalidIdentifier
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			if i == 0 && r >= '0' && r <= '9' {
				return ErrInvalidIdentifier
			}
		default:
			return ErrInvalidIdentifier
		}
	}
	return nil
}

// escapeSQLLiteral 按 SQL 单引号字面量规则转义。
func escapeSQLLiteral(value string) (string, error) {
	if value == "" {
		return "", ErrInvalidLiteral
	}
	if len(value) > maxLiteralLen {
		return "", ErrInvalidLiteral
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return "", ErrInvalidLiteral
		}
	}
	return strings.ReplaceAll(value, "'", "''"), nil
}

func encodePathSegment(segment string) (string, error) {
	if err := validateIdentifier(segment); err != nil {
		return "", err
	}
	return url.PathEscape(segment), nil
}

func timeToMicros(t time.Time) int64 {
	return t.UTC().UnixMicro()
}

func effectiveRowLimit(binding integration.CompiledBinding, profileMax, requestMax int) int {
	limit := requestMax
	if binding.Params.RowLimit != nil && *binding.Params.RowLimit > 0 && *binding.Params.RowLimit < limit {
		limit = *binding.Params.RowLimit
	}
	if profileMax > 0 && profileMax < limit {
		limit = profileMax
	}
	if limit <= 0 {
		limit = 200
	}
	return limit
}

func collectSelectFields(binding integration.CompiledBinding) ([]string, error) {
	seen := map[string]struct{}{}
	add := func(fields *[]string, field string) error {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil
		}
		if err := validateIdentifier(field); err != nil {
			return err
		}
		if _, ok := seen[field]; ok {
			return nil
		}
		seen[field] = struct{}{}
		*fields = append(*fields, field)
		return nil
	}

	fields := make([]string, 0, 8)
	for _, field := range binding.Params.SelectFields {
		if err := add(&fields, field); err != nil {
			return nil, err
		}
	}
	if err := add(&fields, binding.Params.FocusField); err != nil {
		return nil, err
	}
	if err := add(&fields, binding.Params.StageField); err != nil {
		return nil, err
	}
	if err := add(&fields, binding.Params.StatusField); err != nil {
		return nil, err
	}
	for _, mapping := range binding.Params.IdentityMappings {
		if err := add(&fields, mapping.Field); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"trace_id", "span_id", "operation_name", "service_name", "_timestamp"} {
		if err := add(&fields, field); err != nil {
			return nil, err
		}
	}
	if len(fields) == 0 {
		return nil, ErrInvalidIdentifier
	}
	if len(fields) > maxSelectFields {
		return nil, ErrInvalidIdentifier
	}
	return fields, nil
}

// buildSearchQuery 生成固定模板 SQL 与 JSON body。
//
// SQL 形如：SELECT f1, f2 FROM stream WHERE focus_field = 'literal'
func buildSearchQuery(
	org string,
	binding integration.CompiledBinding,
	focusValue string,
	from, to time.Time,
	rowLimit int,
) (queryPlan, error) {
	if err := validateIdentifier(org); err != nil {
		return queryPlan{}, err
	}
	if err := validateIdentifier(binding.Params.Stream); err != nil {
		return queryPlan{}, err
	}
	if err := validateIdentifier(binding.Params.FocusField); err != nil {
		return queryPlan{}, err
	}
	selectFields, err := collectSelectFields(binding)
	if err != nil {
		return queryPlan{}, err
	}
	escaped, err := escapeSQLLiteral(focusValue)
	if err != nil {
		return queryPlan{}, err
	}

	quotedFields := make([]string, len(selectFields))
	for i, field := range selectFields {
		quotedFields[i] = quoteIdentifier(field)
	}
	sql := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s = '%s'",
		strings.Join(quotedFields, ", "),
		quoteIdentifier(binding.Params.Stream),
		quoteIdentifier(binding.Params.FocusField),
		escaped,
	)

	var body searchRequest
	body.Query.SQL = sql
	body.Query.StartTime = timeToMicros(from)
	body.Query.EndTime = timeToMicros(to)
	body.Query.From = 0
	body.Query.Size = int64(rowLimit)
	body.SearchType = "ui"
	body.Timeout = 0

	raw, err := json.Marshal(body)
	if err != nil {
		return queryPlan{}, fmt.Errorf("openobserve: 构造请求失败")
	}

	orgSegment, err := encodePathSegment(org)
	if err != nil {
		return queryPlan{}, err
	}
	fingerprint := queryFingerprint(binding, focusValue, from, to, rowLimit, sql)
	return queryPlan{
		Path:        "api/" + orgSegment + "/_search",
		Body:        raw,
		Fingerprint: fingerprint,
		RowLimit:    rowLimit,
	}, nil
}

func quoteIdentifier(name string) string {
	// 已通过 validateIdentifier；双引号包裹以支持保留字。
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func queryFingerprint(
	binding integration.CompiledBinding,
	focusValue string,
	from, to time.Time,
	rowLimit int,
	sql string,
) string {
	h := sha256.New()
	_, _ = h.Write([]byte(binding.TemplateRef))
	_, _ = h.Write([]byte("\x1e"))
	_, _ = h.Write([]byte(binding.Params.Stream))
	_, _ = h.Write([]byte("\x1e"))
	_, _ = h.Write([]byte(binding.Params.FocusField))
	_, _ = h.Write([]byte("\x1e"))
	_, _ = h.Write([]byte(focusValue))
	_, _ = h.Write([]byte("\x1e"))
	_, _ = h.Write([]byte(from.UTC().Format(time.RFC3339Nano)))
	_, _ = h.Write([]byte("\x1e"))
	_, _ = h.Write([]byte(to.UTC().Format(time.RFC3339Nano)))
	_, _ = h.Write([]byte("\x1e"))
	_, _ = fmt.Fprintf(h, "%d", rowLimit)
	_, _ = h.Write([]byte("\x1e"))
	_, _ = h.Write([]byte(sql))
	return hex.EncodeToString(h.Sum(nil))
}
