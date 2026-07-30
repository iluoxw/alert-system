package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// alertDTO 是 GET /api/v1/alerts、/api/v1/alerts/:id 响应里 alert 部分用得到的字段
// 子集（internal/api/alerts.go 的 alertResponse），测试代码只关心场景断言需要的这几个。
type alertDTO struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Severity   string   `json:"severity"`
	GroupKey   string   `json:"group_key"`
	TeamID     string   `json:"team_id"`
	SilencedBy []string `json:"silenced_by"`
	DedupCount int      `json:"dedup_count"`
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

// alertmanagerPayload 构造一条 kind=alertmanager 的 webhook 请求体：单条告警，
// labels 固定带 alertname（路由/静默/分组的 group_by 均按 alertname 匹配）。
func alertmanagerPayload(alertname, severity, status string, startsAt time.Time, extraLabels map[string]string) []byte {
	labels := map[string]string{"alertname": alertname, "severity": severity}
	for k, v := range extraLabels {
		labels[k] = v
	}
	body := map[string]any{
		"version":  "4",
		"status":   status,
		"receiver": "e2e",
		"alerts": []map[string]any{
			{
				"status":      status,
				"labels":      labels,
				"annotations": map[string]string{"summary": alertname + " 测试告警"},
				"startsAt":    startsAt.Format(time.RFC3339Nano),
				"endsAt":      "0001-01-01T00:00:00Z",
			},
		},
	}
	return mustJSON(body)
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
