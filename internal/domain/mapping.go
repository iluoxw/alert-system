package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ServiceTier string

const (
	ServiceTier0 ServiceTier = "tier-0"
	ServiceTier1 ServiceTier = "tier-1"
	ServiceTier2 ServiceTier = "tier-2"
	ServiceTier3 ServiceTier = "tier-3"
)

func (t ServiceTier) IsValid() bool {
	return t == ServiceTier0 || t == ServiceTier1 || t == ServiceTier2 || t == ServiceTier3
}

type EntityAlias struct {
	ID        string    `json:"id"`
	Namespace string    `json:"namespace"`
	Value     string    `json:"value"`
	EntityID  string    `json:"entity_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ServiceRegistration struct {
	ID            string           `json:"id"`
	Entity        ResourceNode     `json:"entity"`
	OwnerTeamID   string           `json:"owner_team_id"`
	OwnerTeamName string           `json:"owner_team_name,omitempty"`
	Description   string           `json:"description"`
	Tier          ServiceTier      `json:"tier"`
	DashboardURL  string           `json:"dashboard_url"`
	RunbookURL    string           `json:"runbook_url"`
	Enabled       bool             `json:"enabled"`
	Aliases       []EntityAlias    `json:"aliases"`
	Dependencies  []DependencyEdge `json:"dependencies"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type AlertMappingPolicy struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	Priority            int          `json:"priority"`
	SourceIntegrationID string       `json:"source_integration_id,omitempty"`
	Matchers            MatcherSet   `json:"matchers"`
	TargetEntityID      string       `json:"target_entity_id,omitempty"`
	AliasNamespace      string       `json:"alias_namespace,omitempty"`
	AliasLabel          string       `json:"alias_label,omitempty"`
	FaultKind           string       `json:"fault_kind"`
	FaultScope          FaultScope   `json:"fault_scope"`
	EvidenceRole        EvidenceRole `json:"evidence_role"`
	CorrelationLabels   []string     `json:"correlation_labels"`
	Confidence          float64      `json:"confidence"`
	Enabled             bool         `json:"enabled"`
	Version             int          `json:"version"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
}

var ErrInvalidAlertMappingPolicy = errors.New("domain: 非法 Alert Mapping 策略")

func (p AlertMappingPolicy) Validate() error {
	if strings.TrimSpace(p.Name) == "" || p.Priority < 0 || len(p.Matchers) == 0 ||
		strings.TrimSpace(p.FaultKind) == "" || !p.FaultScope.IsValid() ||
		!p.EvidenceRole.IsValid() || p.Confidence < 0 || p.Confidence > 1 {
		return ErrInvalidAlertMappingPolicy
	}
	if err := p.Matchers.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAlertMappingPolicy, err)
	}
	if p.TargetEntityID == "" && (strings.TrimSpace(p.AliasNamespace) == "" || strings.TrimSpace(p.AliasLabel) == "") {
		return fmt.Errorf("%w: 必须指定目标实体或 alias namespace/label", ErrInvalidAlertMappingPolicy)
	}
	seen := map[string]struct{}{}
	for _, label := range p.CorrelationLabels {
		label = strings.TrimSpace(label)
		if label == "" {
			return fmt.Errorf("%w: correlation label 不能为空", ErrInvalidAlertMappingPolicy)
		}
		if _, ok := seen[label]; ok {
			return fmt.Errorf("%w: correlation label %q 重复", ErrInvalidAlertMappingPolicy, label)
		}
		seen[label] = struct{}{}
	}
	return nil
}

type AlertMappingStatus string

const (
	AlertMappingMapped   AlertMappingStatus = "mapped"
	AlertMappingUnmapped AlertMappingStatus = "unmapped"
	AlertMappingConflict AlertMappingStatus = "conflict"
	AlertMappingIgnored  AlertMappingStatus = "ignored"
)

func (s AlertMappingStatus) IsValid() bool {
	return s == AlertMappingMapped || s == AlertMappingUnmapped || s == AlertMappingConflict || s == AlertMappingIgnored
}

type AlertMappingResult struct {
	AlertID        string             `json:"alert_id"`
	Status         AlertMappingStatus `json:"status"`
	PolicyID       string             `json:"policy_id,omitempty"`
	PolicyName     string             `json:"policy_name,omitempty"`
	EntityID       string             `json:"entity_id,omitempty"`
	EntityKey      string             `json:"entity_key,omitempty"`
	EntityName     string             `json:"entity_name,omitempty"`
	FaultID        string             `json:"fault_id,omitempty"`
	ReasonCode     string             `json:"reason_code"`
	Explanation    string             `json:"explanation"`
	CorrelationKey string             `json:"correlation_key,omitempty"`
	EvaluatedAt    time.Time          `json:"evaluated_at"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}
