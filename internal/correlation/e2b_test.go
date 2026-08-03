package correlation

import (
	"fmt"
	"testing"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/e2b"
)

func TestAssessE2BFailedToPlace_StageAStaysCandidate(t *testing.T) {
	fixture, err := e2b.LoadReplayFixture("stage_a.json")
	if err != nil {
		t.Fatal(err)
	}
	alerts := fixtureAlerts(t, fixture)
	assessment := AssessE2BFailedToPlace(alerts, fixture.ObservedAt)

	capability := findProposal(t, assessment, E2BCapabilityCorrelationKey)
	if capability.Fault.Status != domain.FaultStatusCandidate {
		t.Fatalf("stage A capability status=%s, want candidate", capability.Fault.Status)
	}
	if assessment.IncidentSuggested {
		t.Fatalf("create SLO 健康时不应建议 Incident")
	}
	if got := countScope(assessment, domain.FaultScopeInstance); got != 2 {
		t.Fatalf("实例 Fault=%d, want 2", got)
	}
	if !hasRole(capability.Evidence, domain.EvidenceRoleExcluded) {
		t.Fatalf("F5 stall 应作为 excluded 证据保留")
	}
	if hasRole(capability.Evidence, domain.EvidenceRoleDirectImpact) {
		t.Fatalf("stage A 不应出现 direct impact")
	}
}

func TestAssessE2BFailedToPlace_StageBActivatesOnUserSLO(t *testing.T) {
	fixture, err := e2b.LoadReplayFixture("stage_b.json")
	if err != nil {
		t.Fatal(err)
	}
	assessment := AssessE2BFailedToPlace(fixtureAlerts(t, fixture), fixture.ObservedAt)
	capability := findProposal(t, assessment, E2BCapabilityCorrelationKey)
	if capability.Fault.Status != domain.FaultStatusActive || !assessment.IncidentSuggested {
		t.Fatalf("stage B 应为 active + Incident suggested: %+v", assessment)
	}
	if capability.Fault.EntityRef != "capability:"+E2BCapabilityResourceRef {
		t.Fatalf("能力 Fault 缺少稳定 entity_ref: %q", capability.Fault.EntityRef)
	}
	if !hasRole(capability.Evidence, domain.EvidenceRoleDirectImpact) {
		t.Fatalf("stage B 缺少 direct impact")
	}
	if len(capability.ConfirmedImpactRefs) != 1 || capability.ConfirmedImpactRefs[0] != "insforge.preview-runtime" {
		t.Fatalf("业务确认影响错误: %+v", capability.ConfirmedImpactRefs)
	}
}

func TestAssessE2BFailedToPlace_AttemptCountCannotActivateFault(t *testing.T) {
	fixture, err := e2b.LoadReplayFixture("stage_a.json")
	if err != nil {
		t.Fatal(err)
	}
	alerts := fixtureAlerts(t, fixture)
	for i := 0; i < 20; i++ {
		duplicate := alerts[2]
		duplicate.ID = fmt.Sprintf("duplicate-%d", i)
		alerts = append(alerts, duplicate)
	}
	assessment := AssessE2BFailedToPlace(alerts, fixture.ObservedAt.Add(time.Minute))
	capability := findProposal(t, assessment, E2BCapabilityCorrelationKey)
	if capability.Fault.Status != domain.FaultStatusCandidate {
		t.Fatalf("重复 attempt 告警不能把能力 Fault 激活, got %s", capability.Fault.Status)
	}
}

func fixtureAlerts(t *testing.T, fixture e2b.ReplayFixture) []domain.Alert {
	t.Helper()
	events, err := fixture.AlertEvents()
	if err != nil {
		t.Fatal(err)
	}
	alerts := make([]domain.Alert, 0, len(events))
	for i, event := range events {
		alerts = append(alerts, domain.Alert{
			ID: fmt.Sprintf("alert-%d", i), Fingerprint: event.Fingerprint, Status: event.Status,
			Severity: event.Severity, Title: event.Title, Source: event.Source,
			Labels: event.Labels, ResourceRef: event.ResourceRef, SignalContext: event.SignalContext,
			ResolvePolicy: event.ResolvePolicy, StartsAt: event.StartsAt, LastSeenAt: event.StartsAt,
		})
	}
	return alerts
}

func findProposal(t *testing.T, assessment E2BAssessment, key string) FaultProposal {
	t.Helper()
	for _, proposal := range assessment.Faults {
		if proposal.Fault.CorrelationKey == key {
			return proposal
		}
	}
	t.Fatalf("未找到 proposal %s", key)
	return FaultProposal{}
}

func countScope(assessment E2BAssessment, scope domain.FaultScope) int {
	count := 0
	for _, proposal := range assessment.Faults {
		if proposal.Fault.Scope == scope {
			count++
		}
	}
	return count
}

func hasRole(evidence []domain.FaultEvidence, role domain.EvidenceRole) bool {
	for _, item := range evidence {
		if item.Role == role {
			return true
		}
	}
	return false
}
