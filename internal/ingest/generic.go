package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"alert-system/internal/domain"
)

// genericKind 是通用 webhook adapter 的注册名。
const genericKind = "generic"

// genericPayload 是 generic adapter 接受的 JSON 结构，字段与 domain.AlertEvent 一一对应
// （snake_case），供未使用特定告警工具、按本平台约定直接 POST 事件的调用方使用。
type genericPayload struct {
	Fingerprint      string            `json:"fingerprint,omitempty"`
	RuleID           string            `json:"rule_id"`
	SignalKind       string            `json:"signal_kind,omitempty"`
	Severity         string            `json:"severity"`
	SeverityFallback bool              `json:"severity_fallback,omitempty"`
	Status           string            `json:"status"`
	Title            string            `json:"title"`
	Labels           map[string]string `json:"labels,omitempty"`
	Source           string            `json:"source,omitempty"`
	ResourceRef      map[string]string `json:"resource_ref,omitempty"`
	ResolvePolicy    string            `json:"resolve_policy"`
	TTLSeconds       int               `json:"ttl_seconds,omitempty"`
	StartsAt         time.Time         `json:"starts_at"`
	EndsAt           time.Time         `json:"ends_at,omitempty"`
}

// knownGenericFields 是 genericPayload 已识别的 JSON key 集合；其余 key 整体归入
// SignalContext（未知字段透传，仅用于展示，见 domain.AlertEvent.SignalContext 的注释）。
var knownGenericFields = map[string]struct{}{
	"fingerprint": {}, "rule_id": {}, "signal_kind": {}, "severity": {},
	"severity_fallback": {}, "status": {}, "title": {}, "labels": {},
	"source": {}, "resource_ref": {}, "resolve_policy": {}, "ttl_seconds": {},
	"starts_at": {}, "ends_at": {},
}

// GenericAdapter 是 kind="generic" 的通用 webhook adapter：JSON key 直接对应信封字段
// （snake_case），单条对象或数组均接受；已知字段之外的 key 整体放入 SignalContext。
type GenericAdapter struct{}

// NewGenericAdapter 构造 GenericAdapter。
func NewGenericAdapter() *GenericAdapter {
	return &GenericAdapter{}
}

func (a *GenericAdapter) Kind() string { return genericKind }

func (a *GenericAdapter) Parse(body []byte) ([]domain.AlertEvent, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("请求体为空")
	}

	var rawItems []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return nil, fmt.Errorf("解析数组 payload 失败: %w", err)
		}
	} else {
		rawItems = []json.RawMessage{trimmed}
	}
	if len(rawItems) == 0 {
		return nil, errors.New("payload 数组不含任何元素")
	}

	events := make([]domain.AlertEvent, 0, len(rawItems))
	for i, raw := range rawItems {
		event, err := parseGenericItem(raw)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条 payload 解析失败: %w", i, err)
		}
		events = append(events, event)
	}
	return events, nil
}

// parseGenericItem 把单条 JSON 对象翻译为 domain.AlertEvent：已知字段按 genericPayload
// 映射，其余 key 整体序列化进 SignalContext。
func parseGenericItem(raw json.RawMessage) (domain.AlertEvent, error) {
	var p genericPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return domain.AlertEvent{}, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return domain.AlertEvent{}, err
	}
	for k := range knownGenericFields {
		delete(fields, k)
	}
	var signalContext json.RawMessage
	if len(fields) > 0 {
		extra, err := json.Marshal(fields)
		if err != nil {
			return domain.AlertEvent{}, fmt.Errorf("序列化未知字段失败: %w", err)
		}
		signalContext = extra
	}

	return domain.AlertEvent{
		Fingerprint:      p.Fingerprint,
		RuleID:           p.RuleID,
		SignalKind:       domain.SignalKind(p.SignalKind),
		Severity:         domain.Severity(p.Severity),
		SeverityFallback: p.SeverityFallback,
		Status:           domain.Status(p.Status),
		Title:            p.Title,
		Labels:           p.Labels,
		Source:           p.Source,
		ResourceRef:      p.ResourceRef,
		ResolvePolicy:    domain.ResolvePolicy(p.ResolvePolicy),
		TTLSeconds:       p.TTLSeconds,
		SignalContext:    signalContext,
		StartsAt:         p.StartsAt,
		EndsAt:           p.EndsAt,
	}, nil
}
