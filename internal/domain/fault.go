package domain

import (
	"errors"
	"fmt"
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
	FirstDetected  time.Time      `json:"first_detected_at"`
	LastEvaluated  time.Time      `json:"last_evaluated_at"`
	ResolvedAt     time.Time      `json:"resolved_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
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

var ErrInvalidFaultTransition = errors.New("domain: 非法 Fault 状态转移")

func ValidateFaultTransition(from, to FaultStatus) error {
	if (from == FaultStatusCandidate && to == FaultStatusActive) ||
		(from == FaultStatusActive && to == FaultStatusMonitoring) ||
		(from == FaultStatusMonitoring && to == FaultStatusResolved) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidFaultTransition, from, to)
}
