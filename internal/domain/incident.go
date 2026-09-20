package domain

import (
	"errors"
	"fmt"
	"time"
)

type IncidentStatus string

const (
	IncidentStatusDeclared   IncidentStatus = "declared"
	IncidentStatusMitigating IncidentStatus = "mitigating"
	IncidentStatusMonitoring IncidentStatus = "monitoring"
	IncidentStatusResolved   IncidentStatus = "resolved"
)

func (s IncidentStatus) IsValid() bool {
	switch s {
	case IncidentStatusDeclared, IncidentStatusMitigating, IncidentStatusMonitoring, IncidentStatusResolved:
		return true
	default:
		return false
	}
}

func (s IncidentStatus) IsOpen() bool {
	return s == IncidentStatusDeclared || s == IncidentStatusMitigating || s == IncidentStatusMonitoring
}

type IncidentFaultRelation string

const (
	IncidentFaultPrimary    IncidentFaultRelation = "primary"
	IncidentFaultSupporting IncidentFaultRelation = "supporting"
)

func (r IncidentFaultRelation) IsValid() bool {
	return r == IncidentFaultPrimary || r == IncidentFaultSupporting
}

type IncidentFaultDisposition string

const (
	IncidentFaultIncluded IncidentFaultDisposition = "included"
	IncidentFaultExcluded IncidentFaultDisposition = "excluded"
)

func (d IncidentFaultDisposition) IsValid() bool {
	return d == IncidentFaultIncluded || d == IncidentFaultExcluded
}

type IncidentRole string

const (
	IncidentRoleCommander       IncidentRole = "incident_commander"
	IncidentRoleTechnicalLead   IncidentRole = "technical_lead"
	IncidentRoleCommunications  IncidentRole = "communications_lead"
	IncidentRoleBusinessLiaison IncidentRole = "business_liaison"
)

func (r IncidentRole) IsValid() bool {
	switch r {
	case IncidentRoleCommander, IncidentRoleTechnicalLead, IncidentRoleCommunications, IncidentRoleBusinessLiaison:
		return true
	default:
		return false
	}
}

type IncidentTaskStatus string

const (
	IncidentTaskOpen       IncidentTaskStatus = "open"
	IncidentTaskInProgress IncidentTaskStatus = "in_progress"
	IncidentTaskDone       IncidentTaskStatus = "done"
	IncidentTaskCanceled   IncidentTaskStatus = "canceled"
)

func (s IncidentTaskStatus) IsValid() bool {
	switch s {
	case IncidentTaskOpen, IncidentTaskInProgress, IncidentTaskDone, IncidentTaskCanceled:
		return true
	default:
		return false
	}
}

type IncidentTimelineKind string

const (
	IncidentTimelineFact           IncidentTimelineKind = "fact"
	IncidentTimelineHypothesis     IncidentTimelineKind = "hypothesis"
	IncidentTimelineDecision       IncidentTimelineKind = "decision"
	IncidentTimelineAutomation     IncidentTimelineKind = "automation"
	IncidentTimelineCommunication  IncidentTimelineKind = "communication"
	IncidentTimelineStatusChange   IncidentTimelineKind = "status_change"
	IncidentTimelineTaskUpdate     IncidentTimelineKind = "task_update"
	IncidentTimelineRoleAssignment IncidentTimelineKind = "role_assignment"
)

func (k IncidentTimelineKind) IsValid() bool {
	switch k {
	case IncidentTimelineFact, IncidentTimelineHypothesis, IncidentTimelineDecision,
		IncidentTimelineAutomation, IncidentTimelineCommunication, IncidentTimelineStatusChange,
		IncidentTimelineTaskUpdate, IncidentTimelineRoleAssignment:
		return true
	default:
		return false
	}
}

type RecoveryGate string

const (
	RecoveryGateMitigation RecoveryGate = "mitigation"
	RecoveryGateResolution RecoveryGate = "resolution"
)

type RecoveryCriterionStatus string

const (
	RecoveryCriterionPending RecoveryCriterionStatus = "pending"
	RecoveryCriterionPassed  RecoveryCriterionStatus = "passed"
	RecoveryCriterionFailed  RecoveryCriterionStatus = "failed"
)

func (s RecoveryCriterionStatus) IsValid() bool {
	return s == RecoveryCriterionPending || s == RecoveryCriterionPassed || s == RecoveryCriterionFailed
}

type Incident struct {
	ID                     string         `json:"id"`
	Number                 int64          `json:"number"`
	Status                 IncidentStatus `json:"status"`
	Severity               Severity       `json:"severity"`
	Title                  string         `json:"title"`
	Summary                string         `json:"summary"`
	DeclaredBy             string         `json:"declared_by"`
	CommunicationChannel   string         `json:"communication_channel"`
	CommunicationTargetRef string         `json:"communication_target_ref"`
	DeclaredAt             time.Time      `json:"declared_at"`
	MitigatedAt            time.Time      `json:"mitigated_at,omitempty"`
	MonitoringAt           time.Time      `json:"monitoring_at,omitempty"`
	ResolvedAt             time.Time      `json:"resolved_at,omitempty"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

func (i Incident) DisplayID() string { return fmt.Sprintf("INC-%06d", i.Number) }

type IncidentFaultLink struct {
	IncidentID           string                   `json:"incident_id"`
	FaultID              string                   `json:"fault_id"`
	Relation             IncidentFaultRelation    `json:"relation"`
	Disposition          IncidentFaultDisposition `json:"disposition"`
	DispositionRationale string                   `json:"disposition_rationale"`
	DecidedBy            string                   `json:"decided_by,omitempty"`
	DecidedAt            time.Time                `json:"decided_at,omitempty"`
	LinkedAt             time.Time                `json:"linked_at"`
}

type IncidentFaultLifecycle struct {
	FaultID     string
	Disposition IncidentFaultDisposition
	Status      FaultStatus
}

type IncidentRoleAssignment struct {
	IncidentID string       `json:"incident_id"`
	Role       IncidentRole `json:"role"`
	UserID     string       `json:"user_id"`
	AssignedBy string       `json:"assigned_by"`
	AssignedAt time.Time    `json:"assigned_at"`
}

type IncidentTask struct {
	ID          string             `json:"id"`
	IncidentID  string             `json:"incident_id"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Status      IncidentTaskStatus `json:"status"`
	OwnerUserID string             `json:"owner_user_id,omitempty"`
	CreatedBy   string             `json:"created_by"`
	DueAt       time.Time          `json:"due_at,omitempty"`
	CompletedAt time.Time          `json:"completed_at,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

type IncidentTimelineEntry struct {
	ID           string               `json:"id"`
	IncidentID   string               `json:"incident_id"`
	ActorUserID  string               `json:"actor_user_id,omitempty"`
	ActorDisplay string               `json:"actor_display"`
	Kind         IncidentTimelineKind `json:"kind"`
	Summary      string               `json:"summary"`
	Detail       map[string]any       `json:"detail"`
	DedupeKey    string               `json:"-"`
	OccurredAt   time.Time            `json:"occurred_at"`
}

type RecoveryCriterion struct {
	ID          string                  `json:"id"`
	IncidentID  string                  `json:"incident_id"`
	Key         string                  `json:"key"`
	Description string                  `json:"description"`
	Gate        RecoveryGate            `json:"gate"`
	Required    bool                    `json:"required"`
	Status      RecoveryCriterionStatus `json:"status"`
	Evidence    map[string]any          `json:"evidence"`
	EvaluatedBy string                  `json:"evaluated_by,omitempty"`
	ObservedAt  time.Time               `json:"observed_at,omitempty"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
}

type IncidentDetail struct {
	Incident Incident                 `json:"incident"`
	Faults   []IncidentFaultLink      `json:"faults"`
	Roles    []IncidentRoleAssignment `json:"roles"`
	Tasks    []IncidentTask           `json:"tasks"`
	Timeline []IncidentTimelineEntry  `json:"timeline"`
	Recovery []RecoveryCriterion      `json:"recovery_criteria"`
}

type IncidentNotificationEvent string

const (
	IncidentNotificationDeclared  IncidentNotificationEvent = "declared"
	IncidentNotificationMitigated IncidentNotificationEvent = "mitigated"
	IncidentNotificationResolved  IncidentNotificationEvent = "resolved"
	// IncidentNotificationRecoveryNudge 是 incident_recovery_nudge 定时器在判定
	// "全部有效证据告警已解除"时入队的提醒事件。故意不叫 xxxRecovered/xxxRepaired：
	// 证据告警解除（Alertmanager resolved）可能只是采集目标下线或服务发现注销
	// 导致的假解除，不代表故障真的恢复，文案与命名都不能替事故指挥下这个结论
	// （2026-09 线上 E2BNomadAgentDown 假解除误报教训）。
	IncidentNotificationRecoveryNudge IncidentNotificationEvent = "recovery_nudge"
)

type IncidentNotification struct {
	Incident   Incident                  `json:"incident"`
	Event      IncidentNotificationEvent `json:"event"`
	OccurredAt time.Time                 `json:"occurred_at"`
	// RecoveryNudge 仅在 Event == IncidentNotificationRecoveryNudge 时非空，携带
	// 渲染提醒文案需要的证据与门槛快照。
	RecoveryNudge *IncidentRecoveryNudgeDetail `json:"recovery_nudge,omitempty"`
}

// IncidentRecoveryNudgeEvidence 是一条已解除的证据告警，供提醒文案列出 alertname
// 与解除时间，让事故指挥能判断这次"解除"是否可信（而不是无条件当作已恢复）。
type IncidentRecoveryNudgeEvidence struct {
	AlertName string    `json:"alert_name"`
	ClearedAt time.Time `json:"cleared_at"`
}

// IncidentRecoveryNudgeDetail 是恢复提醒的判定快照：全部有效证据（排除人工判为
// excluded 的证据）已解除的时间点，以及当前离下一道关闭门槛还差什么。
type IncidentRecoveryNudgeDetail struct {
	ClearedEvidence          []IncidentRecoveryNudgeEvidence `json:"cleared_evidence"`
	LatestClearedAt          time.Time                       `json:"latest_cleared_at"`
	OpenTaskCount            int                             `json:"open_task_count"`
	ActiveIncludedFaultCount int                             `json:"active_included_fault_count"`
	UnmetRequiredCriteria    []string                        `json:"unmet_required_criteria"`
	// AllEvidenceEntitiesDecommissioned is copy-only: every locatable evidence
	// entity on included Faults is decommissioned. Does not change trigger
	// conditions or advance Incident status.
	AllEvidenceEntitiesDecommissioned bool `json:"all_evidence_entities_decommissioned"`
}

var (
	ErrInvalidIncidentTransition = errors.New("domain: 非法 Incident 状态转移")
	ErrRecoveryGateNotSatisfied  = errors.New("domain: 恢复判据尚未满足")
	ErrIncidentTasksNotClosed    = errors.New("domain: 仍有未收口的 Incident 任务")
	ErrIncidentFaultsNotReady    = errors.New("domain: 仍有关联 active Fault 未缓解或排除")
)

func ValidateIncidentFaultsForResolution(faults []IncidentFaultLifecycle) error {
	for _, fault := range faults {
		if fault.Disposition == IncidentFaultIncluded && fault.Status == FaultStatusActive {
			return fmt.Errorf("%w: %s", ErrIncidentFaultsNotReady, fault.FaultID)
		}
	}
	return nil
}

// ValidateIncidentTransition 只允许向前推进，并把 monitoring/resolved 与结构化
// 恢复证据和处置任务绑定。Incident 不能靠任意 PATCH 状态绕过恢复门槛，也不能
// 在仍有 open/in_progress 响应任务时营造“已解决”的假象。
func ValidateIncidentTransition(from, to IncidentStatus, criteria []RecoveryCriterion, tasks []IncidentTask) error {
	validEdge := false
	switch {
	case (from == IncidentStatusDeclared || from == IncidentStatusMitigating) && to == IncidentStatusMitigating:
		validEdge = from == IncidentStatusDeclared
	case (from == IncidentStatusDeclared || from == IncidentStatusMitigating) && to == IncidentStatusMonitoring:
		validEdge = true
	case from == IncidentStatusMonitoring && to == IncidentStatusResolved:
		validEdge = true
	}
	if !validEdge {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidIncidentTransition, from, to)
	}
	if to == IncidentStatusMonitoring && !requiredCriteriaPassed(criteria, RecoveryGateMitigation) {
		return fmt.Errorf("%w: mitigation", ErrRecoveryGateNotSatisfied)
	}
	if to == IncidentStatusResolved &&
		(!requiredCriteriaPassed(criteria, RecoveryGateMitigation) || !requiredCriteriaPassed(criteria, RecoveryGateResolution)) {
		return fmt.Errorf("%w: resolution", ErrRecoveryGateNotSatisfied)
	}
	if to == IncidentStatusResolved && !incidentTasksClosed(tasks) {
		return ErrIncidentTasksNotClosed
	}
	return nil
}

func incidentTasksClosed(tasks []IncidentTask) bool {
	for _, task := range tasks {
		if task.Status != IncidentTaskDone && task.Status != IncidentTaskCanceled {
			return false
		}
	}
	return true
}

// UnmetRequiredCriteria 返回从 status 推进到下一道关闭门槛前尚未满足的必需判据
// 描述，供 incident_recovery_nudge 提醒文案展示"还差什么"。declared/mitigating
// 只看 mitigation 判据；monitoring 要同时看 mitigation 与 resolution 判据——对齐
// ValidateIncidentTransition 对 resolved 的门槛口径（两个 gate 都必须满足）。
func UnmetRequiredCriteria(status IncidentStatus, criteria []RecoveryCriterion) []string {
	gates := []RecoveryGate{RecoveryGateMitigation}
	if status == IncidentStatusMonitoring {
		gates = append(gates, RecoveryGateResolution)
	}
	var unmet []string
	for _, gate := range gates {
		for _, c := range criteria {
			if c.Gate != gate || !c.Required || c.Status == RecoveryCriterionPassed {
				continue
			}
			unmet = append(unmet, c.Description)
		}
	}
	return unmet
}

func requiredCriteriaPassed(criteria []RecoveryCriterion, gate RecoveryGate) bool {
	foundRequired := false
	for _, criterion := range criteria {
		if criterion.Gate != gate || !criterion.Required {
			continue
		}
		foundRequired = true
		if criterion.Status != RecoveryCriterionPassed {
			return false
		}
	}
	return foundRequired
}

// DefaultRecoveryCriteria 是业务无关的最小恢复契约：先证明应用影响恢复和关联
// Fault 稳定，再经过观察窗口关闭。业务专用 Runbook 可以在此基础上提供更细判据。
func DefaultRecoveryCriteria(incidentID string) []RecoveryCriterion {
	return []RecoveryCriterion{
		{IncidentID: incidentID, Key: "application_impact_recovered", Description: "已确认的应用服务影响恢复到可接受范围", Gate: RecoveryGateMitigation, Required: true, Status: RecoveryCriterionPending},
		{IncidentID: incidentID, Key: "linked_faults_stabilized", Description: "关联 Fault 已进入稳定或恢复观察状态", Gate: RecoveryGateMitigation, Required: true, Status: RecoveryCriterionPending},
		{IncidentID: incidentID, Key: "observation_window_complete", Description: "恢复观察窗口结束且无再次退化", Gate: RecoveryGateResolution, Required: true, Status: RecoveryCriterionPending},
	}
}

// DefaultRecoveryCriteriaForFault 按 Fault 类型返回恢复判据。当前所有 Fault 共享
// 通用最小契约；业务专用判据应通过配置或后续扩展注入，不在核心代码硬编码。
func DefaultRecoveryCriteriaForFault(incidentID string, _ Fault) []RecoveryCriterion {
	return DefaultRecoveryCriteria(incidentID)
}
