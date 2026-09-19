package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type alertmanagerMock struct {
	mu         sync.RWMutex
	mode       string
	startsAt   time.Time
	updatedAt  time.Time
	fp         string
	alertsPath string
	alertname  string
	labelKey   string
}

func (m *alertmanagerMock) setMode(mode string, updatedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
	if !updatedAt.IsZero() {
		m.updatedAt = updatedAt
	}
}

func (m *alertmanagerMock) serveHTTP(w http.ResponseWriter, r *http.Request) {
	alertsPath := m.alertsPath
	if alertsPath == "" {
		alertsPath = "/api/v2/alerts"
	}
	if r.URL.Path != alertsPath {
		http.NotFound(w, r)
		return
	}
	m.mu.RLock()
	mode, startsAt, updatedAt, fp := m.mode, m.startsAt, m.updatedAt, m.fp
	m.mu.RUnlock()
	if mode == "error" {
		http.Error(w, "mock upstream unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if mode == "empty" {
		_, _ = w.Write([]byte("[]"))
		return
	}
	status := map[string]any{
		"state": "active", "silencedBy": []string{}, "inhibitedBy": []string{}, "mutedBy": []string{},
	}
	if mode == "suppressed" {
		status["state"] = "suppressed"
		status["silencedBy"] = []string{"e2e-silence-01"}
	}
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"annotations":  map[string]string{"summary": "Alertmanager 权威链路探针"},
		"receivers":    []map[string]string{{"name": "e2e"}},
		"fingerprint":  fp,
		"startsAt":     startsAt.Format(time.RFC3339Nano),
		"updatedAt":    updatedAt.Format(time.RFC3339Nano),
		"endsAt":       time.Time{}.Format(time.RFC3339Nano),
		"generatorURL": "http://prometheus.test/graph",
		"labels": map[string]string{
			"alertname": firstNonEmpty(m.alertname, "SREControlAuthorityProbe"), "severity": "critical", "service": "cch",
			firstNonEmpty(m.labelKey, "authority_e2e"): fp,
		},
		"status": status,
	}})
}

// TestAlertmanagerAuthority_FullChain 覆盖生产形态的完整边界：webhook 快路径创建
// occurrence；API 慢路径补齐抑制事实；Ack 只改响应轴；本地 Resolve 被拒绝；503 不
// 能据缺失误恢复；连续两次完整空快照才完成 unknown -> resolved。全程还断言权威
// Alert 不进入本地通知 outbox。
func TestAlertmanagerAuthority_FullChain(t *testing.T) {
	client := newLoggedInClient(t)
	startsAt := time.Now().UTC().Truncate(time.Second)
	fingerprint := "authority-e2e-" + uniqueSuffix()
	mock := &alertmanagerMock{
		mode: "error", startsAt: startsAt, updatedAt: startsAt.Add(time.Minute), fp: fingerprint,
	}
	upstream := httptest.NewServer(http.HandlerFunc(mock.serveHTTP))
	defer upstream.Close()

	created := adminCreate(t, client, "/api/v1/admin/integrations", map[string]any{
		"kind":             "alertmanager",
		"name":             "authority-source-" + uniqueSuffix(),
		"default_severity": "warning",
		"config": map[string]any{
			"api_url": upstream.URL, "reconcile_interval_seconds": 3600,
			"stale_after_seconds": 7200, "missing_confirmations": 2,
		},
	}, http.StatusCreated)
	sourceID, _ := created["id"].(string)
	oldWebhookPath, _ := created["webhook_path"].(string)
	if sourceID == "" || oldWebhookPath == "" {
		t.Fatalf("创建 Alertmanager source 响应缺少 id/webhook_path: %+v", created)
	}
	if _, exists := created["token"]; exists {
		t.Fatalf("创建响应不应包含旧 token 字段: %+v", created)
	}

	var rotated map[string]any
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/admin/alert-sources/"+sourceID+"/rotate-hook", nil, &rotated)
	if err != nil || status != http.StatusOK {
		t.Fatalf("轮换 managed hook 失败 status=%d body=%s err=%v", status, body, err)
	}
	webhookPath, _ := rotated["webhook_path"].(string)
	if webhookPath == "" || webhookPath == oldWebhookPath || rotated["id"] != sourceID {
		t.Fatalf("轮换结果未保持 source 身份或未替换 hook: old=%q rotated=%+v", oldWebhookPath, rotated)
	}
	if _, exists := rotated["token"]; exists {
		t.Fatalf("轮换响应不应包含旧 token 字段: %+v", rotated)
	}

	payload := mustJSON(map[string]any{
		"version": "4", "status": "firing", "receiver": "e2e",
		"alerts": []map[string]any{{
			"status": "firing",
			"labels": map[string]string{
				"alertname": "SREControlAuthorityProbe", "severity": "critical", "service": "cch",
				"authority_e2e": fingerprint,
			},
			"annotations": map[string]string{"summary": "Alertmanager 权威链路探针"},
			"startsAt":    startsAt.Format(time.RFC3339Nano), "endsAt": time.Time{}.Format(time.RFC3339Nano),
			"fingerprint": fingerprint,
		}},
	})
	status, body, err = client.doJSON(http.MethodPost, "/ingest/"+sourceID, json.RawMessage(payload), nil)
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("旧 source-id ingest 路由应不存在 status=%d body=%s err=%v", status, body, err)
	}
	status, body, err = client.postManagedHook(oldWebhookPath, payload)
	if err != nil || status != http.StatusUnauthorized {
		t.Fatalf("旧 managed hook 轮换后仍可用 status=%d body=%s err=%v", status, body, err)
	}

	status, body, err = client.postManagedHook(webhookPath, payload)
	if err != nil {
		t.Fatalf("投递 Alertmanager webhook 失败: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("Alertmanager webhook status=%d body=%s, want 202", status, body)
	}

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "authority_e2e", fingerprint)
		if err != nil || !ok {
			return false
		}
		alert = a
		return a.Authoritative && a.SourceState == "firing" && a.ResponseState == "unacknowledged" && a.UpstreamFingerprint == fingerprint
	}, "Alertmanager webhook 镜像为权威 occurrence")
	if alert.SourceIntegrationID != sourceID || alert.SuppressionState != "none" || alert.GroupKey != "" || alert.TeamID != "" {
		t.Fatalf("权威 Alert 边界异常: %+v", alert)
	}
	assertNoLocalNotification(t, alert.ID)

	status, body, err = client.doJSON(http.MethodPost, "/api/v1/alerts/"+alert.ID+"/ack", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("Ack 权威 Alert 失败 status=%d body=%s err=%v", status, body, err)
	}
	assertAlertAxesEventually(t, client, alert.ID, "firing", "acknowledged", "none", 0)

	status, body, err = client.doJSON(http.MethodPost, "/api/v1/alerts/"+alert.ID+"/resolve", nil, nil)
	if err != nil {
		t.Fatalf("Resolve 权威 Alert 请求失败: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("本地 Resolve 权威 Alert status=%d body=%s, want 409", status, body)
	}
	assertAlertAxesEventually(t, client, alert.ID, "firing", "acknowledged", "none", 0)

	mock.setMode("suppressed", startsAt.Add(2*time.Minute))
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "firing", "acknowledged", "silenced", 0)

	// 读取失败必须保留上一次完整快照的事实，不能把“请求失败”解释成“当前为空”。
	mock.setMode("error", time.Time{})
	status, _, err = client.doJSON(http.MethodPost, "/api/v1/admin/alert-sources/"+sourceID+"/reconcile", nil, nil)
	if err != nil {
		t.Fatalf("触发失败对账请求异常: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("上游 503 时 reconcile status=%d, want 502", status)
	}
	assertAlertAxesEventually(t, client, alert.ID, "firing", "acknowledged", "silenced", 0)

	var source map[string]any
	status, body, err = client.doJSON(http.MethodGet, "/api/v1/admin/alert-sources/"+sourceID, nil, &source)
	if err != nil || status != http.StatusOK {
		t.Fatalf("读取 source 健康失败 status=%d body=%s err=%v", status, body, err)
	}
	if source["health"] != "degraded" {
		t.Fatalf("503 后 source health=%v, want degraded；source=%+v", source["health"], source)
	}

	mock.setMode("empty", time.Time{})
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "unknown", "acknowledged", "silenced", 1)
	reconcileSourceOK(t, client, sourceID)
	assertAlertAxesEventually(t, client, alert.ID, "resolved", "acknowledged", "silenced", 2)
	assertNoLocalNotification(t, alert.ID)

	status, body, err = client.doJSON(http.MethodGet, "/api/v1/admin/alert-sources/"+sourceID, nil, &source)
	if err != nil || status != http.StatusOK {
		t.Fatalf("读取恢复后的 source 健康失败 status=%d body=%s err=%v", status, body, err)
	}
	if source["health"] != "healthy" || source["firing_count"] != float64(0) || source["unknown_count"] != float64(0) {
		t.Fatalf("最终 source 统计异常: %+v", source)
	}
}

func reconcileSourceOK(t *testing.T, client *apiClient, sourceID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body, err := client.doJSON(http.MethodPost, "/api/v1/admin/alert-sources/"+sourceID+"/reconcile", nil, nil)
		if err != nil {
			t.Fatalf("触发 Alertmanager 对账失败: %v", err)
		}
		if status == http.StatusOK {
			return
		}
		if status != http.StatusBadGateway || time.Now().After(deadline) {
			t.Fatalf("Alertmanager 对账 status=%d body=%s, want 200", status, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertAlertAxesEventually(t *testing.T, client *apiClient, alertID, source, response, suppression string, misses int) {
	t.Helper()
	waitFor(t, 5*time.Second, 100*time.Millisecond, func() bool {
		alert, err := getAlert(client, alertID)
		return err == nil && alert.SourceState == source && alert.ResponseState == response &&
			alert.SuppressionState == suppression && alert.ReconcileMissCount == misses
	}, "Alert 三轴达到预期")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func assertNoLocalNotification(t *testing.T, alertID string) {
	t.Helper()
	var count int
	if err := sys.Store.Pool.QueryRow(t.Context(), `SELECT count(*) FROM notification_log WHERE alert_id = $1`, alertID).Scan(&count); err != nil {
		t.Fatalf("查询 notification_log 失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("Alertmanager 权威 Alert 产生了 %d 条本地通知记录，want 0", count)
	}
}
