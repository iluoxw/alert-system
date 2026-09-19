package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// alertDTO 是 GET /api/v1/alerts、/api/v1/alerts/:id 响应里 alert 部分用得到的字段
// 子集（internal/api/alerts.go 的 alertResponse），测试代码只关心场景断言需要的这几个。
type alertDTO struct {
	ID                  string   `json:"id"`
	Status              string   `json:"status"`
	SourceState         string   `json:"source_state"`
	ResponseState       string   `json:"response_state"`
	SuppressionState    string   `json:"suppression_state"`
	Authoritative       bool     `json:"authoritative"`
	UpstreamFingerprint string   `json:"upstream_fingerprint"`
	SourceIntegrationID string   `json:"source_integration_id"`
	ReconcileMissCount  int      `json:"reconcile_miss_count"`
	Severity            string   `json:"severity"`
	GroupKey            string   `json:"group_key"`
	TeamID              string   `json:"team_id"`
	SilencedBy          []string `json:"silenced_by"`
	DedupCount          int      `json:"dedup_count"`
}

type alertListResponse struct {
	Alerts []alertDTO `json:"alerts"`
	Total  int        `json:"total"`
}

type alertDetailResponse struct {
	Alert alertDTO `json:"alert"`
}

// findAlertByLabel 经 GET /api/v1/alerts?label[key]=value 查找唯一命中的告警，
// found=false 表示尚未入库（供 waitFor 轮询）。
func findAlertByLabel(client *apiClient, key, value string) (alertDTO, bool, error) {
	path := fmt.Sprintf("/api/v1/alerts?label[%s]=%s", key, url.QueryEscape(value))
	var resp alertListResponse
	status, body, err := client.doJSON(http.MethodGet, path, nil, &resp)
	if err != nil {
		return alertDTO{}, false, err
	}
	if status != http.StatusOK {
		return alertDTO{}, false, fmt.Errorf("e2e: 查询告警列表失败 status=%d body=%s", status, body)
	}
	if len(resp.Alerts) == 0 {
		return alertDTO{}, false, nil
	}
	return resp.Alerts[0], true, nil
}

// getAlert 经 GET /api/v1/alerts/:id 取告警当前状态（比 findAlertByLabel 更精确——
// 静默/repeat 恢复等场景需要反复查询同一条已知 ID 的告警最新状态）。
func getAlert(client *apiClient, id string) (alertDTO, error) {
	var resp alertDetailResponse
	status, body, err := client.doJSON(http.MethodGet, "/api/v1/alerts/"+id, nil, &resp)
	if err != nil {
		return alertDTO{}, err
	}
	if status != http.StatusOK {
		return alertDTO{}, fmt.Errorf("e2e: 查询告警详情失败 status=%d body=%s", status, body)
	}
	return resp.Alert, nil
}

// waitFor 轮询 cond 直到为 true 或超时，超时调用 t.Fatalf。用于用秒级配置压缩后的
// 时钟等待场景断言（不 mock 时钟本身，只是真实等待更短的时长）。
func waitFor(t *testing.T, timeout, interval time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("e2e: 等待超时（%s）：%s", timeout, what)
		}
		time.Sleep(interval)
	}
}

// containsString 报告 ss 中是否存在等于 s 的元素。
func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// countSentForAlert 统计 mock 渠道当前已记录的、AlertIDs 包含 alertID 的发送条数——
// 用计数而不是"存在与否"断言，是场景②（静默恢复）区分"静默前那一条"与"恢复后
// 新发的那一条"的关键：Bug C 修复前二者会被幂等键判定成同一次发送，计数会停在 1
// 不再增长。
func countSentForAlert(ch *mockChannel, alertID string) int {
	count := 0
	for _, s := range ch.Sent() {
		if containsString(s.Msg.AlertIDs, alertID) {
			count++
		}
	}
	return count
}

// suffixCounter 与调用时刻的纳秒时间戳一起拼出跨场景唯一的 alertname/rule_id，
// 避免高频调用时纳秒时间戳恰好重复。
var suffixCounter int64

func uniqueSuffix() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddInt64(&suffixCounter, 1))
}

// compatibleWebhookPayload 构造本地编排场景用的 generic 信封。Grafana 已是权威来源
// （必须带 fingerprint，且不经过本地 route/group/silence），这些场景改走 generic。
func compatibleWebhookPayload(alertname, severity, status string, startsAt time.Time, extraLabels map[string]string) []byte {
	labels := map[string]string{"alertname": alertname, "severity": severity}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return genericAlertPayload(alertname, severity, status, "source", 0, startsAt, labels)
}

// genericAlertPayload 构造一条 kind=generic 的 ingest 请求体：唯一能显式指定
// resolve_policy=ttl 的 adapter（AlertmanagerAdapter.translate 硬编码
// ResolvePolicySource，无法产出 TTL 告警，见任务报告）。
func genericAlertPayload(ruleID, severity, status, resolvePolicy string, ttlSeconds int, startsAt time.Time, labels map[string]string) []byte {
	body := map[string]any{
		"rule_id":        ruleID,
		"severity":       severity,
		"status":         status,
		"title":          ruleID + " 测试告警",
		"labels":         labels,
		"resolve_policy": resolvePolicy,
		"starts_at":      startsAt.Format(time.RFC3339Nano),
	}
	if ttlSeconds > 0 {
		body["ttl_seconds"] = ttlSeconds
	}
	return mustJSON(body)
}

// pendingTimerCount 直接查 timers 表统计给定 kinds+refID 当前处于 pending 的定时器
// 数量——比"一段时间内没收到通知"更确定的验证方式：Ack 之后升级定时器应当被同步
// Cancel，不需要等到它们本会触发的时刻才能断言。
func pendingTimerCount(t *testing.T, kinds []string, refID string) int {
	t.Helper()
	var count int
	err := sys.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM timers WHERE kind = ANY($1) AND ref_id = $2 AND status = 'pending'`,
		kinds, refID).Scan(&count)
	if err != nil {
		t.Fatalf("e2e: 查询 timers 表失败: %v", err)
	}
	return count
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("e2e: 序列化测试 payload 失败: %v", err))
	}
	return b
}

// adminCreate 是任务 16 旅程场景专用的 POST /api/v1/admin/... helper：状态码非
// wantStatus 直接 t.Fatalf，成功时把响应体解到 map 供调用方取 id 等字段。既有五场景
// 各自只调用一两次 admin 创建端点，直接内联 client.doJSON+状态检查即可；旅程场景要
// 连续创建 team/user/member/schedule/shifts-generate/route/escalation-policy 七种
// 资源，抽出这个 helper 避免七次几乎相同的样板代码。
func adminCreate(t *testing.T, client *apiClient, path string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	var resp map[string]any
	status, raw, err := client.doJSON(http.MethodPost, path, body, &resp)
	if err != nil {
		t.Fatalf("e2e: POST %s 失败: %v", path, err)
	}
	if status != wantStatus {
		t.Fatalf("e2e: POST %s 响应非 %d，status=%d body=%s", path, wantStatus, status, raw)
	}
	return resp
}

// notificationLogSent 直接查 notification_log 表（只读断言，不写库）：返回指定
// alert_id + channel 下 status=sent 记录的 target 与 idempotency_key。这条记录由
// notify.Sender.Send 在外部 IO 成功后回写，比"mock 渠道收到过调用"多验证一层
// "发送状态确实落库为 sent"，是任务 16 断言链路里独立于 mockChannel 的第二重证据。
func notificationLogSent(t *testing.T, alertID, channel string) (target, idempotencyKey string, found bool) {
	t.Helper()
	err := sys.Store.Pool.QueryRow(context.Background(),
		`SELECT target, idempotency_key FROM notification_log WHERE alert_id = $1 AND channel = $2 AND status = 'sent' LIMIT 1`,
		alertID, channel).Scan(&target, &idempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("e2e: 查询 notification_log 失败: %v", err)
	}
	return target, idempotencyKey, true
}

// timelineHasNotifySentToEmail 查 timeline_entries 表（只读断言）：是否存在给定
// 告警的 notify_sent 条目，且 detail.target 呈邮箱形态（含 "@"）。时间线里的 target
// 经 notify.maskTarget 脱敏（如 a***@example.com），这里只需确认"看得出是邮箱"，
// 不要求逐字节匹配真实地址——脱敏正是 R6.4 要求的行为，不是缺陷。
func timelineHasNotifySentToEmail(t *testing.T, alertID string) bool {
	t.Helper()
	rows, err := sys.Store.Pool.Query(context.Background(),
		`SELECT detail->>'target' FROM timeline_entries WHERE alert_id = $1 AND action = 'notify_sent'`,
		alertID)
	if err != nil {
		t.Fatalf("e2e: 查询 timeline_entries 失败: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatalf("e2e: 扫描 timeline_entries 失败: %v", err)
		}
		if strings.Contains(target, "@") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("e2e: 遍历 timeline_entries 失败: %v", err)
	}
	return found
}
