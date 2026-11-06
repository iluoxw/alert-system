package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// TestFaultNotify_WebhookAckCooldownReconcile 覆盖 P2 验收闸门（任务 21）：
// webhook → Fault active → 恰好一条 notify_send → REST ack 幂等 → 冷却窗口不重发
// → unknown→firing 对账不重发 → reprocess 不产生新意图。
func TestFaultNotify_WebhookAckCooldownReconcile(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	teamID := createNotifyTeam(t, client, "fault-notify-"+suffix)
	entity := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/fault-notify/" + suffix,
		"layer":      "platform", "name": "Fault Notify Cluster", "provider": "test",
		"environment": "prod", "owner_team_id": teamID,
	}, http.StatusCreated)
	entityID, _ := entity["id"].(string)

	source := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "fault-notify-source-" + suffix,
		"default_severity": "warning", "enabled": true,
		"config": map[string]any{"reconcile_interval_seconds": 3600, "stale_after_seconds": 7200, "missing_confirmations": 2},
	}, http.StatusCreated)
	sourceID, _ := source["id"].(string)
	webhookPath, _ := source["webhook_path"].(string)

	createFixedTargetMappingPolicy(t, client, 30000, "fault-notify-"+suffix, "FaultNotifyProbe", entityID, "notify_probe")

	startsAt := time.Now().UTC().Truncate(time.Second)
	fingerprint := "fault-notify-" + suffix
	alert := postAuthorityMappingOccurrence(t, client, sourceID, webhookPath, "firing", "FaultNotifyProbe", "svc-"+suffix, "critical", fingerprint, startsAt)
	faultID := mappingFaultID(t, client, alert.ID)

	waitFor(t, 8*time.Second, 100*time.Millisecond, func() bool {
		return countFaultNotifyTimers(t, faultID) == 1
	}, "Fault 进入 active 恰好一条 notify_send")

	var ack struct {
		Fault domain.Fault `json:"fault"`
	}
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/faults/"+faultID+"/ack", nil, &ack)
	if err != nil || status != http.StatusOK || !ack.Fault.IsAcked() {
		t.Fatalf("首次 ack status=%d body=%s err=%v fault=%+v", status, body, err, ack.Fault)
	}
	status, body, err = client.doJSON(http.MethodPost, "/api/v1/faults/"+faultID+"/ack", nil, &ack)
	if err != nil || status != http.StatusOK {
		t.Fatalf("重复 ack status=%d body=%s err=%v", status, body, err)
	}
	if ack.Fault.AckedByName == "" {
		t.Fatalf("重复 ack 应返回当前认领状态: %+v", ack.Fault)
	}

	snapshotEmpty := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if snapshotEmpty {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"annotations": map[string]string{"summary": "Fault notify probe"},
			"fingerprint": fingerprint,
			"startsAt":    startsAt.Format(time.RFC3339Nano),
			"updatedAt":   startsAt.Add(2 * time.Minute).Format(time.RFC3339Nano),
			"endsAt":      time.Time{}.Format(time.RFC3339Nano),
			"labels": map[string]string{
				"alertname": "FaultNotifyProbe", "severity": "critical",
				"service": "svc-" + suffix, "cluster": "prod-a", "mapping_e2e": fingerprint,
			},
			"status": map[string]any{"state": "active", "silencedBy": []string{}, "inhibitedBy": []string{}, "mutedBy": []string{}},
		}})
	}))
	defer upstream.Close()
	status, body, err = client.doJSON(http.MethodPatch, "/api/v1/admin/integrations/"+sourceID, map[string]any{
		"kind": "alertmanager", "name": "fault-notify-source-" + suffix,
		"default_severity": "warning", "enabled": true,
		"config": map[string]any{
			"api_url": upstream.URL, "reconcile_interval_seconds": 3600,
			"stale_after_seconds": 7200, "missing_confirmations": 2,
		},
	}, nil)
	if err != nil || (status != http.StatusOK && status != http.StatusNoContent) {
		t.Fatalf("更新 source api_url 失败 status=%d body=%s err=%v", status, body, err)
	}
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "unknown", "unacknowledged", "none", 1)
	snapshotEmpty = false
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "firing", "unacknowledged", "none", 0)
	if countFaultNotifyTimers(t, faultID) != 1 {
		t.Fatalf("unknown→firing 对账不得新增通知意图, timers=%d", countFaultNotifyTimers(t, faultID))
	}

	before := countFaultNotifyTimers(t, faultID)
	status, body, err = client.doJSON(http.MethodPost, "/api/v1/admin/alert-mapping/results/"+alert.ID+"/reprocess", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reprocess status=%d body=%s err=%v", status, body, err)
	}
	if countFaultNotifyTimers(t, faultID) != before {
		t.Fatalf("reprocess 不得产生新的 notify_send, before=%d after=%d", before, countFaultNotifyTimers(t, faultID))
	}

	resolveFault(t, faultID)
	replay := postAuthorityMappingOccurrence(t, client, sourceID, webhookPath, "firing", "FaultNotifyProbe", "svc-"+suffix, "critical", fingerprint+"-replay", startsAt.Add(time.Minute))
	replayFaultID := mappingFaultID(t, client, replay.ID)
	if countFaultNotifyTimers(t, replayFaultID) != 0 {
		t.Fatalf("冷却窗口内复发不应再调度 notify_send, timers=%d", countFaultNotifyTimers(t, replayFaultID))
	}
}

func TestFaultNotify_TopologySuppression(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	teamID := createNotifyTeam(t, client, "fault-suppress-"+suffix)
	provider := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/fault-provider/" + suffix,
		"layer":      "platform", "name": "Provider Cluster", "provider": "test",
		"environment": "prod", "owner_team_id": teamID,
	}, http.StatusCreated)
	providerID, _ := provider["id"].(string)
	consumer := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/fault-consumer/" + suffix,
		"layer":      "platform", "name": "Consumer Cluster", "provider": "test",
		"environment": "prod", "owner_team_id": teamID,
		"dependencies": []map[string]any{{"to_entity_id": providerID, "criticality": "critical"}},
	}, http.StatusCreated)
	consumerID, _ := consumer["id"].(string)

	source := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "fault-suppress-source-" + suffix,
		"default_severity": "warning", "enabled": true,
		"config": map[string]any{"reconcile_interval_seconds": 3600, "stale_after_seconds": 7200, "missing_confirmations": 2},
	}, http.StatusCreated)
	sourceID, _ := source["id"].(string)
	webhookPath, _ := source["webhook_path"].(string)
	createFixedTargetMappingPolicy(t, client, 30100, "fault-provider-"+suffix, "ProviderDown", providerID, "provider_down")
	createFixedTargetMappingPolicy(t, client, 30101, "fault-consumer-"+suffix, "ConsumerSymptom", consumerID, "consumer_symptom")

	providerAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "ProviderDown", "provider-"+suffix, "critical", "provider-"+suffix)
	providerFaultID := mappingFaultID(t, client, providerAlert.ID)
	waitFor(t, 8*time.Second, 100*time.Millisecond, func() bool {
		return countFaultNotifyTimers(t, providerFaultID) == 1
	}, "上游 Fault 通知")

	consumerAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "ConsumerSymptom", "consumer-"+suffix, "critical", "consumer-"+suffix)
	consumerFaultID := mappingFaultID(t, client, consumerAlert.ID)
	if countFaultNotifyTimers(t, consumerFaultID) != 0 {
		t.Fatalf("上游已通知时下游不得单发, timers=%d", countFaultNotifyTimers(t, consumerFaultID))
	}
}

func TestFaultsAck_Unauthenticated(t *testing.T) {
	client, err := newAPIClient(sys.Server.URL)
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := client.doJSON(http.MethodPost, "/api/v1/faults/00000000-0000-0000-0000-000000000001/ack", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", status)
	}
}

func createNotifyTeam(t *testing.T, client *apiClient, name string) string {
	t.Helper()
	team := adminCreate(t, client, "/api/v1/admin/teams", map[string]any{"name": name}, http.StatusCreated)
	teamID, _ := team["id"].(string)
	if teamID == "" {
		t.Fatalf("创建团队失败: %+v", team)
	}
	status, body, err := client.doJSON(http.MethodPut, "/api/v1/admin/teams/"+teamID+"/ops-settings", map[string]any{
		"timezone":             "Asia/Shanghai",
		"daily_report_enabled": false, "daily_report_time": "09:00",
		"stale_sweep_enabled": false, "stale_sweep_interval_seconds": 1800,
		"oncall_broadcast_enabled": false, "oncall_broadcast_time": "09:30",
		"channels": []map[string]string{{"kind": "lark", "target": "chat:" + name}},
	}, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("配置团队通知渠道失败 status=%d body=%s err=%v", status, body, err)
	}
	return teamID
}

func countFaultNotifyTimers(t *testing.T, faultID string) int {
	t.Helper()
	var n int
	if err := sys.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM timers WHERE kind = 'notify_send' AND ref_id LIKE $1`,
		"fault:"+faultID+":activated%",
	).Scan(&n); err != nil {
		t.Fatalf("查询 Fault notify_send 失败: %v", err)
	}
	return n
}

func resolveFault(t *testing.T, faultID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := sys.Store.WithTx(context.Background(), func(tx pgx.Tx) error {
		if _, err := sys.Faults.UpdateLifecycleStatus(context.Background(), tx, faultID, domain.FaultStatusActive, domain.FaultStatusMonitoring, now); err != nil {
			return err
		}
		_, err := sys.Faults.UpdateLifecycleStatus(context.Background(), tx, faultID, domain.FaultStatusMonitoring, domain.FaultStatusResolved, now)
		return err
	}); err != nil {
		t.Fatalf("推进 Fault 到 resolved 失败: %v", err)
	}
}
