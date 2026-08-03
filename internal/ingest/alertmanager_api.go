package ingest

import (
	"encoding/json"
	"fmt"
	"time"

	"alert-system/internal/domain"
)

// AlertmanagerAPIAlert 对齐官方 API v2 gettableAlert。API 只返回当前 Alert 视图，
// status.state 是 active/suppressed/unprocessed，不是 firing/resolved 生命周期枚举。
type AlertmanagerAPIAlert struct {
	Annotations map[string]string `json:"annotations"`
	Receivers   []struct {
		Name string `json:"name"`
	} `json:"receivers"`
	Fingerprint  string            `json:"fingerprint"`
	StartsAt     time.Time         `json:"startsAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Labels       map[string]string `json:"labels"`
	Status       struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
		MutedBy     []string `json:"mutedBy"`
	} `json:"status"`
}

// AlertmanagerAPIEvent 把 gettableAlert 归一化为权威镜像事件。API 返回的 EndsAt 对
// firing Alert 可能是预估有效期，不能据此判 resolved；生命周期固定为 firing，真正
// resolved 由 webhook 或连续完整快照缺失确认。
func AlertmanagerAPIEvent(alert AlertmanagerAPIAlert, defaultSeverity domain.Severity) (domain.AlertEvent, error) {
	if alert.Fingerprint == "" {
		return domain.AlertEvent{}, fmt.Errorf("Alertmanager API alert fingerprint 不能为空")
	}
	if alert.StartsAt.IsZero() {
		return domain.AlertEvent{}, fmt.Errorf("Alertmanager API alert startsAt 不能为空")
	}
	ruleID := alert.Labels["alertname"]
	if ruleID == "" {
		return domain.AlertEvent{}, fmt.Errorf("Alertmanager API alert labels.alertname 不能为空")
	}
	severity, fallback := mapSeverity(alert.Labels["severity"], defaultSeverity)
	receivers := make([]string, 0, len(alert.Receivers))
	for _, receiver := range alert.Receivers {
		receivers = append(receivers, receiver.Name)
	}
	details := map[string][]string{
		"silenced_by":  alert.Status.SilencedBy,
		"inhibited_by": alert.Status.InhibitedBy,
		"muted_by":     alert.Status.MutedBy,
	}
	suppression := domain.SuppressionStateNone
	switch {
	case len(alert.Status.SilencedBy) > 0 || len(alert.Status.MutedBy) > 0:
		suppression = domain.SuppressionStateSilenced
	case len(alert.Status.InhibitedBy) > 0:
		suppression = domain.SuppressionStateInhibited
	}
	contextJSON, err := json.Marshal(map[string]any{
		"generator_url":       alert.GeneratorURL,
		"vendor_fingerprint":  alert.Fingerprint,
		"annotations":         alert.Annotations,
		"receivers":           receivers,
		"alertmanager_status": alert.Status.State,
		"api_ends_at":         alert.EndsAt,
	})
	if err != nil {
		return domain.AlertEvent{}, fmt.Errorf("序列化 Alertmanager API signal_context 失败: %w", err)
	}
	updatedAt := alert.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = alert.StartsAt
	}
	return domain.AlertEvent{
		Fingerprint:         alert.Fingerprint,
		UpstreamFingerprint: alert.Fingerprint,
		RuleID:              ruleID,
		SignalKind:          domain.SignalKindMetric,
		Severity:            severity,
		SeverityFallback:    fallback,
		Status:              domain.StatusFiring,
		Title:               titleFromAnnotations(alert.Annotations, ruleID),
		Labels:              alert.Labels,
		Source:              alertmanagerKind,
		ResourceRef:         normalizeResourceRef(alert.Labels),
		ResolvePolicy:       domain.ResolvePolicySource,
		SignalContext:       contextJSON,
		StartsAt:            alert.StartsAt,
		SourceUpdatedAt:     updatedAt,
		SuppressionState:    suppression,
		SuppressionDetails:  details,
	}, nil
}
