package domain

import "time"

type RunbookRiskLevel string

const (
	RunbookRiskReadonly     RunbookRiskLevel = "readonly"
	RunbookRiskCoordination RunbookRiskLevel = "coordination"
	RunbookRiskMutating     RunbookRiskLevel = "mutating"
)

type RunbookDefinition struct {
	ActionKey        string           `json:"action_key"`
	Name             string           `json:"name"`
	Description      string           `json:"description"`
	RiskLevel        RunbookRiskLevel `json:"risk_level"`
	Enabled          bool             `json:"enabled"`
	UATOnly          bool             `json:"uat_only"`
	RuntimeAvailable bool             `json:"runtime_available"`
	RuntimeReason    string           `json:"runtime_reason,omitempty"`
}

type RunbookRunMode string

const (
	RunbookModeCoordination RunbookRunMode = "coordination"
	RunbookModeReadonly     RunbookRunMode = "readonly"
	RunbookModeDryRun       RunbookRunMode = "dry_run"
	RunbookModeApply        RunbookRunMode = "apply"
)

type RunbookRunStatus string

const (
	RunbookRunRunning   RunbookRunStatus = "running"
	RunbookRunSucceeded RunbookRunStatus = "succeeded"
	RunbookRunFailed    RunbookRunStatus = "failed"
	RunbookRunRejected  RunbookRunStatus = "rejected"
)

type RunbookRun struct {
	ID              string           `json:"id"`
	IncidentID      string           `json:"incident_id"`
	ActionKey       string           `json:"action_key"`
	Mode            RunbookRunMode   `json:"mode"`
	Status          RunbookRunStatus `json:"status"`
	Parameters      map[string]any   `json:"parameters"`
	Plan            map[string]any   `json:"plan"`
	PlanSHA256      string           `json:"plan_sha256,omitempty"`
	PlanExpiresAt   time.Time        `json:"plan_expires_at,omitempty"`
	SourcePlanRunID string           `json:"source_plan_run_id,omitempty"`
	RequestedBy     string           `json:"requested_by"`
	ExecutedBy      string           `json:"executed_by,omitempty"`
	RequestedAt     time.Time        `json:"requested_at"`
	StartedAt       time.Time        `json:"started_at,omitempty"`
	CompletedAt     time.Time        `json:"completed_at,omitempty"`
	Result          map[string]any   `json:"result"`
	Error           string           `json:"error,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

type RunbookApprovalDecision string

const (
	RunbookApprovalApproved RunbookApprovalDecision = "approved"
	RunbookApprovalRejected RunbookApprovalDecision = "rejected"
)

type RunbookApproval struct {
	ID         string                  `json:"id"`
	PlanRunID  string                  `json:"plan_run_id"`
	PlanSHA256 string                  `json:"plan_sha256"`
	Decision   RunbookApprovalDecision `json:"decision"`
	ApprovedBy string                  `json:"approved_by"`
	Comment    string                  `json:"comment"`
	ApprovedAt time.Time               `json:"approved_at"`
}
