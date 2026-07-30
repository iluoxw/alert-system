package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"alert-system/internal/domain"
)

// grafanaKind 是 Grafana webhook adapter 的注册名。
const grafanaKind = "grafana"

// grafanaWebhook 对应 Grafana Unified Alerting 的 webhook 通知器投递的 payload。
// Grafana ngalert 的 webhook sender 复用了 Alertmanager 的 template.Data 结构，
// 在其基础上扩展了 orgId/title/state/message；version/groupKey/truncatedAlerts
// 与 Alertmanager 同名同义。
type grafanaWebhook struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	OrgID             int64             `json:"orgId"`
	Title             string            `json:"title"`
	State             string            `json:"state"`
	Message           string            `json:"message"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []grafanaAlert    `json:"alerts"`
}

// grafanaAlert 是 Grafana alerts 数组里单条告警的结构：在 Alertmanager 的
// template.Alert 基础上扩展了 dashboardURL/panelURL/silenceURL/values/valueString/
// imageURL（Grafana 特有的可跳转/取值信息），这些字段全部进 SignalContext，不参与
// 信封字段（R2.3：核心管道不读取 signal_context）。
type grafanaAlert struct {
	Status       string             `json:"status"`
	Labels       map[string]string  `json:"labels"`
	Annotations  map[string]string  `json:"annotations"`
	StartsAt     time.Time          `json:"startsAt"`
	EndsAt       time.Time          `json:"endsAt"`
	GeneratorURL string             `json:"generatorURL"`
	Fingerprint  string             `json:"fingerprint"`
	SilenceURL   string             `json:"silenceURL,omitempty"`
	DashboardURL string             `json:"dashboardURL,omitempty"`
	PanelURL     string             `json:"panelURL,omitempty"`
	Values       map[string]float64 `json:"values,omitempty"`
	ValueString  string             `json:"valueString,omitempty"`
	ImageURL     string             `json:"imageURL,omitempty"`
}

// GrafanaAdapter 把 Grafana Unified Alerting webhook payload 翻译为统一信封（R1.2）。
// ResolvePolicy 固定为 source：Grafana 同样会在恢复时推送 status=resolved（R4.3）。
type GrafanaAdapter struct {
	defaultSeverity domain.Severity
}

// NewGrafanaAdapter 构造 GrafanaAdapter；defaultSeverity 语义与 NewAlertmanagerAdapter
// 一致，同样受限于 Parse 签名不携带 integration 上下文（见 alertmanager.go 的注释）。
func NewGrafanaAdapter(defaultSeverity domain.Severity) *GrafanaAdapter {
	if !defaultSeverity.IsValid() {
		defaultSeverity = domain.SeverityWarning
	}
	return &GrafanaAdapter{defaultSeverity: defaultSeverity}
}

func (a *GrafanaAdapter) Kind() string { return grafanaKind }

func (a *GrafanaAdapter) Parse(body []byte) ([]domain.AlertEvent, error) {
	var payload grafanaWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Grafana payload 失败: %w", err)
	}
	if len(payload.Alerts) == 0 {
		return nil, errors.New("payload 不含任何告警（alerts 为空）")
	}

	events := make([]domain.AlertEvent, 0, len(payload.Alerts))
	for i, alert := range payload.Alerts {
		event, err := a.translate(payload, alert)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条告警翻译失败: %w", i, err)
		}
		events = append(events, event)
	}
	return events, nil
}

func (a *GrafanaAdapter) translate(payload grafanaWebhook, alert grafanaAlert) (domain.AlertEvent, error) {
	status, err := parseVendorStatus(alert.Status)
	if err != nil {
		return domain.AlertEvent{}, err
	}

	ruleID := alert.Labels["alertname"]
	if ruleID == "" {
		return domain.AlertEvent{}, errors.New("labels.alertname 不能为空")
	}

	severity, fallback := mapSeverity(alert.Labels["severity"], a.defaultSeverity)

	signalContext, err := json.Marshal(map[string]any{
		"generator_url":      alert.GeneratorURL,
		"vendor_fingerprint": alert.Fingerprint,
		"annotations":        alert.Annotations,
		"receiver":           payload.Receiver,
		"group_key":          payload.GroupKey,
		"external_url":       payload.ExternalURL,
		"org_id":             payload.OrgID,
		"dashboard_url":      alert.DashboardURL,
		"panel_url":          alert.PanelURL,
		"silence_url":        alert.SilenceURL,
		"values":             alert.Values,
		"value_string":       alert.ValueString,
		"image_url":          alert.ImageURL,
	})
	if err != nil {
		return domain.AlertEvent{}, fmt.Errorf("序列化 signal_context 失败: %w", err)
	}

	return domain.AlertEvent{
		RuleID:           ruleID,
		SignalKind:       domain.SignalKindMetric,
		Severity:         severity,
		SeverityFallback: fallback,
		Status:           status,
		Title:            titleFromAnnotations(alert.Annotations, ruleID),
		Labels:           alert.Labels,
		Source:           grafanaKind,
		ResourceRef:      normalizeResourceRef(alert.Labels),
		ResolvePolicy:    domain.ResolvePolicySource,
		SignalContext:    signalContext,
		StartsAt:         alert.StartsAt,
		EndsAt:           alert.EndsAt,
	}, nil
}
