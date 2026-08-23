package prometheus

import (
	"errors"
	"fmt"

	"alert-system/internal/serviceobs/observe"
	"alert-system/internal/serviceobs/provider"
)

// MapHTTPStatus 把 HTTP 状态码映射为 observe sentinel 或 typed adapter 错误。
func MapHTTPStatus(status int) error {
	switch status {
	case 401, 403:
		return observe.ErrUpstreamAuthFailed
	case 429:
		return observe.ErrUpstreamRateLimited
	default:
		if status >= 400 {
			return fmt.Errorf("%w: status=%d", ErrUpstreamReadFailed, status)
		}
		return nil
	}
}

// MapProviderError 把 provider.Client 错误映射为 observe sentinel。
func MapProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrResponseTooLarge) {
		return observe.ErrResponseTooLarge
	}
	if errors.Is(err, ErrCardinalityExceeded) {
		return observe.ErrCardinalityExceeded
	}
	return fmt.Errorf("%w: %w", ErrUpstreamReadFailed, err)
}

var (
	// ErrBindingMismatch 表示 compiled binding 与 Prometheus adapter 期望不一致。
	ErrBindingMismatch = errors.New("prometheus: binding 与 adapter 不匹配")
	// ErrInvalidParams 表示 typed params 无法安全构造 PromQL。
	ErrInvalidParams = errors.New("prometheus: 参数非法")
	// ErrParseFailed 表示响应 envelope 无法解析。
	ErrParseFailed = errors.New("prometheus: 响应解析失败")
	// ErrUnsupportedResult 表示 resultType 或样本形状不受支持。
	ErrUnsupportedResult = errors.New("prometheus: 不支持的查询结果")
	// ErrInvalidSample 表示样本值非法（NaN/Inf/非数字）。
	ErrInvalidSample = errors.New("prometheus: 样本值非法")
	// ErrUpstreamReadFailed 表示上游读取失败（5xx、协议错误等）。
	ErrUpstreamReadFailed = errors.New("prometheus: 上游读取失败")
	// ErrCardinalityExceeded 表示 series 超过上界。
	ErrCardinalityExceeded = errors.New("prometheus: series 基数超限")
)
