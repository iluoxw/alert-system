package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// TestEntityLifecycle_DecommissionCascadeLeavesIncidentOpen 钉住 D3 与遍历断链：
// 挂着 active Fault + 未关闭 Incident 的实体下线并 cascade 后，Fault 收口、
// Incident 仍开、时间线写的是下线不是恢复、爆炸半径不再含该实体及其下游。
func TestEntityLifecycle_DecommissionCascadeLeavesIncidentOpen(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	reason := "节点已从集群剔除 " + suffix

	upstream := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/lifecycle-up/" + suffix,
		"layer":      "platform", "name": "Lifecycle Upstream", "provider": "test",
		"environment": "prod", "owner_team_id": seedResult.TeamID,
	}, http.StatusCreated)
	upstreamID, _ := upstream["id"].(string)
	mid := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/lifecycle-mid/" + suffix,
		"layer":      "platform", "name": "Lifecycle Mid", "provider": "test",
		"environment": "prod", "owner_team_id": seedResult.TeamID,
		"dependencies": []map[string]any{{"to_entity_id": upstreamID, "criticality": "critical"}},
	}, http.StatusCreated)
	midID, _ := mid["id"].(string)
	if upstreamID == "" || midID == "" {
		t.Fatalf("受管实体缺少 id: upstream=%+v mid=%+v", upstream, mid)
	}

	downstream := createMappedServiceWithDependencies(t, client, "service/lifecycle-down/"+suffix, "Lifecycle Downstream", "lifecycle-down-"+suffix, midID)
	bypass := createMappedServiceWithDependencies(t, client, "service/lifecycle-bypass/"+suffix, "Lifecycle Bypass", "lifecycle-bypass-"+suffix, upstreamID)
	downstreamID := serviceEntityID(t, downstream)
	bypassID := serviceEntityID(t, bypass)

	source := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "lifecycle-source-" + suffix,
		"default_severity": "warning", "enabled": true,
		"config": map[string]any{"reconcile_interval_seconds": 3600, "stale_after_seconds": 7200, "missing_confirmations": 2},
	}, http.StatusCreated)
	sourceID, _ := source["id"].(string)
	webhookPath, _ := source["webhook_path"].(string)
	createFixedTargetMappingPolicy(t, client, 32000, "lifecycle-mid-"+suffix, "LifecycleMidDown", midID, "exporter_down")
	createMappingPolicy(t, client, 32001, "lifecycle-down-slo-"+suffix, "LifecycleDownSLO", "application_slo_degradation")

	midAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "LifecycleMidDown", "mid-"+suffix, "critical", "lifecycle-mid-"+suffix)
	faultID := mappingFaultID(t, client, midAlert.ID)
	sloStartsAt := time.Now().UTC().Truncate(time.Millisecond)
	postAuthorityMappingOccurrence(t, client, sourceID, webhookPath, "firing", "LifecycleDownSLO", "lifecycle-down-"+suffix, "critical", "lifecycle-down-"+suffix, sloStartsAt)
	assertImpactStates(t, client, faultID, map[string]domain.ImpactState{
		downstreamID: domain.ImpactStateConfirmed,
	}, true)

	var declared struct {
		Incident domain.Incident `json:"incident"`
		Created  bool            `json:"created"`
	}
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/incidents", map[string]any{
		"fault_id": faultID, "title": "生命周期下线收口 " + suffix,
		"summary": "采集目标已消失", "severity": "critical",
		"communication_channel": "lark_webhook", "communication_target_ref": "https://open.feishu.cn/lifecycle-e2e",
	}, &declared)
	if err != nil || status != http.StatusCreated || !declared.Created || declared.Incident.ID == "" {
		t.Fatalf("宣布 Incident 失败 status=%d body=%s err=%v", status, body, err)
	}
	incidentID := declared.Incident.ID
	if declared.Incident.Status != domain.IncidentStatusDeclared {
		t.Fatalf("新建 Incident 状态=%s want declared", declared.Incident.Status)
	}

	var preview struct {
		ActiveFaults  []domain.Fault    `json:"active_faults"`
		OpenIncidents []domain.Incident `json:"open_incidents"`
	}
	status, body, err = client.doJSON(http.MethodGet, "/api/v1/admin/entities/"+midID+"/lifecycle-preview", nil, &preview)
	if err != nil || status != http.StatusOK || len(preview.ActiveFaults) != 1 || preview.ActiveFaults[0].ID != faultID ||
		len(preview.OpenIncidents) != 1 || preview.OpenIncidents[0].ID != incidentID {
		t.Fatalf("下线前 preview 应列出 Fault 与 Incident status=%d body=%s err=%v preview=%+v", status, body, err, preview)
	}

	before := blastRadiusIDs(t, upstreamID)
	if _, ok := before[midID]; !ok {
		t.Fatalf("下线前上游爆炸半径应含待下线实体: %+v", before)
	}
	if _, ok := before[downstreamID]; !ok {
		t.Fatalf("下线前上游爆炸半径应含下游: %+v", before)
	}
	if before[upstreamID] != 0 || before[bypassID] != 1 {
		t.Fatalf("下线前 active 旁路不完整: %+v", before)
	}

	status, body, err = client.doJSON(http.MethodPatch, "/api/v1/admin/entities/"+midID+"/lifecycle", map[string]any{
		"state": "decommissioned", "reason": reason,
	}, nil)
	if err != nil || status != http.StatusConflict {
		t.Fatalf("挂着 active Fault 且未 cascade 应 409 status=%d body=%s err=%v", status, body, err)
	}

	var patched struct {
		Entity         domain.ResourceNode `json:"entity"`
		CascadedFaults []domain.Fault      `json:"cascaded_faults"`
	}
	status, body, err = client.doJSON(http.MethodPatch, "/api/v1/admin/entities/"+midID+"/lifecycle", map[string]any{
		"state": "decommissioned", "reason": reason, "cascade_faults": true,
	}, &patched)
	if err != nil || status != http.StatusOK {
		t.Fatalf("cascade 下线失败 status=%d body=%s err=%v", status, body, err)
	}
	if patched.Entity.LifecycleState != domain.EntityLifecycleDecommissioned ||
		len(patched.CascadedFaults) != 1 || patched.CascadedFaults[0].ID != faultID ||
		patched.CascadedFaults[0].Status != domain.FaultStatusResolved {
		t.Fatalf("cascade 响应不符合收口约定: %+v", patched)
	}

	var faultDetail struct {
		Fault    domain.Fault     `json:"fault"`
		Impacts  []impactItem     `json:"impacts"`
		Incident *domain.Incident `json:"incident"`
	}
	status, body, err = client.doJSON(http.MethodGet, "/api/v1/faults/"+faultID, nil, &faultDetail)
	if err != nil || status != http.StatusOK {
		t.Fatalf("读取 Fault 失败 status=%d body=%s err=%v", status, body, err)
	}
	if faultDetail.Fault.Status != domain.FaultStatusResolved || faultDetail.Fault.ResolvedAt.IsZero() {
		t.Fatalf("D3/R3.3：Fault 应收口为 resolved: %+v", faultDetail.Fault)
	}
	for _, item := range faultDetail.Impacts {
		if item.Node.ID == downstreamID || item.Node.ID == midID {
			t.Fatalf("Fault 影响面不应再含下线实体或其下游: %+v", item)
		}
	}

	var incidentDetail domain.IncidentDetail
	status, body, err = client.doJSON(http.MethodGet, "/api/v1/incidents/"+incidentID, nil, &incidentDetail)
	if err != nil || status != http.StatusOK {
		t.Fatalf("读取 Incident 失败 status=%d body=%s err=%v", status, body, err)
	}
	if incidentDetail.Incident.Status != domain.IncidentStatusDeclared || !incidentDetail.Incident.ResolvedAt.IsZero() {
		t.Fatalf("D3：下线不得关闭 Incident: %+v", incidentDetail.Incident)
	}
	if faultDetail.Incident == nil || faultDetail.Incident.ID != incidentID ||
		faultDetail.Incident.Status != domain.IncidentStatusDeclared {
		t.Fatalf("Fault 详情仍应挂着未关闭 Incident: %+v", faultDetail.Incident)
	}
	foundDecommissionNote := false
	for _, entry := range incidentDetail.Timeline {
		if entry.Kind == domain.IncidentTimelineFact && strings.Contains(entry.Summary, "下线") {
			if !strings.Contains(entry.Summary, "不是恢复") {
				t.Fatalf("下线时间线必须写明不是恢复: %+v", entry)
			}
			if reasonVal, _ := entry.Detail["reason"].(string); reasonVal != reason {
				t.Fatalf("下线时间线 reason=%q want %q entry=%+v", reasonVal, reason, entry)
			}
			foundDecommissionNote = true
		}
		if entry.Kind == domain.IncidentTimelineStatusChange {
			to, _ := entry.Detail["to"].(string)
			if to != "" && to != string(domain.IncidentStatusDeclared) {
				t.Fatalf("下线不得推进 Incident 状态: %+v", entry)
			}
		}
	}
	if !foundDecommissionNote {
		t.Fatalf("Incident 时间线应写下线收口说明: %+v", incidentDetail.Timeline)
	}
	if !faultTimelineHasAction(t, faultID, string(domain.FaultTimelineActionEntityDecommissioned)) {
		t.Fatal("Fault 时间线应有 entity_decommissioned")
	}

	var nodeDetail struct {
		Node   domain.ResourceNode `json:"node"`
		Faults []domain.Fault      `json:"faults"`
		Edges  []map[string]any    `json:"edges"`
	}
	status, body, err = client.doJSON(http.MethodGet, "/api/v1/infrastructure/nodes/"+midID, nil, &nodeDetail)
	if err != nil || status != http.StatusOK {
		t.Fatalf("实体详情（v0.3.0 曾 500）应可读 status=%d body=%s err=%v", status, body, err)
	}
	if nodeDetail.Node.LifecycleState != domain.EntityLifecycleDecommissioned || len(nodeDetail.Edges) == 0 {
		t.Fatalf("下线后实体仍应可解析且保留依赖边: %+v", nodeDetail)
	}

	fromUpstream := blastRadiusIDs(t, upstreamID)
	if _, ok := fromUpstream[midID]; ok {
		t.Fatalf("爆炸半径不得再含下线实体: %+v", fromUpstream)
	}
	if _, ok := fromUpstream[downstreamID]; ok {
		t.Fatalf("爆炸半径不得再含下线实体的下游: %+v", fromUpstream)
	}
	if fromUpstream[upstreamID] != 0 || fromUpstream[bypassID] != 1 || len(fromUpstream) != 2 {
		t.Fatalf("active 旁路应仍可达: %+v", fromUpstream)
	}
	fromMid := blastRadiusIDs(t, midID)
	if len(fromMid) != 1 || fromMid[midID] != 0 {
		t.Fatalf("从下线根出发应只含自身: %+v", fromMid)
	}
}

type impactItem struct {
	Impact domain.FaultImpact  `json:"impact"`
	Node   domain.ResourceNode `json:"node"`
}

func blastRadiusIDs(t *testing.T, rootID string) map[string]int {
	t.Helper()
	got := map[string]int{}
	err := sys.Store.WithTx(context.Background(), func(tx pgx.Tx) error {
		radius, err := sys.Topology.ListBlastRadiusByNodeID(context.Background(), tx, rootID, 8, 24*time.Hour)
		if err != nil {
			return err
		}
		for _, item := range radius {
			got[item.Node.ID] = item.Depth
		}
		return nil
	})
	if err != nil {
		t.Fatalf("查询爆炸半径失败: %v", err)
	}
	return got
}

func faultTimelineHasAction(t *testing.T, faultID, action string) bool {
	t.Helper()
	var n int
	if err := sys.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM fault_timeline_entries WHERE fault_id = $1 AND action = $2`,
		faultID, action,
	).Scan(&n); err != nil {
		t.Fatalf("查询 Fault 时间线失败: %v", err)
	}
	return n > 0
}
