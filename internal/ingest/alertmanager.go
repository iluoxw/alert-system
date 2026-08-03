package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"alert-system/internal/domain"
)

// alertmanagerKind 是 Alertmanager webhook adapter 的注册名。
const alertmanagerKind = "alertmanager"

// alertmanagerWebhook 对应 Alertmanager webhook_config 投递的 payload 顶层结构
// （prometheus/alertmanager/template.Data 内嵌进 WebhookMessage，外加
// version/groupKey/truncatedAlerts 三个字段）。字段名与官方文档一致，不做重命名。
type alertmanagerWebhook struct {
	Version           string              `json:"version"`
	GroupKey          string              `json:"groupKey"`
	TruncatedAlerts   int                 `json:"truncatedAlerts"`
	Status            string              `json:"status"`
	Receiver          string              `json:"receiver"`
	GroupLabels       map[string]string   `json:"groupLabels"`
	CommonLabels      map[string]string   `json:"commonLabels"`
	CommonAnnotations map[string]string   `json:"commonAnnotations"`
	ExternalURL       string              `json:"externalURL"`
	Alerts            []alertmanagerAlert `json:"alerts"`
}

// alertmanagerAlert 是 alerts 数组里单条告警的结构（template.Alert）。
type alertmanagerAlert struct {
	Status       string            `json:"status"` // "firing" 或 "resolved"
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"` // 未 resolved 时为零值时间的 RFC3339 表示
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"` // Alertmanager 自己的指纹，仅供展示，不替代 domain.Fingerprint
}

// AlertmanagerAdapter 把 Alertmanager webhook_config 投递的 payload 翻译为统一信封
// （R1.1）。ResolvePolicy 固定为 source：Alertmanager 会在告警恢复时主动推送
// status=resolved 事件，管道据此自动恢复，不需要 TTL 兜底（R4.3）。
type AlertmanagerAdapter struct {
	defaultSeverity domain.Severity
}

// NewAlertmanagerAdapter 构造 AlertmanagerAdapter。defaultSeverity 是上游 labels 缺失
// severity 或取值无法映射时的兜底等级（R2.5）；留空/非法则回退为 domain.SeverityWarning
// （与 integrations 表 default_severity 列的 DEFAULT 'warning' 对齐）。
//
// 注意：InboundAdapter.Parse 的签名（任务 17 定义，本任务不可改）不携带 integration
// 上下文，因此这里的兜底等级是构造时固定的单个值，无法做到「同一 kind 下不同
// integration 各自使用 integrations.default_severity 列的值」。main 装配（后续任务）
// 如需按 integration 区分兜底等级，需要按 integration 各自 New 一个实例并自行路由，
// 或调整 InboundAdapter 接口签名——两者都超出本任务范围，留给后续任务判断。
func NewAlertmanagerAdapter(defaultSeverity domain.Severity) *AlertmanagerAdapter {
	if !defaultSeverity.IsValid() {
		defaultSeverity = domain.SeverityWarning
	}
	return &AlertmanagerAdapter{defaultSeverity: defaultSeverity}
}

func (a *AlertmanagerAdapter) Kind() string { return alertmanagerKind }

func (a *AlertmanagerAdapter) Parse(body []byte) ([]domain.AlertEvent, error) {
	var payload alertmanagerWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 Alertmanager payload 失败: %w", err)
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

func (a *AlertmanagerAdapter) translate(payload alertmanagerWebhook, alert alertmanagerAlert) (domain.AlertEvent, error) {
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
	})
	if err != nil {
		return domain.AlertEvent{}, fmt.Errorf("序列化 signal_context 失败: %w", err)
	}
	sourceUpdatedAt := alert.StartsAt
	if status == domain.StatusResolved && !alert.EndsAt.IsZero() {
		sourceUpdatedAt = alert.EndsAt
	}

	return domain.AlertEvent{
		RuleID:              ruleID,
		UpstreamFingerprint: alert.Fingerprint,
		SignalKind:          domain.SignalKindMetric,
		Severity:            severity,
		SeverityFallback:    fallback,
		Status:              status,
		Title:               titleFromAnnotations(alert.Annotations, ruleID),
		Labels:              alert.Labels,
		Source:              alertmanagerKind,
		ResourceRef:         normalizeResourceRef(alert.Labels),
		ResolvePolicy:       domain.ResolvePolicySource,
		SignalContext:       signalContext,
		StartsAt:            alert.StartsAt,
		EndsAt:              alert.EndsAt,
		SourceUpdatedAt:     sourceUpdatedAt,
		SuppressionState:    domain.SuppressionStateNone,
	}, nil
}

// --- 以下为 Alertmanager/Grafana 两个 adapter 共用的翻译辅助函数 ---
// （Grafana 的 webhook payload 是在 Alertmanager 的 template.Data 基础上扩展的，
// status/labels/annotations 语义完全一致，因此 grafana.go 直接复用这里的实现。）

// parseVendorStatus 把两家共用的 status 字符串（"firing"/"resolved"）翻译为
// domain.Status；两者取值恰好与 domain.Status 的字符串常量一致，直接转换校验。
func parseVendorStatus(status string) (domain.Status, error) {
	s := domain.Status(status)
	if s != domain.StatusFiring && s != domain.StatusResolved {
		return "", fmt.Errorf("非法 status: %q", status)
	}
	return s, nil
}

// mapSeverity 把上游 severity label 映射为 domain.Severity，大小写不敏感；
// 缺失或无法映射时按 defaultSeverity 兜底并标记 SeverityFallback=true（R2.5）。
func mapSeverity(raw string, defaultSeverity domain.Severity) (domain.Severity, bool) {
	s := domain.Severity(strings.ToLower(strings.TrimSpace(raw)))
	if s.IsValid() {
		return s, false
	}
	return defaultSeverity, true
}

// titleFromAnnotations 按 summary -> description -> ruleID 的优先级取告警标题。
func titleFromAnnotations(annotations map[string]string, ruleID string) string {
	if v := annotations["summary"]; v != "" {
		return v
	}
	if v := annotations["description"]; v != "" {
		return v
	}
	return ruleID
}

// resourceKeyPriority 把常见 Prometheus/Kubernetes label key 映射到 OTel semantic
// conventions 键名（R2.4：resource_ref 采用 service.name/host.name/
// deployment.environment 等键名）。同一 OTel 键可能有多个候选 label，按声明顺序
// 优先取值（如同时出现 service 与 job 时，service 优先映射到 service.name）。
// 只覆盖需求明确列出及若干常见扩展，未命中的原始 label 不会出现在 resource_ref
// 里——labels 字段本身仍按上游原样保留，resource_ref 是从中派生的独立视图。
var resourceKeyPriority = []struct {
	otelKey   string
	labelKeys []string
}{
	{"service.name", []string{"service", "service_name", "job"}},
	{"host.name", []string{"host", "hostname", "node"}},
	{"service.instance.id", []string{"instance"}},
	{"deployment.environment", []string{"environment", "env"}},
	{"k8s.namespace.name", []string{"namespace"}},
	{"k8s.pod.name", []string{"pod"}},
	{"k8s.container.name", []string{"container"}},
	{"k8s.cluster.name", []string{"cluster"}},
}

func normalizeResourceRef(labels map[string]string) map[string]string {
	ref := make(map[string]string)
	for _, entry := range resourceKeyPriority {
		for _, labelKey := range entry.labelKeys {
			if v := labels[labelKey]; v != "" {
				ref[entry.otelKey] = v
				break
			}
		}
	}
	if len(ref) == 0 {
		return nil
	}
	return ref
}
