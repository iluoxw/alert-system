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
}

// Alert 是去重后持久化的告警实体，对应 alerts 表，一条记录代表一轮 firing 周期（R3）。
// 字段与 migrations/000001_core.up.sql 的 alerts 表对齐。
type Alert struct {
	ID string

	Fingerprint      string
	Status           Status
	Severity         Severity
	SeverityFallback bool

	Title  string
	Source string

	Labels        map[string]string
	ResourceRef   map[string]string
	SignalContext json.RawMessage

	ResolvePolicy ResolvePolicy
	TTLSeconds    int // 仅 ResolvePolicy == ResolvePolicyTTL 时有意义

	SourceIntegrationID string // 对应 source_integration_id，可为空字符串（无来源集成）

	DedupCount int    // 同 fingerprint 合并计数（R3.3）
	GroupKey   string // 分组通知用（R9），未分组时为空
	TeamID     string // 路由到的团队，未路由时为空

	SilencedBy []string // 命中的 silence 规则 ID 列表（R10）

	AckedBy string    // 响应人，未 Ack 时为空
	AckedAt time.Time // 未 Ack 时为零值

	StartsAt   time.Time
	LastSeenAt time.Time // 最近一次同 fingerprint 事件到达时间（R3.3）
	EndsAt     time.Time // 未恢复时为零值

	CreatedAt time.Time
	UpdatedAt time.Time
}
