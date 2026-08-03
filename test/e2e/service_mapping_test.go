package e2e

import (
	"net/http"
	"testing"
	"time"

	"alert-system/internal/domain"
)

// TestGenericServiceCatalogMapping_E2BAndCCH proves that onboarding a second
// business requires data only: service + alias + policy. Core code has no branch
// for either E2B or CCH.
func TestGenericServiceCatalogMapping_E2BAndCCH(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	source := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "mapping-source-" + suffix,
		"default_severity": "warning", "enabled": true,
		"config": map[string]any{"reconcile_interval_seconds": 3600, "stale_after_seconds": 7200, "missing_confirmations": 2},
	}, http.StatusCreated)
	sourceID, _ := source["id"].(string)
	webhookPath, _ := source["webhook_path"].(string)

	e2bKey := "service/e2b/" + suffix
	cchKey := "service/cch/" + suffix
	createMappedService(t, client, e2bKey, "E2B Sandbox Runtime", "e2b-"+suffix)
	cchService := createMappedService(t, client, cchKey, "Claude Code Hub", "cch-"+suffix)
	cchServiceID, _ := cchService["id"].(string)
	status, body, err := client.doJSON(http.MethodPut, "/api/v1/admin/services/"+cchServiceID, map[string]any{
		"entity_key": cchKey + "-renamed", "name": "Claude Code Hub", "environment": "prod",
		"owner_team_id": seedResult.TeamID, "description": "generic E2E service",
		"tier": "tier-1", "enabled": true,
		"aliases": []map[string]any{{"namespace": "prometheus.service", "value": "cch-" + suffix, "enabled": true}},
	}, nil)
	if err != nil || status != http.StatusUnprocessableEntity {
		t.Fatalf("canonical entity key 应保持不可变 status=%d body=%s err=%v", status, body, err)
	}
	createMappingPolicy(t, client, 20000, "e2b-failed-to-place-"+suffix, "FailedToPlaceSandbox", "sandbox_placement_failure")
	createMappingPolicy(t, client, 20001, "cch-proxy-slo-"+suffix, "CCHProxySLOBreach", "proxy_request_degradation")

	e2bAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "FailedToPlaceSandbox", "e2b-"+suffix, "critical", "e2b-map-"+suffix)
	cchAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "CCHProxySLOBreach", "cch-"+suffix, "critical", "cch-map-"+suffix)
	assertMappedToServiceFault(t, client, e2bAlert.ID, e2bKey, "sandbox_placement_failure")
	assertMappedToServiceFault(t, client, cchAlert.ID, cchKey, "proxy_request_degradation")

	// No policy means governance queue, never an inferred active Fault.
	unmappedAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "UnknownBusinessAlarm", "new-business-"+suffix, "warning", "unmapped-"+suffix)
	assertMappingStatus(t, client, unmappedAlert.ID, "unmapped", "no_policy")

	// A duplicate enabled alias is intentionally representable and must isolate
	// the next occurrence instead of selecting an arbitrary owner.
	createMappedService(t, client, "service/cch-shadow/"+suffix, "CCH Shadow Claim", "cch-"+suffix)
	conflictAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "CCHProxySLOBreach", "cch-"+suffix, "critical", "cch-conflict-"+suffix)
	assertMappingStatus(t, client, conflictAlert.ID, "conflict", "alias_conflict")
}

func TestGenericEntityRelations_E2BAndCCHBlastRadius(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	source := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "radius-source-" + suffix,
		"default_severity": "warning", "enabled": true,
		"config": map[string]any{"reconcile_interval_seconds": 3600, "stale_after_seconds": 7200, "missing_confirmations": 2},
	}, http.StatusCreated)
	sourceID, _ := source["id"].(string)
	webhookPath, _ := source["webhook_path"].(string)

	provider := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/shared-kubernetes/" + suffix,
		"layer":      "platform", "name": "Shared Production Kubernetes", "provider": "Kubernetes",
		"environment": "prod", "owner_team_id": seedResult.TeamID,
	}, http.StatusCreated)
	providerID, _ := provider["id"].(string)
	if providerID == "" {
		t.Fatalf("受管平台实体响应缺少 id: %+v", provider)
	}

	e2bKey := "service/e2b-radius/" + suffix
	cchKey := "service/cch-radius/" + suffix
	e2bService := createMappedServiceWithDependencies(t, client, e2bKey, "E2B Radius App", "e2b-radius-"+suffix, providerID)
	cchService := createMappedServiceWithDependencies(t, client, cchKey, "CCH Radius App", "cch-radius-"+suffix, providerID)
	e2bEntityID := serviceEntityID(t, e2bService)
	cchEntityID := serviceEntityID(t, cchService)

	createFixedTargetMappingPolicy(t, client, 21000, "shared-kubernetes-pressure-"+suffix, "SharedKubernetesPressure", providerID, "cluster_capacity_degradation")
	createMappingPolicy(t, client, 21001, "e2b-radius-slo-"+suffix, "E2BRadiusSLOBreach", "application_slo_degradation")
	createMappingPolicy(t, client, 21002, "cch-radius-slo-"+suffix, "CCHRadiusSLOBreach", "application_slo_degradation")

	providerAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "SharedKubernetesPressure", "shared-k8s-"+suffix, "critical", "provider-radius-"+suffix)
	providerFaultID := mappingFaultID(t, client, providerAlert.ID)
	assertImpactStates(t, client, providerFaultID, map[string]domain.ImpactState{
		e2bEntityID: domain.ImpactStatePotential,
		cchEntityID: domain.ImpactStatePotential,
	}, true)

	cchStartsAt := time.Now().UTC().Truncate(time.Millisecond)
	postAuthorityMappingOccurrence(t, client, sourceID, webhookPath, "firing", "CCHRadiusSLOBreach", "cch-radius-"+suffix, "critical", "cch-radius-evidence-"+suffix, cchStartsAt)
	assertImpactStates(t, client, providerFaultID, map[string]domain.ImpactState{
		e2bEntityID: domain.ImpactStatePotential,
		cchEntityID: domain.ImpactStateConfirmed,
	}, true)

	postAuthorityMappingOccurrence(t, client, sourceID, webhookPath, "resolved", "CCHRadiusSLOBreach", "cch-radius-"+suffix, "critical", "cch-radius-evidence-"+suffix, cchStartsAt)
	assertImpactStates(t, client, providerFaultID, map[string]domain.ImpactState{
		e2bEntityID: domain.ImpactStatePotential,
		cchEntityID: domain.ImpactStatePotential,
	}, true)

	status, body, err := client.doJSON(http.MethodPut, "/api/v1/faults/"+providerFaultID+"/impacts/"+e2bEntityID, map[string]any{
		"state": "ruled_out", "rationale": "E2B synthetic 与用户 SLO 均健康", "confidence": 1,
	}, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("人工排除 E2B Impact 失败 status=%d body=%s err=%v", status, body, err)
	}
	postAuthorityMappingAlert(t, client, sourceID, webhookPath, "E2BRadiusSLOBreach", "e2b-radius-"+suffix, "critical", "e2b-radius-evidence-"+suffix)
	assertImpactStates(t, client, providerFaultID, map[string]domain.ImpactState{
		e2bEntityID: domain.ImpactStateRuledOut,
		cchEntityID: domain.ImpactStatePotential,
	}, true)
}

func createMappedService(t *testing.T, client *apiClient, entityKey, name, alias string) map[string]any {
	return createMappedServiceWithDependencies(t, client, entityKey, name, alias)
}

func createMappedServiceWithDependencies(t *testing.T, client *apiClient, entityKey, name, alias string, dependencyIDs ...string) map[string]any {
	t.Helper()
	dependencies := make([]map[string]any, 0, len(dependencyIDs))
	for _, dependencyID := range dependencyIDs {
		dependencies = append(dependencies, map[string]any{"to_entity_id": dependencyID, "criticality": "critical"})
	}
	return adminCreate(t, client, "/api/v1/admin/services", map[string]any{
		"entity_key": entityKey, "name": name, "environment": "prod",
		"owner_team_id": seedResult.TeamID, "description": "generic E2E service",
		"tier": "tier-1", "enabled": true,
		"aliases":      []map[string]any{{"namespace": "prometheus.service", "value": alias, "enabled": true}},
		"dependencies": dependencies,
	}, http.StatusCreated)
}

func serviceEntityID(t *testing.T, service map[string]any) string {
	t.Helper()
	entity, _ := service["entity"].(map[string]any)
	id, _ := entity["id"].(string)
	if id == "" {
		t.Fatalf("服务响应缺少 entity.id: %+v", service)
	}
	return id
}

func createMappingPolicy(t *testing.T, client *apiClient, priority int, name, alertname, faultKind string) {
	t.Helper()
	adminCreate(t, client, "/api/v1/admin/alert-mappings", map[string]any{
		"name": name, "priority": priority,
		"matchers":        []map[string]string{{"name": "alertname", "op": "=", "value": alertname}},
		"alias_namespace": "prometheus.service", "alias_label": "service",
		"fault_kind": faultKind, "fault_scope": "capability",
		"evidence_role": "direct_impact", "correlation_labels": []string{"cluster"},
		"confidence": 0.98, "enabled": true,
	}, http.StatusCreated)
}

func createFixedTargetMappingPolicy(t *testing.T, client *apiClient, priority int, name, alertname, entityID, faultKind string) {
	t.Helper()
	adminCreate(t, client, "/api/v1/admin/alert-mappings", map[string]any{
		"name": name, "priority": priority,
		"matchers":         []map[string]string{{"name": "alertname", "op": "=", "value": alertname}},
		"target_entity_id": entityID, "fault_kind": faultKind, "fault_scope": "capability",
		"evidence_role": "direct_impact", "correlation_labels": []string{"cluster"},
		"confidence": 0.97, "enabled": true,
	}, http.StatusCreated)
}

func postAuthorityMappingAlert(t *testing.T, client *apiClient, sourceID, webhookPath, alertname, service, severity, fingerprint string) alertDTO {
	t.Helper()
	return postAuthorityMappingOccurrence(t, client, sourceID, webhookPath, "firing", alertname, service, severity, fingerprint, time.Now().UTC().Truncate(time.Millisecond))
}

func postAuthorityMappingOccurrence(t *testing.T, client *apiClient, sourceID, webhookPath, sourceState, alertname, service, severity, fingerprint string, startsAt time.Time) alertDTO {
	t.Helper()
	endsAt := time.Time{}
	if sourceState == "resolved" {
		endsAt = time.Now().UTC()
	}
	payload := mustJSON(map[string]any{
		"version": "4", "status": sourceState, "receiver": "mapping-e2e",
		"alerts": []map[string]any{{
			"status":      sourceState,
			"labels":      map[string]string{"alertname": alertname, "service": service, "severity": severity, "cluster": "prod-a", "mapping_e2e": fingerprint},
			"annotations": map[string]string{"summary": alertname + " generic mapping probe"},
			"startsAt":    startsAt.Format(time.RFC3339Nano), "endsAt": endsAt.Format(time.RFC3339Nano),
			"fingerprint": fingerprint,
		}},
	})
	status, body, err := client.postManagedHook(webhookPath, payload)
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("投递 Mapping Alert 失败 status=%d body=%s err=%v", status, body, err)
	}
	var alert alertDTO
	waitFor(t, 10*time.Second, 100*time.Millisecond, func() bool {
		candidate, ok, err := findAlertByLabel(client, "mapping_e2e", fingerprint)
		if err != nil || !ok || candidate.SourceIntegrationID != sourceID || candidate.SourceState != sourceState {
			return false
		}
		// Multiple occurrences can share alertname; locate the newest source alert.
		alert = candidate
		return true
	}, "Alertmanager occurrence 入库")
	return alert
}

func mappingFaultID(t *testing.T, client *apiClient, alertID string) string {
	t.Helper()
	var mapping map[string]any
	waitFor(t, 10*time.Second, 100*time.Millisecond, func() bool {
		status, _, err := client.doJSON(http.MethodGet, "/api/v1/alerts/"+alertID+"/mapping", nil, &mapping)
		return err == nil && status == http.StatusOK && mapping["status"] == "mapped"
	}, "读取 Alert Mapping Fault")
	faultID, _ := mapping["fault_id"].(string)
	if faultID == "" {
		t.Fatalf("mapping 缺少 fault_id: %+v", mapping)
	}
	return faultID
}

func assertImpactStates(t *testing.T, client *apiClient, faultID string, want map[string]domain.ImpactState, requireReadablePath bool) {
	t.Helper()
	type impactItem struct {
		Impact    domain.FaultImpact    `json:"impact"`
		Node      domain.ResourceNode   `json:"node"`
		PathNodes []domain.ResourceNode `json:"path_nodes"`
	}
	var detail struct {
		Impacts []impactItem `json:"impacts"`
	}
	waitFor(t, 10*time.Second, 100*time.Millisecond, func() bool {
		status, _, err := client.doJSON(http.MethodGet, "/api/v1/faults/"+faultID, nil, &detail)
		if err != nil || status != http.StatusOK || len(detail.Impacts) != len(want) {
			return false
		}
		for _, item := range detail.Impacts {
			if want[item.Node.ID] != item.Impact.State {
				return false
			}
			if requireReadablePath && (len(item.PathNodes) < 2 || item.PathNodes[len(item.PathNodes)-1].ID != item.Node.ID) {
				return false
			}
		}
		return true
	}, "Fault Impact 状态与可读路径收敛")
}

func assertMappedToServiceFault(t *testing.T, client *apiClient, alertID, entityKey, faultKind string) {
	t.Helper()
	var mapping map[string]any
	waitFor(t, 10*time.Second, 100*time.Millisecond, func() bool {
		status, _, err := client.doJSON(http.MethodGet, "/api/v1/alerts/"+alertID+"/mapping", nil, &mapping)
		return err == nil && status == http.StatusOK && mapping["status"] == "mapped"
	}, "Alert 映射为 Fault")
	if mapping["entity_key"] != entityKey {
		t.Fatalf("mapping entity_key=%v want=%s: %+v", mapping["entity_key"], entityKey, mapping)
	}
	faultID, _ := mapping["fault_id"].(string)
	if faultID == "" {
		t.Fatalf("mapping 未返回 fault_id: %+v", mapping)
	}
	var detail struct {
		Fault struct {
			Kind      string `json:"kind"`
			EntityRef string `json:"entity_ref"`
			Status    string `json:"status"`
		} `json:"fault"`
		Evidence []map[string]any `json:"evidence"`
		Impacts  []map[string]any `json:"impacts"`
	}
	status, body, err := client.doJSON(http.MethodGet, "/api/v1/faults/"+faultID, nil, &detail)
	if err != nil || status != http.StatusOK {
		t.Fatalf("读取映射 Fault 失败 status=%d body=%s err=%v", status, body, err)
	}
	if detail.Fault.Kind != faultKind || detail.Fault.EntityRef != entityKey || detail.Fault.Status != "active" || len(detail.Evidence) != 1 || len(detail.Impacts) != 1 {
		t.Fatalf("映射 Fault 不完整: %+v", detail)
	}
}

func assertMappingStatus(t *testing.T, client *apiClient, alertID, wantStatus, wantReason string) {
	t.Helper()
	var mapping map[string]any
	waitFor(t, 10*time.Second, 100*time.Millisecond, func() bool {
		status, _, err := client.doJSON(http.MethodGet, "/api/v1/alerts/"+alertID+"/mapping", nil, &mapping)
		return err == nil && status == http.StatusOK
	}, "Mapping 治理结果生成")
	if mapping["status"] != wantStatus || mapping["reason_code"] != wantReason || mapping["fault_id"] != nil && mapping["fault_id"] != "" {
		t.Fatalf("mapping=%+v want status=%s reason=%s no fault", mapping, wantStatus, wantReason)
	}
}
