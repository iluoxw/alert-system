package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/correlation"
	"alert-system/internal/domain"
	e2bfixture "alert-system/internal/e2b"
	"alert-system/internal/runbook"
	"alert-system/internal/store"
)

// TestScenarioE2BFailedToPlaceIncidentLifecycle 是黄金场景的纵向切片：真实 HTTP
// ingest -> Alert -> Fault/证据/爆炸半径 -> 人工宣布 Incident -> 角色/任务/统一时间线
// -> 受控 Runbook -> 结构化恢复门槛 -> 三次可靠状态通报。
func TestScenarioE2BFailedToPlaceIncidentLifecycle(t *testing.T) {
	ctx := context.Background()
	client := newLoggedInClient(t)
	topologyRepo := sys.Topology
	faultRepo := store.NewFaultRepo()
	evaluator := correlation.NewEvaluator(topologyRepo, faultRepo)

	topology, err := e2bfixture.LoadTopologyFixture()
	if err != nil {
		t.Fatal(err)
	}
	var nodeIDs map[string]string
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		result, err := topologyRepo.ImportSnapshot(ctx, tx, topology)
		if err != nil {
			return err
		}
		nodeIDs = result.NodeIDs
		return nil
	}); err != nil {
		t.Fatalf("导入 E2B 拓扑失败: %v", err)
	}

	stageA, err := e2bfixture.LoadReplayFixture("stage_a.json")
	if err != nil {
		t.Fatal(err)
	}
	postReplayFixture(t, client, stageA)
	alertsA := waitScenarioAlerts(t, stageA.ScenarioID, len(stageA.Signals))
	assessmentA := correlation.AssessE2BFailedToPlace(alertsA, stageA.ObservedAt)
	if assessmentA.IncidentSuggested {
		t.Fatalf("stage A create SLO 健康，不应建议 Incident")
	}
	var capabilityA domain.Fault
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		stored, err := evaluator.Persist(ctx, tx, topology.Source, assessmentA)
		if err != nil {
			return err
		}
		capabilityA = findStoredCapability(stored)
		if capabilityA.Status != domain.FaultStatusCandidate {
			return fmt.Errorf("stage A status=%s want=candidate", capabilityA.Status)
		}
		impacts, err := faultRepo.ListImpacts(ctx, tx, capabilityA.ID)
		if err != nil {
			return err
		}
		if len(impacts) != 2 {
			return fmt.Errorf("stage A impacts=%d want=2: %+v", len(impacts), impacts)
		}
		for _, impact := range impacts {
			if impact.State != domain.ImpactStatePotential {
				return fmt.Errorf("stage A 影响只能是 potential: %+v", impact)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := sys.Incidents.GetByFaultID(ctx, tx, capabilityA.ID)
		if !errors.Is(err, store.ErrIncidentNotFound) {
			return fmt.Errorf("stage A 不应产生 Incident: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stageB, err := e2bfixture.LoadReplayFixture("stage_b.json")
	if err != nil {
		t.Fatal(err)
	}
	postReplayFixture(t, client, stageB)
	// stage B 与 stage A 共享的信号按 fingerprint dedup；新增四类告警，stage A 的
	// load pressure 仍保持 firing，因此总活跃 Alert 至少为 10。
	alertsB := waitScenarioAlerts(t, stageB.ScenarioID, 10)
	assessmentB := correlation.AssessE2BFailedToPlace(alertsB, stageB.ObservedAt)
	if !assessmentB.IncidentSuggested {
		t.Fatalf("用户级 SLO 与生命周期探针失败后应建议 Incident")
	}
	var capabilityB domain.Fault
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		stored, err := evaluator.Persist(ctx, tx, topology.Source, assessmentB)
		if err != nil {
			return err
		}
		capabilityB = findStoredCapability(stored)
		if capabilityB.ID != capabilityA.ID || capabilityB.Status != domain.FaultStatusActive {
			return fmt.Errorf("能力 Fault 应原位 candidate→active: A=%+v B=%+v", capabilityA, capabilityB)
		}
		evidence, err := faultRepo.ListEvidence(ctx, tx, capabilityB.ID)
		if err != nil {
			return err
		}
		if !evidenceHasRole(evidence, domain.EvidenceRoleDirectImpact) || !evidenceHasRole(evidence, domain.EvidenceRoleExcluded) {
			return fmt.Errorf("stage B 证据图缺少 direct_impact/excluded: %+v", evidence)
		}
		impacts, err := faultRepo.ListImpacts(ctx, tx, capabilityB.ID)
		if err != nil {
			return err
		}
		states := map[string]domain.ImpactState{}
		for _, impact := range impacts {
			states[impact.NodeID] = impact.State
		}
		if states[nodeIDs["e2b.sandbox-runtime"]] != domain.ImpactStateConfirmed ||
			states[nodeIDs["insforge.preview-runtime"]] != domain.ImpactStateConfirmed {
			return fmt.Errorf("平台能力和消费业务均应为 confirmed impact: %+v", states)
		}
		var contributes int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM fault_relations
			WHERE to_fault_id = $1 AND relation = 'contributes_to'
		`, capabilityB.ID).Scan(&contributes); err != nil {
			return err
		}
		if contributes != stageB.Expectations.InstanceFaultCount {
			return fmt.Errorf("contributes_to=%d want=%d", contributes, stageB.Expectations.InstanceFaultCount)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	incident := declareE2BIncident(t, client, capabilityB.ID, true)
	duplicate := declareE2BIncident(t, client, capabilityB.ID, false)
	if duplicate.ID != incident.ID {
		t.Fatalf("重复宣布应返回相同 Incident: first=%s duplicate=%s", incident.ID, duplicate.ID)
	}
	var supportingFaultID string
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT from_fault_id::text FROM fault_relations
			WHERE to_fault_id = $1 AND relation = 'contributes_to'
			ORDER BY created_at LIMIT 1
		`, capabilityB.ID).Scan(&supportingFaultID)
	}); err != nil {
		t.Fatalf("查询 Supporting Fault 失败: %v", err)
	}
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/faults",
		map[string]any{"fault_id": supportingFaultID}, http.StatusCreated)
	// 重复关联同一 Incident 幂等，不能重复写关系或时间线。
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/faults",
		map[string]any{"fault_id": supportingFaultID}, http.StatusOK)
	// Primary Fault 必须先切换才能排除，避免 Incident 失去主诊断对象。
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/incidents/"+incident.ID+"/faults/"+capabilityB.ID+"/disposition",
		map[string]any{"disposition": domain.IncidentFaultExcluded, "rationale": "验证 primary 排除护栏"}, http.StatusConflict)
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/incidents/"+incident.ID+"/faults/"+supportingFaultID+"/primary", nil, http.StatusOK)
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/incidents/"+incident.ID+"/faults/"+capabilityB.ID+"/disposition",
		map[string]any{"disposition": domain.IncidentFaultExcluded, "rationale": "先验证独立实例链可被明确排除且关系仍保留"}, http.StatusOK)
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/incidents/"+incident.ID+"/faults/"+capabilityB.ID+"/disposition",
		map[string]any{"disposition": domain.IncidentFaultIncluded, "rationale": "能力级直接影响证据确认，应重新纳入关闭范围"}, http.StatusOK)
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/incidents/"+incident.ID+"/faults/"+capabilityB.ID+"/primary", nil, http.StatusOK)
	var multiFaultDetail domain.IncidentDetail
	status, body, err := client.doJSON(http.MethodGet, "/api/v1/incidents/"+incident.ID, nil, &multiFaultDetail)
	if err != nil || status != http.StatusOK || len(multiFaultDetail.Faults) != 2 {
		t.Fatalf("多 Fault Incident 详情错误 status=%d body=%s faults=%+v err=%v", status, body, multiFaultDetail.Faults, err)
	}
	primaryCount := 0
	for _, link := range multiFaultDetail.Faults {
		if link.Relation == domain.IncidentFaultPrimary {
			primaryCount++
			if link.FaultID != capabilityB.ID {
				t.Fatalf("Primary Fault 回切错误: %+v", link)
			}
		}
		if link.Disposition != domain.IncidentFaultIncluded {
			t.Fatalf("E2E 恢复流程前所有 Fault 应重新纳入: %+v", link)
		}
	}
	if primaryCount != 1 {
		t.Fatalf("Incident primary 数量=%d want=1", primaryCount)
	}
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		return countIncidentMessages(mockLark.Sent(), incident.ID, string(domain.IncidentNotificationDeclared)) == 1
	}, "Incident declared 通报经 outbox 发送到 mock 飞书")
	var f5AlertID string
	for _, alert := range alertsB {
		if alert.Labels["e2b.evidence_class"] == "f5_stream_stall" {
			f5AlertID = alert.ID
			break
		}
	}
	if f5AlertID == "" {
		t.Fatal("stage B 缺少 F5 独立因果链证据")
	}
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/faults/"+capabilityB.ID+"/evidence/"+f5AlertID,
		map[string]any{
			"role":       domain.EvidenceRoleExcluded,
			"rationale":  "人工复核：F5 单向流 stall 与 Sandbox placement 资源耗尽属于独立因果链",
			"confidence": 1,
		}, http.StatusOK)
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		evidence, err := sys.Faults.ListEvidence(ctx, tx, capabilityB.ID)
		if err != nil {
			return err
		}
		for _, item := range evidence {
			if item.AlertID == f5AlertID && item.Role == domain.EvidenceRoleExcluded &&
				item.DecisionOrigin == domain.DecisionOriginHuman && item.DecidedBy == seedResult.AdminUserID {
				return nil
			}
		}
		return fmt.Errorf("F5 excluded 证据未固化为人工优先裁决: %+v", evidence)
	}); err != nil {
		t.Fatal(err)
	}
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/faults/"+capabilityB.ID+"/impacts/"+nodeIDs["insforge.preview-runtime"],
		map[string]any{
			"state":      domain.ImpactStateConfirmed,
			"rationale":  "人工复核：Preview Runtime 合成探针与 Sandbox create SLO 同时失败",
			"confidence": 1,
		}, http.StatusOK)
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		impacts, err := sys.Faults.ListImpacts(ctx, tx, capabilityB.ID)
		if err != nil {
			return err
		}
		for _, impact := range impacts {
			if impact.NodeID == nodeIDs["insforge.preview-runtime"] && impact.State == domain.ImpactStateConfirmed &&
				impact.DecisionOrigin == domain.DecisionOriginHuman && impact.DecidedBy == seedResult.AdminUserID {
				return nil
			}
		}
		return fmt.Errorf("应用 Impact 人工裁决未写入: %+v", impacts)
	}); err != nil {
		t.Fatal(err)
	}

	for role, userID := range map[domain.IncidentRole]string{
		domain.IncidentRoleTechnicalLead:   seedResult.AdminUserID,
		domain.IncidentRoleCommunications:  seedResult.AdminUserID,
		domain.IncidentRoleBusinessLiaison: seedResult.ApproverUserID,
	} {
		assertIncidentAPIStatus(t, client, http.MethodPut,
			"/api/v1/incidents/"+incident.ID+"/roles/"+string(role),
			map[string]any{"user_id": userID}, http.StatusOK)
	}

	var task domain.IncidentTask
	status, body, err = client.doJSON(http.MethodPost, "/api/v1/incidents/"+incident.ID+"/tasks", map[string]any{
		"title":         "停止无效的 Sandbox 批量重试",
		"description":   "保护调度与节点容量，等待根因验证后再逐步恢复",
		"owner_user_id": seedResult.AdminUserID,
	}, &task)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("创建 Incident 任务失败 status=%d body=%s err=%v", status, body, err)
	}
	for _, entry := range []struct {
		kind    domain.IncidentTimelineKind
		summary string
	}{
		{domain.IncidentTimelineFact, "用户级 Sandbox create SLO 已确认失败"},
		{domain.IncidentTimelineHypothesis, "Firecracker 进程漂移可能耗尽节点可调度容量"},
		{domain.IncidentTimelineDecision, "先停止无效重试并保留现场，不执行批量删除"},
	} {
		assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/timeline", map[string]any{
			"kind": entry.kind, "summary": entry.summary,
			"detail": map[string]any{"source": "e2b-golden-replay"},
		}, http.StatusCreated)
	}
	runGuardedE2BCleanup(t, client, incident.ID)

	// 未提供恢复证据时，即使人主动操作也不能把 Incident 推进到 monitoring。
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/transitions",
		map[string]any{"to": domain.IncidentStatusMonitoring}, http.StatusConflict)

	recovery, err := e2bfixture.LoadReplayFixture("recovery.json")
	if err != nil {
		t.Fatal(err)
	}
	postReplayFixture(t, client, recovery)
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		alerts := waitScenarioAlertsSnapshot(recovery.ScenarioID)
		resolved := map[string]bool{}
		for _, alert := range alerts {
			if alert.Status == domain.StatusResolved {
				resolved[alert.Labels["e2b.evidence_class"]] = true
			}
		}
		return resolved["sandbox_create_slo_breach"] && resolved["lifecycle_probe_failure"]
	}, "recovery fixture 经真实 pipeline 恢复用户 SLO 与生命周期探针 Alert")

	criteriaByObservation := map[string]string{
		"sandbox_create_success_rate":    "user_create_slo_recovered",
		"placement_attempt_failure_rate": "placement_attempt_rate_recovered",
		"firecracker_gauge_drift":        "fc_gauge_drift_converged",
		"node_capacity_healthy":          "node_capacity_recovered",
		"lifecycle_probe_success":        "lifecycle_probe_passed",
		"observation_window_complete":    "observation_window_complete",
	}
	for _, observation := range recovery.Observations {
		criterionKey := criteriaByObservation[observation.Name]
		if criterionKey == "observation_window_complete" {
			continue
		}
		passRecoveryCriterion(t, client, incident.ID, criterionKey, observation)
	}

	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/transitions",
		map[string]any{"to": domain.IncidentStatusMonitoring}, http.StatusOK)
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		for _, faultID := range []string{capabilityB.ID, supportingFaultID} {
			fault, err := sys.Faults.GetByID(ctx, tx, faultID)
			if err != nil {
				return err
			}
			if fault.Status != domain.FaultStatusMonitoring {
				return fmt.Errorf("Incident 进入 monitoring 后关联 Fault %s=%s want=monitoring", faultID, fault.Status)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var lateSupportingFaultID string
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT from_fault_id::text FROM fault_relations
			WHERE to_fault_id = $1 AND relation = 'contributes_to' AND from_fault_id <> $2
			ORDER BY created_at LIMIT 1
		`, capabilityB.ID, supportingFaultID).Scan(&lateSupportingFaultID)
	}); err != nil {
		t.Fatalf("查询观察期晚关联 Fault 失败: %v", err)
	}
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/faults",
		map[string]any{"fault_id": lateSupportingFaultID}, http.StatusCreated)
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		return countIncidentMessages(mockLark.Sent(), incident.ID, string(domain.IncidentNotificationMitigated)) == 1
	}, "恢复门槛通过后发送 mitigated 通报")

	// mitigation 通过并不等于事故已结束；观察窗口仍 pending 时关闭必须被拒绝。
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/transitions",
		map[string]any{"to": domain.IncidentStatusResolved}, http.StatusConflict)
	for _, observation := range recovery.Observations {
		if criteriaByObservation[observation.Name] == "observation_window_complete" {
			passRecoveryCriterion(t, client, incident.ID, "observation_window_complete", observation)
		}
	}
	// 指标恢复不代表响应工作已经收口；open 任务必须阻止 Incident 关闭。
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/transitions",
		map[string]any{"to": domain.IncidentStatusResolved}, http.StatusConflict)
	assertIncidentAPIStatus(t, client, http.MethodPatch, "/api/v1/incidents/"+incident.ID+"/tasks/"+task.ID,
		map[string]any{"status": domain.IncidentTaskDone}, http.StatusOK)
	// monitoring 后晚关联的 active Fault 必须单独缓解或明确排除，不能只看 primary 关闭。
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/transitions",
		map[string]any{"to": domain.IncidentStatusResolved}, http.StatusConflict)
	assertIncidentAPIStatus(t, client, http.MethodPost,
		"/api/v1/incidents/"+incident.ID+"/faults/"+lateSupportingFaultID+"/transitions",
		map[string]any{
			"to":        domain.FaultStatusMonitoring,
			"rationale": "观察期复核对象恢复与实例重建均成功，该晚发现 Fault 已缓解",
		}, http.StatusOK)
	assertIncidentAPIStatus(t, client, http.MethodPost, "/api/v1/incidents/"+incident.ID+"/transitions",
		map[string]any{"to": domain.IncidentStatusResolved}, http.StatusOK)
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		return countIncidentMessages(mockLark.Sent(), incident.ID, string(domain.IncidentNotificationResolved)) == 1
	}, "观察窗口通过后发送 resolved 通报")

	var detail domain.IncidentDetail
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		status, _, err := client.doJSON(http.MethodGet, "/api/v1/incidents/"+incident.ID, nil, &detail)
		if err != nil || status != http.StatusOK || detail.Incident.Status != domain.IncidentStatusResolved {
			return false
		}
		kinds := map[domain.IncidentTimelineKind]bool{}
		sentCommunications := 0
		for _, entry := range detail.Timeline {
			kinds[entry.Kind] = true
			if entry.Kind == domain.IncidentTimelineCommunication && entry.Detail["status"] == "sent" {
				sentCommunications++
			}
		}
		return kinds[domain.IncidentTimelineFact] && kinds[domain.IncidentTimelineHypothesis] &&
			kinds[domain.IncidentTimelineDecision] && kinds[domain.IncidentTimelineAutomation] &&
			kinds[domain.IncidentTimelineCommunication] && sentCommunications == 3
	}, "Incident 详情聚合角色、任务、恢复判据与完整通信审计时间线")
	if len(detail.Roles) != 4 || len(detail.Tasks) != 1 || detail.Tasks[0].Status != domain.IncidentTaskDone {
		t.Fatalf("Incident 协作状态不完整: roles=%+v tasks=%+v", detail.Roles, detail.Tasks)
	}
	if countIncidentMessages(mockLark.Sent(), incident.ID, "") != 3 {
		t.Fatalf("Incident 应且仅应发送 declared/mitigated/resolved 三次通报")
	}
	if err := sys.Store.WithTx(ctx, func(tx pgx.Tx) error {
		for _, faultID := range []string{capabilityB.ID, supportingFaultID, lateSupportingFaultID} {
			fault, err := sys.Faults.GetByID(ctx, tx, faultID)
			if err != nil {
				return err
			}
			if fault.Status != domain.FaultStatusResolved {
				return fmt.Errorf("Incident 关闭后 included Fault %s=%s want=resolved", faultID, fault.Status)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runGuardedE2BCleanup(t *testing.T, client *apiClient, incidentID string) {
	t.Helper()
	approverClient := newLoggedInApproverClient(t)
	applyPath := "/api/v1/incidents/" + incidentID + "/runbook-apply"
	runsPath := "/api/v1/incidents/" + incidentID + "/runbook-runs"

	// 任意命令与 UAT-only 动作都不能通过 action_key 绕过白名单/授权边界。
	assertIncidentAPIStatus(t, client, http.MethodPost, runsPath, map[string]any{
		"action_key": "shell", "parameters": map[string]any{"command": "pkill firecracker"},
	}, http.StatusConflict)
	assertIncidentAPIStatus(t, client, http.MethodPost, runsPath, map[string]any{
		"action_key": runbook.ActionLifecycleVerify,
		"parameters": map[string]any{
			"environment_id": "isolated-uat", "authorization_ref": "self-approved",
			"template_id": "tpl-untrusted", "confirm_cleanup": true,
		},
	}, http.StatusConflict)
	var definitions struct {
		Items []domain.RunbookDefinition `json:"items"`
	}
	status, body, err := client.doJSON(http.MethodGet, "/api/v1/runbooks", nil, &definitions)
	if err != nil || status != http.StatusOK {
		t.Fatalf("查询 Runbook 运行时能力失败 status=%d body=%s err=%v", status, body, err)
	}
	foundUnavailableLifecycle := false
	for _, definition := range definitions.Items {
		if definition.ActionKey == runbook.ActionLifecycleVerify && !definition.RuntimeAvailable && definition.RuntimeReason != "" {
			foundUnavailableLifecycle = true
		}
	}
	if !foundUnavailableLifecycle {
		t.Fatalf("未配置 UAT policy 时，目录必须明确标记 lifecycle 不可用: %+v", definitions.Items)
	}

	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"retries_stopped": true,
	}, http.StatusConflict)

	plan1 := createCleanupDryRun(t, client, incidentID)
	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan1.ID, "confirm_plan_sha256": plan1.PlanSHA256 + "tampered",
		"retries_stopped": true,
	}, http.StatusConflict)
	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan1.ID, "confirm_plan_sha256": plan1.PlanSHA256,
		"retries_stopped": false,
	}, http.StatusConflict)
	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan1.ID, "confirm_plan_sha256": plan1.PlanSHA256,
		"retries_stopped": true,
	}, http.StatusConflict)

	if err := sys.Store.WithTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			UPDATE runbook_runs SET plan = jsonb_set(plan, '{tampered}', 'true'::jsonb) WHERE id = $1
		`, plan1.ID)
		return err
	}); err != nil {
		t.Fatalf("篡改计划内容失败: %v", err)
	}
	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan1.ID, "confirm_plan_sha256": plan1.PlanSHA256,
		"retries_stopped": true,
	}, http.StatusConflict)
	if err := sys.Store.WithTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			UPDATE runbook_runs
			SET plan = plan - 'tampered', plan_expires_at = now() - interval '1 second'
			WHERE id = $1
		`, plan1.ID)
		return err
	}); err != nil {
		t.Fatalf("设置过期计划失败: %v", err)
	}
	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan1.ID, "confirm_plan_sha256": plan1.PlanSHA256,
		"retries_stopped": true,
	}, http.StatusConflict)

	plan2 := createCleanupDryRun(t, client, incidentID)
	approvalPath := runsPath + "/" + plan2.ID + "/approval"
	assertIncidentAPIStatus(t, client, http.MethodPost, approvalPath, map[string]any{
		"plan_sha256": plan2.PlanSHA256, "decision": domain.RunbookApprovalApproved,
		"comment": "执行人自己审批，仅用于验证职责分离护栏",
	}, http.StatusOK)
	assertIncidentAPIStatus(t, client, http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan2.ID, "confirm_plan_sha256": plan2.PlanSHA256,
		"retries_stopped": true,
	}, http.StatusConflict)

	assertIncidentAPIStatus(t, approverClient, http.MethodPost, approvalPath, map[string]any{
		"plan_sha256": plan2.PlanSHA256, "decision": domain.RunbookApprovalApproved,
		"comment": "独立审批：计划目标、工单与停止重试记录已复核",
	}, http.StatusOK)
	var applyRun domain.RunbookRun
	status, body, err = client.doJSON(http.MethodPost, applyPath, map[string]any{
		"plan_run_id": plan2.ID, "confirm_plan_sha256": plan2.PlanSHA256,
		"retries_stopped": true,
	}, &applyRun)
	if err != nil || status != http.StatusCreated || applyRun.Status != domain.RunbookRunSucceeded {
		t.Fatalf("受控 cleanup apply 失败 status=%d body=%s run=%+v err=%v", status, body, applyRun, err)
	}
	if external, _ := applyRun.Result["external_mutation"].(bool); external {
		t.Fatalf("本地 E2E fake executor 不得修改外部 E2B: %+v", applyRun.Result)
	}

	var audit struct {
		Items []domain.RunbookRun `json:"items"`
	}
	status, body, err = client.doJSON(http.MethodGet, runsPath, nil, &audit)
	if err != nil || status != http.StatusOK {
		t.Fatalf("查询 Runbook 审计失败 status=%d body=%s err=%v", status, body, err)
	}
	rejected := 0
	succeededApply := 0
	unknownActionRedacted := false
	for _, item := range audit.Items {
		if item.Status == domain.RunbookRunRejected {
			rejected++
		}
		if item.ActionKey == runbook.ActionCleanupApply && item.Status == domain.RunbookRunSucceeded {
			succeededApply++
		}
		if item.ActionKey == "shell" && item.Parameters["command"] == "[redacted]" {
			unknownActionRedacted = true
		}
	}
	if rejected < 8 || succeededApply != 1 || !unknownActionRedacted {
		t.Fatalf("Runbook 护栏审计不完整 rejected=%d succeeded_apply=%d redacted=%v items=%+v", rejected, succeededApply, unknownActionRedacted, audit.Items)
	}
	executions := sys.RunbookExecutor.Executions()
	applyExecutions := 0
	for _, execution := range executions {
		if execution.Request.IncidentID == incidentID && execution.Request.ActionKey == runbook.ActionCleanupApply {
			applyExecutions++
		}
	}
	if applyExecutions != 1 {
		t.Fatalf("只有通过全部护栏的一次 apply 可到达执行器，实际=%d", applyExecutions)
	}
}

func createCleanupDryRun(t *testing.T, client *apiClient, incidentID string) domain.RunbookRun {
	t.Helper()
	var run domain.RunbookRun
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/incidents/"+incidentID+"/runbook-runs", map[string]any{
		"action_key": runbook.ActionCleanupDryRun,
		"parameters": map[string]any{
			"target_refs":        []string{"sid-a91c", "sid-b72e"},
			"retries_stopped":    true,
			"retries_stopped_at": time.Now().UTC().Format(time.RFC3339Nano),
			"retries_stopped_by": "incident-commander",
			"ticket_ref":         "INC-E2B-GOLDEN",
		},
	}, &run)
	if err != nil || status != http.StatusCreated || run.Status != domain.RunbookRunSucceeded ||
		run.PlanSHA256 == "" || run.PlanExpiresAt.IsZero() {
		t.Fatalf("cleanup dry-run 失败 status=%d body=%s run=%+v err=%v", status, body, run, err)
	}
	return run
}

func declareE2BIncident(t *testing.T, client *apiClient, faultID string, wantCreated bool) domain.Incident {
	t.Helper()
	var response struct {
		Incident domain.Incident `json:"incident"`
		Created  bool            `json:"created"`
	}
	wantStatus := http.StatusOK
	if wantCreated {
		wantStatus = http.StatusCreated
	}
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/incidents", map[string]any{
		"fault_id":                 faultID,
		"title":                    "E2B Sandbox placement capability unavailable",
		"summary":                  "Failed to place sandbox 已影响 insforge.preview-runtime 用户创建 SLO",
		"severity":                 "critical",
		"communication_channel":    "lark",
		"communication_target_ref": "e2b-incident-room",
	}, &response)
	if err != nil || status != wantStatus || response.Created != wantCreated || response.Incident.ID == "" {
		t.Fatalf("宣布 Incident 失败 status=%d want=%d body=%s response=%+v err=%v", status, wantStatus, body, response, err)
	}
	return response.Incident
}

func assertIncidentAPIStatus(t *testing.T, client *apiClient, method, path string, payload any, want int) {
	t.Helper()
	status, body, err := client.doJSON(method, path, payload, nil)
	if err != nil || status != want {
		t.Fatalf("Incident API %s %s status=%d want=%d body=%s err=%v", method, path, status, want, body, err)
	}
}

func passRecoveryCriterion(t *testing.T, client *apiClient, incidentID, key string, observation e2bfixture.ReplayObservation) {
	t.Helper()
	if key == "" {
		t.Fatalf("恢复观测 %q 没有对应 criterion", observation.Name)
	}
	assertIncidentAPIStatus(t, client, http.MethodPut,
		"/api/v1/incidents/"+incidentID+"/recovery-criteria/"+key,
		map[string]any{
			"status": domain.RecoveryCriterionPassed,
			"evidence": map[string]any{
				"kind": observation.Kind, "name": observation.Name,
				"value": observation.Value, "threshold": observation.Threshold,
				"breached": observation.Breached,
			},
			"observed_at": observation.ObservedAt,
		}, http.StatusOK)
}

func countIncidentMessages(messages []mockSend, incidentID, event string) int {
	count := 0
	for _, message := range messages {
		if message.Msg.Labels["incident_id"] != incidentID {
			continue
		}
		if event == "" || message.Msg.Labels["incident_event"] == event {
			count++
		}
	}
	return count
}

func waitScenarioAlertsSnapshot(scenarioID string) []domain.Alert {
	var alerts []domain.Alert
	_ = sys.Store.WithTx(context.Background(), func(tx pgx.Tx) error {
		page, err := sys.Alerts.List(context.Background(), tx, store.AlertFilter{
			Labels: map[string]string{"scenario_id": scenarioID}, Limit: 100,
		})
		alerts = page.Alerts
		return err
	})
	return alerts
}

func postReplayFixture(t *testing.T, client *apiClient, fixture e2bfixture.ReplayFixture) {
	t.Helper()
	status, body, err := client.postManagedHook(
		seedResult.GenericWebhookPath,
		mustJSON(fixture.GenericPayloads()),
	)
	if err != nil {
		t.Fatalf("回放 %s ingest 失败: %v", fixture.Phase, err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("回放 %s ingest status=%d body=%s", fixture.Phase, status, body)
	}
}

func waitScenarioAlerts(t *testing.T, scenarioID string, minCount int) []domain.Alert {
	t.Helper()
	var alerts []domain.Alert
	waitFor(t, 15*time.Second, 200*time.Millisecond, func() bool {
		var page store.AlertPage
		err := sys.Store.WithTx(context.Background(), func(tx pgx.Tx) error {
			var err error
			page, err = sys.Alerts.List(context.Background(), tx, store.AlertFilter{
				Labels: map[string]string{"scenario_id": scenarioID}, Limit: 100,
			})
			return err
		})
		if err != nil || page.Total < minCount {
			return false
		}
		alerts = page.Alerts
		return true
	}, fmt.Sprintf("场景 %s 至少 %d 条 Alert 完成 pipeline", scenarioID, minCount))
	return alerts
}

func findStoredCapability(faults []domain.Fault) domain.Fault {
	for _, fault := range faults {
		if fault.CorrelationKey == correlation.E2BCapabilityCorrelationKey {
			return fault
		}
	}
	return domain.Fault{}
}

func evidenceHasRole(evidence []domain.FaultEvidence, role domain.EvidenceRole) bool {
	for _, item := range evidence {
		if item.Role == role {
			return true
		}
	}
	return false
}
