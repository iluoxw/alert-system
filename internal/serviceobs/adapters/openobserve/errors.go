package openobserve

import (
	"errors"
	"fmt"

	"alert-system/internal/serviceobs/observe"
	"alert-system/internal/serviceobs/provider"
)

var (
	// ErrNotConnected 表示 OpenObserve client 未配置。
	ErrNotConnected = errors.New("openobserve: provider 未连接")
	// ErrInvalidBinding 表示 compiled binding 与 reader 期望的 source/kind 不匹配。
	ErrInvalidBinding = errors.New("openobserve: binding 与 reader 不匹配")
	// ErrInvalidIdentifier 表示 organization/stream/field 未通过 allowlist。
	ErrInvalidIdentifier = errors.New("openobserve: identifier 非法")
	// ErrInvalidLiteral 表示 focus 字面量不可安全转义。
	ErrInvalidLiteral = errors.New("openobserve: focus 字面量非法")
	// ErrSchemaError 表示上游返回 schema/SQL 错误（非 zero-hit 空结果）。
	ErrSchemaError = errors.New("openobserve: 上游 schema 或 SQL 错误")
	// ErrParseResponse 表示响应体无法按契约解析。
	ErrParseResponse = errors.New("openobserve: 响应解析失败")
)

// mapHTTPError 把 HTTP 状态与 provider 错误映射为 observe sentinel。
//
// 错误文本绝不回显 SQL、body、focus、org 或 stream。
func mapHTTPError(status int, body []byte) error {
	switch status {
	case 401, 403:
		return observe.ErrUpstreamAuthFailed
	case 429:
		return observe.ErrUpstreamRateLimited
	default:
		if status >= 500 {
			return fmt.Errorf("%w: status=%d", ErrSchemaError, status)
		}
		if status >= 400 {
			return ErrSchemaError
		}
		_ = body
		return ErrParseResponse
	}
}

func mapProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrResponseTooLarge) {
		return observe.ErrResponseTooLarge
	}
	return err
}
