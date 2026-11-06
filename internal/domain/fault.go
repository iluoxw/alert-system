package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// FaultScope 明确区分单个 Sandbox 不可恢复与共享基础设施能力失守。
type FaultScope string

const (
	FaultScopeInstance   FaultScope = "instance"
	FaultScopeCapability FaultScope = "capability"
)

func (s FaultScope) IsValid() bool {
	return s == FaultScopeInstance || s == FaultScopeCapability
}

type FaultStatus string

const (
	FaultStatusCandidate  FaultStatus = "candidate"
	FaultStatusActive     FaultStatus = "active"
	FaultStatusMonitoring FaultStatus = "monitoring"
	FaultStatusResolved   FaultStatus = "resolved"
)

func (s FaultStatus) IsValid() bool {
	switch s {
	case FaultStatusCandidate, FaultStatusActive, FaultStatusMonitoring, FaultStatusResolved:
		return true
	default:
		return false
	}
}

// EvidenceRole 是 Alert 在某个 Fault 证据图里的角色；同一 Alert 在不同 Fault 中可以
// 扮演不同角色，例如 object_missing 对实例 Fault 是 direct_impact，对能力 Fault 是
// cause_candidate。
type EvidenceRole string

const (
	EvidenceRoleDirectImpact   EvidenceRole = "direct_impact"
	EvidenceRoleCauseCandidate EvidenceRole = "cause_candidate"
	EvidenceRoleTrigger        EvidenceRole = "trigger"
	EvidenceRoleAmplifier      EvidenceRole = "amplifier"
	EvidenceRoleSymptom        EvidenceRole = "symptom"
	EvidenceRoleSupporting     EvidenceRole = "supporting"
	EvidenceRoleExcluded       EvidenceRole = "excluded"
)

func (r EvidenceRole) IsValid() bool {
	switch r {
	case EvidenceRoleDirectImpact, EvidenceRoleCauseCandidate, EvidenceRoleTrigger,
		EvidenceRoleAmplifier, EvidenceRoleSymptom, EvidenceRoleSupporting, EvidenceRoleExcluded:
		return true
	default:
		return false
	}
}

type DecisionOrigin string

const (
	DecisionOriginAutomation DecisionOrigin = "automation"
	DecisionOriginHuman      DecisionOrigin = "human"
)

type ImpactState string

const (
	ImpactStatePotential ImpactState = "potential"
	ImpactStateConfirmed ImpactState = "confirmed"
	ImpactStateRuledOut  ImpactState = "ruled_out"
)

func (s ImpactState) IsValid() bool {
	switch s {
	case ImpactStatePotential, ImpactStateConfirmed, ImpactStateRuledOut:
		return true
	default:
		return false
	}
}

type Fault struct {
	ID             string         `json:"id"`
	CorrelationKey string         `json:"correlation_key"`
	Scope          FaultScope     `json:"scope"`
	Status         FaultStatus    `json:"status"`
	Severity       Severity       `json:"severity"`
	Kind           string         `json:"kind"`
	Title          string         `json:"title"`
	Summary        string         `json:"summary"`
	ResourceNodeID string         `json:"resource_node_id,omitempty"`
	EntityRef      string         `json:"entity_ref"`
	Confidence     float64        `json:"confidence"`
	Metadata       map[string]any `json:"metadata"`
	// OwnerTeamID 只能来自 resource_nodes.owner_team_id（不变量 15），由 projector
	// 在 Upsert 时写入；本包不做任何推断，认领动作也不改变它（R3.4）。
	OwnerTeamID     string    `json:"owner_team_id,omitempty"`
	AckedBy         string    `json:"acked_by,omitempty"`
	AckedByName     string    `json:"acked_by_name,omitempty"`
	AckedAt         time.Time `json:"acked_at,omitempty"`
	OwnerNotifiedAt time.Time `json:"owner_notified_at,omitempty"`
	FirstDetected   time.Time `json:"first_detected_at"`
	LastEvaluated   time.Time `json:"last_evaluated_at"`
	ResolvedAt      time.Time `json:"resolved_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IsAcked 报告该 Fault 是否已被认领——AckedAt 非零即视为已认领，与
// alerts.acked_at 的判定口径一致。
func (f Fault) IsAcked() bool {
	return !f.AckedAt.IsZero()
}

// FaultTimelineAction 标识 fault_timeline_entries 的动作类型；当前只有 ack，
// 其余动作（通知、拓扑抑制等）留给后续任务扩展。
type FaultTimelineAction string

const (
	FaultTimelineActionAck FaultTimelineAction = "ack"
	// FaultTimelineActionNotifySuppressed 记录"上游 provider 已有已通知的 active
	// Fault，本 Fault 不单独通知"这一拓扑抑制决策（不变量 22），Detail 携带
	// upstream_fault_id 供人回溯抑制来源。
	FaultTimelineActionNotifySuppressed FaultTimelineAction = "notify_suppressed"
	// FaultTimelineActionNotifySkipped 记录"owner team 与全局 fallback 均未配置
	// 通知渠道，本轮未产生任何通知意图"，避免通知静默失败无迹可寻。
	FaultTimelineActionNotifySkipped FaultTimelineAction = "notify_skipped"
	// FaultTimelineActionEntityDecommissioned 记录「实体已下线，本 Fault 按下线收口」；
	// 这不是恢复，不得写成恢复语义。
	FaultTimelineActionEntityDecommissioned FaultTimelineAction = "entity_decommissioned"
	// FaultTimelineActionEntityRestored 记录「实体从下线恢复为 active」，不重开 Fault。
	FaultTimelineActionEntityRestored FaultTimelineAction = "entity_restored"
)

// FaultTimelineEntry 是不可篡改的 Fault 时间线条目，结构对齐
// domain.TimelineEntry（Alert）与 incident_timeline_entries（Incident）。
// 由状态机函数产出，供 store 层 append-only 落库；本包不做任何 I/O。
type FaultTimelineEntry struct {
	FaultID string
	// ActorID 对应 fault_timeline_entries.actor_id（UUID，可空，系统动作无用户）；
	// Actor 是必填的可读名称。两者分开存放，沿用不变量 7 的双参数口径。
	ActorID string
	Actor   string
	Action  FaultTimelineAction
	Detail  map[string]any
	At      time.Time
}

type FaultEvidence struct {
	FaultID        string         `json:"fault_id"`
	AlertID        string         `json:"alert_id"`
	Role           EvidenceRole   `json:"role"`
	Rationale      string         `json:"rationale"`
	Confidence     float64        `json:"confidence"`
	DecisionOrigin DecisionOrigin `json:"decision_origin"`
	DecidedBy      string         `json:"decided_by,omitempty"`
	DecidedAt      time.Time      `json:"decided_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type FaultRelationType string

const (
	FaultRelationContributesTo FaultRelationType = "contributes_to"
)

type FaultRelation struct {
	FromFaultID string
	ToFaultID   string
	Type        FaultRelationType
	Rationale   string
	Confidence  float64
	CreatedAt   time.Time
}

type FaultImpact struct {
	FaultID        string         `json:"fault_id"`
	NodeID         string         `json:"node_id"`
	State          ImpactState    `json:"state"`
	Path           []string       `json:"path"`
	Rationale      string         `json:"rationale"`
	Confidence     float64        `json:"confidence"`
	DecisionOrigin DecisionOrigin `json:"decision_origin"`
	DecidedBy      string         `json:"decided_by,omitempty"`
	DecidedAt      time.Time      `json:"decided_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// FaultTransition 标识一次 Upsert 是否让 Fault"进入 active"，供调用方（任务 17）
// 判断是否需要触发 Fault 通知（R5.1/R5.10/R5.11）。只关心进入 active 这一件事：
// 已经是 active 的重复评估、或本次结果不是 active，一律为 None，避免同一次 active
// 转移在多入口并发调用 Project 时重复触发通知意图。
type FaultTransition string

const (
	// FaultTransitionNone 表示本次 Upsert 未让 Fault 进入 active（含已经是 active 的
	// 重复评估），调用方不应触发通知。
	FaultTransitionNone FaultTransition = ""
	// FaultTransitionCreatedActive 表示 correlation_key 此前不存在，本次直接以
	// active 状态创建（R5.10 的 direct firing 场景）。
	FaultTransitionCreatedActive FaultTransition = "created_active"
	// FaultTransitionEnteredActive 表示已存在的 Fault 本次由非 active 状态进入
	// active——覆盖两种场景：candidate 升级为 active（后续证据补上了
	// direct_impact），以及 monitoring 复发重新升级为 active（观察期内故障复发，
	// 同样需要重新通知 owner，"没有告警了"只是缺乏证据不等于恢复，不变量 22）。
	// 命名不叫 CandidateActive 是为了不误导后续实现者以为只覆盖 candidate 一种来源。
	FaultTransitionEnteredActive FaultTransition = "entered_active"
)

var (
	ErrInvalidFaultTransition = errors.New("domain: 非法 Fault 状态转移")
	// ErrAckActorNameRequired 表示认领人可读名称为空。时间线不能留下一条不知道是谁做的 ack。
	ErrAckActorNameRequired = errors.New("domain: ack 的 actorName 不能为空")
)

func ValidateFaultTransition(from, to FaultStatus) error {
	if (from == FaultStatusCandidate && to == FaultStatusActive) ||
		(from == FaultStatusActive && to == FaultStatusMonitoring) ||
		(from == FaultStatusMonitoring && to == FaultStatusResolved) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidFaultTransition, from, to)
}

// AckFault 执行 Fault 认领动作（R4.2）：actorID/actorName 双参数口径与
// domain.Ack(alert, ...)（不变量 7）一致——actorID 写入 faults.acked_by（UUID
// 列，可空），actorName 写入 fault_timeline_entries.actor_name 与
// faults.acked_by_name（TEXT，必填，时间线不能有一条不知道是谁做的 ack 记录）。
//
// WHEN 该 Fault 已认领 THEN 按不变量 6 的幂等口径短路：返回入参原样与零值
// FaultTimelineEntry（ok=false），调用方据此判断不重写时间线、不返回 409——
// 409/200 是 API 层的返回码语义，本函数只产出纯数据结果。
//
// 纯函数：不做任何 I/O，不修改入参 fault，返回更新后的副本。
func AckFault(fault Fault, actorID, actorName string, now time.Time) (Fault, FaultTimelineEntry, bool, error) {
	// 纯空白与空串同样视为缺失，口径与 incident.Actor.valid 的 TrimSpace 一致。
	if strings.TrimSpace(actorName) == "" {
		return fault, FaultTimelineEntry{}, false, ErrAckActorNameRequired
	}
	if fault.IsAcked() {
		return fault, FaultTimelineEntry{}, false, nil
	}

	updated := fault
	updated.AckedBy = actorID
	updated.AckedByName = actorName
	updated.AckedAt = now
	updated.UpdatedAt = now

	entry := FaultTimelineEntry{
		FaultID: fault.ID,
		ActorID: actorID,
		Actor:   actorName,
		Action:  FaultTimelineActionAck,
		At:      now,
		Detail:  map[string]any{},
	}
	return updated, entry, true, nil
}
