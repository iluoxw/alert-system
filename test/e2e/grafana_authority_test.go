package e2e

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGrafanaAuthority_FullChain 是任务 26 的 P3 验收闸门：Grafana webhook 快路径
// 创建权威 occurrence；对账走 Grafana Alertmanager 兼容路径；看不到 ≠ resolved；
// 本地 Resolve 拒绝；Fault 映射生效；权威 Alert 不进本地通知 outbox。
func TestGrafanaAuthority_FullChain(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()
	startsAt := time.Now().UTC().Truncate(time.Second)
	fingerprint := "grafana-e2e-" + suffix
	mock := &alertmanagerMock{
		mode: "error", startsAt: startsAt, updatedAt: startsAt.Add(time.Minute), fp: fingerprint,
		alertsPath: "/api/alertmanager/grafana/api/v2/alerts",
		alertname:  "GrafanaAuthorityProbe",
		labelKey:   "grafana_e2e",
	}
	upstream := httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	defer upstream.Close()

	entity := adminCreate(t, client, "/api/v1/admin/entities", map[string]any{
		"entity_key": "platform/grafana-authority/" + suffix,
		"layer":      "platform", "name": "Grafana Authority Cluster", "provider": "test",
		"environment": "prod", "owner_team_id": seedResult.TeamID,
	}, http.StatusCreated)
	entityID, _ := entity["id"].(string)
	createFixedTargetMappingPolicy(t, client, 31000, "grafana-authority-"+suffix, "GrafanaAuthorityProbe", entityID, "grafana_authority_probe")

	created := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind":             "grafana",
		"name":             "grafana-authority-source-" + suffix,
		"default_severity": "warning",
		"enabled":          true,
		"config": map[string]any{
			"api_url": upstream.URL, "reconcile_interval_seconds": 3600,
			"stale_after_seconds": 7200, "missing_confirmations": 2,
		},
	}, http.StatusCreated)
	sourceID, _ := created["id"].(string)
	webhookPath, _ := created["webhook_path"].(string)
	if sourceID == "" || webhookPath == "" {
		t.Fatalf("创建 Grafana source 响应缺少 id/webhook_path: %+v", created)
	}

	payload := mustJSON(map[string]any{
		"version": "1", "status": "firing", "receiver": "e2e",
		"orgId": 1, "title": "[FIRING:1] GrafanaAuthorityProbe", "state": "alerting",
		"alerts": []map[string]any{{
			"status": "firing",
			"labels": map[string]string{
				"alertname": "GrafanaAuthorityProbe", "severity": "critical", "service": "checkout",
				"grafana_e2e": fingerprint,
			},
			"annotations": map[string]string{"summary": "Grafana 权威链路探针"},
			"startsAt":    startsAt.Format(time.RFC3339Nano), "endsAt": time.Time{}.Format(time.RFC3339Nano),
			"fingerprint": fingerprint,
		}},
	})
	status, body, err := client.postManagedHook(webhookPath, payload)
	if err != nil {
		t.Fatalf("投递 Grafana webhook 失败: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("Grafana webhook status=%d body=%s, want 202", status, body)
	}

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "grafana_e2e", fingerprint)
		if err != nil || !ok {
			return false
		}
		alert = a
		return a.Authoritative && a.SourceState == "firing" && a.ResponseState == "unacknowledged" && a.UpstreamFingerprint == fingerprint
	}, "Grafana webhook 镜像为权威 occurrence")
	if alert.SourceIntegrationID != sourceID || alert.SuppressionState != "none" || alert.GroupKey != "" || alert.TeamID != "" {
		t.Fatalf("Grafana 权威 Alert 边界异常: %+v", alert)
	}
	assertNoLocalNotification(t, alert.ID)
	if mappingFaultID(t, client, alert.ID) == "" {
		t.Fatal("Grafana occurrence 应映射出 Fault")
	}

	status, body, err = client.doJSON(http.MethodPost, "/api/v1/alerts/"+alert.ID+"/ack", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("Ack Grafana Alert 失败 status=%d body=%s err=%v", status, body, err)
	}
	status, body, err = client.doJSON(http.MethodPost, "/api/v1/alerts/"+alert.ID+"/resolve", nil, nil)
	if err != nil {
		t.Fatalf("Resolve Grafana Alert 请求失败: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("本地 Resolve Grafana Alert status=%d body=%s, want 409", status, body)
	}

	mock.setMode("suppressed", startsAt.Add(2*time.Minute))
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "firing", "acknowledged", "silenced", 0)

	mock.setMode("error", time.Time{})
	status, _, err = client.doJSON(http.MethodPost, "/api/v1/admin/alert-sources/"+sourceID+"/reconcile", nil, nil)
	if err != nil {
		t.Fatalf("触发失败对账请求异常: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("上游 503 时 reconcile status=%d, want 502", status)
	}
	assertAlertAxesEventually(t, client, alert.ID, "firing", "acknowledged", "silenced", 0)

	mock.setMode("empty", time.Time{})
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "unknown", "acknowledged", "silenced", 1)
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "resolved", "acknowledged", "silenced", 2)
	assertNoLocalNotification(t, alert.ID)
}
