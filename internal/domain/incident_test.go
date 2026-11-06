package domain

import (
	"errors"
	"testing"
)

func TestValidateIncidentTransitionRequiresRecoveryGates(t *testing.T) {
	criteria := DefaultRecoveryCriteria("incident-1")
	if err := ValidateIncidentTransition(IncidentStatusDeclared, IncidentStatusMonitoring, criteria, nil); !errors.Is(err, ErrRecoveryGateNotSatisfied) {
		t.Fatalf("未满足 mitigation gate 应拒绝 monitoring, got %v", err)
	}
	for i := range criteria {
		if criteria[i].Gate == RecoveryGateMitigation {
			criteria[i].Status = RecoveryCriterionPassed
		}
	}
	if err := ValidateIncidentTransition(IncidentStatusDeclared, IncidentStatusMonitoring, criteria, nil); err != nil {
		t.Fatalf("mitigation gate 已满足应允许 monitoring: %v", err)
	}
	if err := ValidateIncidentTransition(IncidentStatusMonitoring, IncidentStatusResolved, criteria, nil); !errors.Is(err, ErrRecoveryGateNotSatisfied) {
		t.Fatalf("观察窗口未通过应拒绝 resolved, got %v", err)
	}
	for i := range criteria {
		criteria[i].Status = RecoveryCriterionPassed
	}
	if err := ValidateIncidentTransition(IncidentStatusMonitoring, IncidentStatusResolved, criteria, nil); err != nil {
		t.Fatalf("全部 gate 已满足应允许 resolved: %v", err)
	}
}

func TestValidateIncidentTransitionRequiresResponseTasksClosed(t *testing.T) {
	criteria := DefaultRecoveryCriteria("incident-1")
	for i := range criteria {
		criteria[i].Status = RecoveryCriterionPassed
	}
	tasks := []IncidentTask{{Status: IncidentTaskOpen}, {Status: IncidentTaskDone}}
	if err := ValidateIncidentTransition(IncidentStatusMonitoring, IncidentStatusResolved, criteria, tasks); !errors.Is(err, ErrIncidentTasksNotClosed) {
		t.Fatalf("仍有 open 任务应拒绝 resolved, got %v", err)
	}
	tasks[0].Status = IncidentTaskCanceled
	if err := ValidateIncidentTransition(IncidentStatusMonitoring, IncidentStatusResolved, criteria, tasks); err != nil {
		t.Fatalf("任务均 done/canceled 应允许 resolved: %v", err)
	}
}

func TestValidateIncidentTransitionRejectsBackwardMove(t *testing.T) {
	criteria := DefaultRecoveryCriteria("incident-1")
	if err := ValidateIncidentTransition(IncidentStatusMonitoring, IncidentStatusMitigating, criteria, nil); !errors.Is(err, ErrInvalidIncidentTransition) {
		t.Fatalf("状态回退应被拒绝, got %v", err)
	}
}

func TestDefaultRecoveryCriteriaForFaultUsesGenericProfile(t *testing.T) {
	t.Parallel()

	generic := DefaultRecoveryCriteriaForFault("incident-generic", Fault{Kind: "database_connection_exhausted"})
	if len(generic) != 3 || generic[0].Key != "application_impact_recovered" || generic[2].Key != "observation_window_complete" {
		t.Fatalf("通用 Fault 应使用通用恢复判据，got %+v", generic)
	}

	placement := DefaultRecoveryCriteriaForFault("incident-placement", Fault{Kind: "sandbox_placement_degradation"})
	if len(placement) != 3 || placement[0].Key != "application_impact_recovered" {
		t.Fatalf("专用 Fault kind 也不应再注入业务硬编码判据，got %+v", placement)
	}
}

func TestValidateIncidentFaultsForResolution(t *testing.T) {
	t.Parallel()

	faults := []IncidentFaultLifecycle{
		{FaultID: "primary", Disposition: IncidentFaultIncluded, Status: FaultStatusMonitoring},
		{FaultID: "late-active", Disposition: IncidentFaultIncluded, Status: FaultStatusActive},
		{FaultID: "independent", Disposition: IncidentFaultExcluded, Status: FaultStatusActive},
	}
	if err := ValidateIncidentFaultsForResolution(faults); !errors.Is(err, ErrIncidentFaultsNotReady) {
		t.Fatalf("included active Fault 应阻止关闭, got %v", err)
	}

	faults[1].Status = FaultStatusMonitoring
	if err := ValidateIncidentFaultsForResolution(faults); err != nil {
		t.Fatalf("included active Fault 全部缓解后应允许关闭, got %v", err)
	}
}
