package domain

import (
	"encoding/json"
	"time"
)

// Severity 表示告警等级（R2.1）。
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// IsValid 报告 s 是否为已定义的等级取值。
func (s Severity) IsValid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	default:
		return false
	}
}

// Status 表示告警生命周期状态（R2.1/R4.1）。
// 上游 adapter 产出的 AlertEvent 只会携带 StatusFiring 或 StatusResolved；
// StatusAcknowledged 是本平台在 Ack 动作后才产生的内部状态，只出现在 Alert 实体上。
type Status string

const (
	StatusFiring       Status = "firing"
	StatusAcknowledged Status = "acknowledged"
	StatusResolved     Status = "resolved"
)

// IsValid 报告 s 是否为已定义的状态取值。
func (s Status) IsValid() bool {
	switch s {
	case StatusFiring, StatusAcknowledged, StatusResolved:
		return true
	default:
		return false
	}
}

// SourceState 是上游 Alertmanager 对 occurrence 生命周期的权威判断。它与本地响应
// 动作完全正交：值班人 Ack 不能把 firing 改成 acknowledged，来源暂时不可判定时也
// 不能伪造 resolved。
type SourceState string

const (
	SourceStateFiring   SourceState = "firing"
	SourceStateResolved SourceState = "resolved"
	SourceStateUnknown  SourceState = "unknown"
)

func (s SourceState) IsValid() bool {
	switch s {
	case SourceStateFiring, SourceStateResolved, SourceStateUnknown:
		return true
	default:
		return false
	}
}

// ResponseState 是 SRE Control 自己拥有的响应状态，只能由用户/API 写入。
type ResponseState string

const (
	ResponseStateUnacknowledged ResponseState = "unacknowledged"
	ResponseStateAcknowledged   ResponseState = "acknowledged"
)

func (s ResponseState) IsValid() bool {
	return s == ResponseStateUnacknowledged || s == ResponseStateAcknowledged
}

// SuppressionState 镜像 Alertmanager 的通知抑制事实。一个 Alert 同时命中 silence
// 与 inhibition 时，主状态选择 silenced，并在 SuppressionDetails 中保留两组 ID。
type SuppressionState string

const (
	SuppressionStateNone      SuppressionState = "none"
	SuppressionStateSilenced  SuppressionState = "silenced"
	SuppressionStateInhibited SuppressionState = "inhibited"
)

func (s SuppressionState) IsValid() bool {
	switch s {
	case SuppressionStateNone, SuppressionStateSilenced, SuppressionStateInhibited:
		return true
	default:
		return false
	}
}

// ResolvePolicy 表示告警的恢复策略（D4，信封级必备字段）。
type ResolvePolicy string

const (
	// ResolvePolicySource 依赖上游发来的 resolved 事件自动恢复（R4.3）。
	ResolvePolicySource ResolvePolicy = "source"
	// ResolvePolicyTTL 在 TTLSeconds 内未收到同 fingerprint 的新事件则自动恢复（R4.4）。
	ResolvePolicyTTL ResolvePolicy = "ttl"
	// ResolvePolicyManual 只能由值班人手动 Resolve。
	ResolvePolicyManual ResolvePolicy = "manual"
)

// IsValid 报告 p 是否为已定义的恢复策略取值。
func (p ResolvePolicy) IsValid() bool {
	switch p {
	case ResolvePolicySource, ResolvePolicyTTL, ResolvePolicyManual:
		return true
	default:
		return false
	}
}

// SignalKind 表示产生告警的信号类型，决定 fingerprint 计算策略（D5）。
type SignalKind string

const (
	SignalKindMetric SignalKind = "metric"
	SignalKindLog    SignalKind = "log"
	SignalKindTrace  SignalKind = "trace"
)

// IsValid 报告 k 是否为已定义的信号类型取值。
func (k SignalKind) IsValid() bool {
	switch k {
	case SignalKindMetric, SignalKindLog, SignalKindTrace:
		return true
	default:
		return false
	}
}

// AlertEvent 是 adapter 输出的统一告警信封（R2.1），是 adapter 与核心管道之间唯一的契约。
// 核心管道（去重/路由/通知/状态机）的逻辑只依赖信封字段，不读取 SignalContext（R2.3）。
type AlertEvent struct {
	Fingerprint string // 由 Fingerprint() 计算得出（D5），不由上游提供
	// UpstreamFingerprint 是 Alertmanager API/webhook 给出的稳定 alert 指纹。
	// Alertmanager occurrence 的幂等键是 integration + 此字段 + StartsAt。
	UpstreamFingerprint string

	RuleID     string     // 规则/告警名标识，参与 fingerprint 计算（D5）
	SignalKind SignalKind // 决定 fingerprint 计算策略（D5）

	Severity         Severity
	SeverityFallback bool // 上游缺失 severity 或无法映射，按 adapter 默认等级兜底（R2.5）

	Status Status // 上游只会给出 firing/resolved（R2.1）

	Title  string
	Labels map[string]string // 归一化后的标签集

	Source      string            // 来源标识，如 integration kind/name
	ResourceRef map[string]string // OTel semantic conventions 规范化后的资源标识（D3/R2.4）

	ResolvePolicy ResolvePolicy
	TTLSeconds    int // 仅 ResolvePolicy == ResolvePolicyTTL 时有意义（R4.4）

	// SignalContext 是来源特有的类型化载荷（metric 表达式/log 样本行/trace_id 等），
	// 仅用于展示与跳转，对核心管道不透明（R2.2/R2.3）。
	SignalContext json.RawMessage

	StartsAt time.Time
	EndsAt   time.Time // 上游 resolved 事件携带的结束时间；未恢复时为零值

	// SourceUpdatedAt 用于拒绝乱序上游事件。webhook 没有 updatedAt 时，firing 用
	// StartsAt、resolved 用 EndsAt；API 对账使用 gettableAlert.updatedAt。
	SourceUpdatedAt    time.Time
	SuppressionState   SuppressionState
	SuppressionDetails map[string][]string
}

// Alert 是去重后持久化的告警实体，对应 alerts 表，一条记录代表一轮 firing 周期（R3）。
// 字段与 migrations/000001_core.up.sql 的 alerts 表对齐。
type Alert struct {
	ID string

	Fingerprint         string
	UpstreamFingerprint string
	Status              Status
	SourceState         SourceState
	ResponseState       ResponseState
	SuppressionState    SuppressionState
	SuppressionDetails  map[string][]string
	Severity            Severity
	SeverityFallback    bool

	Title  string
	Source string

	Labels        map[string]string
	ResourceRef   map[string]string
	SignalContext json.RawMessage

	ResolvePolicy ResolvePolicy
	TTLSeconds    int // 仅 ResolvePolicy == ResolvePolicyTTL 时有意义

	SourceIntegrationID    string // 对应 source_integration_id，可为空字符串（无来源集成）
	SourceUpdatedAt        time.Time
	LastReconciledAt       time.Time
	ReconcileGeneration    int64
	ReconcileMissCount     int
	SourceResolutionReason string

	DedupCount int    // 同 fingerprint 合并计数（R3.3）
	GroupKey   string // 分组通知用（R9），未分组时为空
	TeamID     string // 路由到的团队，未路由时为空
	RouteID    string // 路由到的规则 ID（aiops-foundation R3，供风暴熔断按路由计数），未路由时为空

	SilencedBy []string // 命中的 silence 规则 ID 列表（R10）

	// FlapCount/FlapWindowStart/Flapping 是 flapping 防抖状态（aiops-foundation R4）：
	// 窗口内状态翻转计数、窗口起点、是否已判定为抖动中；均只影响通知层，不影响
	// 状态机/入库/dedup/时间线（AC4.3）。未开启防抖或从未翻转过时保持零值。
	FlapCount       int
	FlapWindowStart time.Time
	Flapping        bool

	AckedBy string    // 响应人，未 Ack 时为空
	AckedAt time.Time // 未 Ack 时为零值

	StartsAt   time.Time
	LastSeenAt time.Time // 最近一次同 fingerprint 事件到达时间（R3.3）
	EndsAt     time.Time // 未恢复时为零值

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsAuthoritativeSource 表示该行是 Alertmanager occurrence 镜像，而不是兼容来源的
// 本地生命周期 Alert。判断依赖持久化身份字段，不只看可展示的 Source 字符串。
func (a Alert) IsAuthoritativeSource() bool {
	return a.SourceIntegrationID != "" && a.UpstreamFingerprint != ""
}

// 权威来源 integration kind 常量（对应 integrations.kind）。新增权威来源（如
// Grafana）只需在 AuthoritativeSourceKinds 中追加，避免各处平铺 "alertmanager"
// 字面量分支（不变量 14/24）：Alertmanager 是 Alert 生命周期唯一权威，Grafana 作为
// 第二条权威源独立接入，两者共享"跳过本地 route/group/silence/inhibit"的语义。
const (
	SourceKindAlertmanager = "alertmanager"
	SourceKindGrafana      = "grafana"
)

// AuthoritativeSourceKinds 是当前已接入的权威来源 kind 集合。
var AuthoritativeSourceKinds = []string{SourceKindAlertmanager, SourceKindGrafana}

// IsAuthoritativeSourceKind 判断给定 integration kind 是否属于权威来源。仅表达
// "来源种类"这一维度，不替代 Alert.IsAuthoritativeSource 依赖持久化身份字段的判断。
func IsAuthoritativeSourceKind(kind string) bool {
	for _, k := range AuthoritativeSourceKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ProjectLegacyStatus 在兼容期把三轴投影回旧 status，供尚未迁移的查询/页面使用。
// resolved 优先；其余情况下 Ack 只影响响应投影，不篡改 SourceState。
func ProjectLegacyStatus(source SourceState, response ResponseState) Status {
	if source == SourceStateResolved {
		return StatusResolved
	}
	if response == ResponseStateAcknowledged {
		return StatusAcknowledged
	}
	return StatusFiring
}
