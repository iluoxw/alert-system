package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/app"
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
		return countFaultDeliveries(t, providerFaultID, "sent") == 1
	}, "上游渠道接受成功后才允许相同目标覆盖下游")

	consumerAlert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, "ConsumerSymptom", "consumer-"+suffix, "critical", "consumer-"+suffix)
	consumerFaultID := mappingFaultID(t, client, consumerAlert.ID)
	if countFaultNotifyTimers(t, consumerFaultID) != 0 {
		t.Fatalf("上游已通知时下游不得单发, timers=%d", countFaultNotifyTimers(t, consumerFaultID))
	}
	if countFaultDeliveries(t, consumerFaultID, "covered") != 1 {
		t.Fatal("相同目标必须持久记录covered决策")
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
		`SELECT count(*) FROM timers WHERE kind = 'notify_send' AND payload->>'GroupKey'=$1 AND COALESCE((payload->>'meta_alert')::boolean,false)=false`,
		"fault:"+faultID+":activated",
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

func countFaultDeliveries(t *testing.T, faultID, status string) int {
	t.Helper()
	var n int
	if e := sys.Store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1 AND status=$2`, faultID, status).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// TestFaultNotify_SilentReprocessPreviewAndBackfill 通过真实HTTP完成N4：
// 缺规则→补规则静默回放→只读预览→显式提交→幂等提交及送达审计。
func TestFaultNotify_SilentReprocessPreviewAndBackfill(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	name := "fault-backfill-" + suffix
	teamID := createNotifyTeam(t, client, name)
	entity := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/fault-backfill/" + suffix, "layer": "platform", "name": "Backfill Cluster", "provider": "test", "environment": "prod", "owner_team_id": teamID,
	}, http.StatusCreated)
	entityID, _ := entity["id"].(string)
	source := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "fault-backfill-source-" + suffix, "default_severity": "warning", "enabled": true,
		"config": map[string]any{"reconcile_interval_seconds": 3600, "stale_after_seconds": 7200, "missing_confirmations": 2},
	}, http.StatusCreated)
	sourceID, _ := source["id"].(string)
	webhookPath, _ := source["webhook_path"].(string)
	alertname := "LateFaultMapping" + suffix
	alert := postAuthorityMappingAlert(t, client, sourceID, webhookPath, alertname, "late-"+suffix, "critical", "late-"+suffix)
	assertMappingStatus(t, client, alert.ID, "unmapped", "no_policy")
	createFixedTargetMappingPolicy(t, client, 30200, "late-map-"+suffix, alertname, entityID, "backfill_probe")
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/admin/alert-mapping/results/"+alert.ID+"/reprocess", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("静默回放 status=%d body=%s err=%v", status, body, err)
	}
	faultID := mappingFaultID(t, client, alert.ID)
	snapshot := func() [4]int {
		t.Helper()
		var n [4]int
		if e := sys.Store.Pool.QueryRow(context.Background(), `SELECT
   (SELECT count(*) FROM fault_notification_watches WHERE fault_id=$1),
   (SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1),
   (SELECT count(*) FROM fault_timeline_entries WHERE fault_id=$1),
   (SELECT count(*) FROM timers WHERE kind='notify_send' AND payload->>'GroupKey'=$2)`, faultID, "fault:"+faultID+":activated").Scan(&n[0], &n[1], &n[2], &n[3]); e != nil {
			t.Fatal(e)
		}
		return n
	}
	before := snapshot()
	if before[0] != 0 || before[1] != 0 || before[3] != 0 {
		t.Fatalf("静默回放产生投递副作用: %v", before)
	}
	var ownerNotified bool
	if e := sys.Store.Pool.QueryRow(context.Background(), `SELECT owner_notified_at IS NOT NULL FROM faults WHERE id=$1`, faultID).Scan(&ownerNotified); e != nil {
		t.Fatal(e)
	}
	if ownerNotified {
		t.Fatal("回放不能成为渠道接受证明")
	}
	base := "/api/v1/admin/faults/" + faultID + "/notifications"
	secretTarget := "chat:" + name
	assertMasked := func(body []byte) {
		t.Helper()
		if strings.Contains(string(body), secretTarget) {
			t.Fatal("管理响应泄露目标")
		}
	}
	for _, endpoint := range []struct{ method, path string }{{http.MethodPost, base + "/preview"}, {http.MethodGet, base}} {
		var p app.FaultNotificationPreview
		status, body, err = client.doJSON(endpoint.method, endpoint.path, nil, &p)
		if err != nil || status != http.StatusOK || !p.Eligible || p.Reason != "ready" || len(p.Targets) != 1 || p.Targets[0].Action != "queue" {
			t.Fatalf("预览 status=%d body=%s err=%v", status, body, err)
		}
		assertMasked(body)
		if after := snapshot(); after != before {
			t.Fatalf("预览写入数据库 before=%v after=%v", before, after)
		}
	}
	var submitted app.FaultNotificationPreview
	status, body, err = client.doJSON(http.MethodPost, base+"/submit", nil, &submitted)
	if err != nil || status != http.StatusOK || len(submitted.Deliveries) != 1 {
		t.Fatalf("补投 status=%d body=%s err=%v", status, body, err)
	}
	assertMasked(body)
	deliveryID := submitted.Deliveries[0].ID
	waitFor(t, 8*time.Second, 100*time.Millisecond, func() bool { return countFaultDeliveries(t, faultID, "sent") == 1 }, "补投经fake渠道接受并持久记录sent")
	for i := 0; i < 2; i++ {
		status, body, err = client.doJSON(http.MethodPost, base+"/submit", nil, &submitted)
		if err != nil || status != http.StatusOK || len(submitted.Deliveries) != 1 || submitted.Deliveries[0].ID != deliveryID || submitted.Deliveries[0].Status != "sent" {
			t.Fatalf("重复补投不是幂等: status=%d body=%s err=%v", status, body, err)
		}
		assertMasked(body)
	}
	if countFaultNotifyTimers(t, faultID) != 1 {
		t.Fatal("重复补投新增timer")
	}
	var audits int
	var auditText string
	if e := sys.Store.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE action='notify_backfill'),COALESCE(jsonb_agg(detail)::text,'[]') FROM fault_timeline_entries WHERE fault_id=$1`, faultID).Scan(&audits, &auditText); e != nil {
		t.Fatal(e)
	}
	if audits != 1 || strings.Contains(auditText, secretTarget) {
		t.Fatalf("补投审计应幂等且脱敏: count=%d", audits)
	}
	var acknowledged bool
	if e := sys.Store.Pool.QueryRow(context.Background(), `SELECT owner_notified_at IS NOT NULL,acked_at IS NOT NULL FROM faults WHERE id=$1`, faultID).Scan(&ownerNotified, &acknowledged); e != nil {
		t.Fatal(e)
	}
	if !ownerNotified || acknowledged {
		t.Fatal("渠道接受与人工ACK语义混淆")
	}
	sends := 0
	for _, send := range mockLark.Sent() {
		if send.Msg.ResourceID == faultID {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("补投物理发送次数=%d,want1", sends)
	}
}
