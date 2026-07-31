// Package e2e（本文件）：任务 16「从零到通知到人」旅程场景——全链路只经 API 走一遍
// R6 到人通知：建团队 -> 建用户（带 email）-> 加成员 -> 建值班表 -> 生成轮换班次
// （任务 6 的 shifts/generate 端点）-> 配路由 + 升级策略 -> ingest critical 告警 ->
// 断言 mock email 渠道收到的 target 是解析后的邮箱地址（而非 UUID）、
// notification_log 落 status=sent 且幂等键含该地址、时间线出现 notify_sent。
//
// 是任务 9（ContactResolver 把 TargetUserID 解析成真实渠道地址）与任务 6（HTTP 生成
// 轮换班次，此前只能直接调仓储）两条链路汇合之后的最终判据，走的是与既有五场景
// 同一套种子团队/管理员之外、独立新建的团队/用户/值班表/路由/升级策略，互不干扰。
package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestScenarioJourney_ZeroToPersonNotification(t *testing.T) {
	client := newLoggedInClient(t)
	suffix := uniqueSuffix()

	// ---- 建团队 ----
	teamResp := adminCreate(t, client, "/api/v1/admin/teams", map[string]any{
		"name": "e2e-journey-team-" + suffix,
	}, http.StatusCreated)
	teamID, _ := teamResp["id"].(string)
	if teamID == "" {
		t.Fatalf("e2e: 创建团队响应缺少 id: %+v", teamResp)
	}

	// ---- 建用户（含 email，到人发送最终解析出的就是这个地址）----
	email := "e2e-journey-" + suffix + "@example.com"
	userResp := adminCreate(t, client, "/api/v1/admin/users", map[string]any{
		"username":     "e2e-journey-user-" + suffix,
		"password":     "e2e-journey-pass-000",
		"display_name": "E2E Journey User",
		"email":        email,
	}, http.StatusCreated)
	userID, _ := userResp["id"].(string)
	if userID == "" {
		t.Fatalf("e2e: 创建用户响应缺少 id: %+v", userResp)
	}

	// ---- 加成员 ----
	adminCreate(t, client, "/api/v1/admin/teams/"+teamID+"/members", map[string]any{
		"user_id": userID,
		"role":    "member",
	}, http.StatusCreated)

	// ---- 建值班表 ----
	scheduleResp := adminCreate(t, client, "/api/v1/admin/schedules", map[string]any{
		"team_id":  teamID,
		"name":     "e2e-journey-schedule-" + suffix,
		"timezone": "UTC",
	}, http.StatusCreated)
	scheduleID, _ := scheduleResp["id"].(string)
	if scheduleID == "" {
		t.Fatalf("e2e: 创建值班表响应缺少 id: %+v", scheduleResp)
	}

	// ---- 生成轮换班次（任务 6，替代此前 seed.go 里直接调 ShiftRepo.Create 的旧路径）：
	// 单人单轮，起点比"现在"早 1 分钟、时长 6 小时，确保告警触发时刻落在班次窗口内。
	adminCreate(t, client, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate", map[string]any{
		"user_ids":               []string{userID},
		"shift_duration_seconds": int((6*time.Hour + time.Minute).Seconds()),
		"starts_at":              time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
		"rounds":                 1,
		"dry_run":                false,
	}, http.StatusOK)

	// ---- 配路由：只匹配本场景专用的 alertname，priority 给得比既有场景路由都低
	// （数值更小，命中更早），避免被兜底路由（priority=100）抢先匹配到别的团队。----
	alertname := "e2e-journey-" + suffix
	adminCreate(t, client, "/api/v1/admin/routes", map[string]any{
		"priority":                5,
		"matchers":                []map[string]string{{"name": "alertname", "op": "=", "value": alertname}},
		"target_team_id":          teamID,
		"group_by":                []string{"alertname"},
		"group_wait_seconds":      1,
		"group_interval_seconds":  1,
		"repeat_interval_seconds": 60,
	}, http.StatusCreated)

	// ---- 配升级策略：第 0 级 wait_seconds=0，targets 是团队 ID（oncall.Resolver 按
	// 团队 ID 查值班表求出当班人，不是直接填用户 ID，见 seed.go 同款注释）。----
	adminCreate(t, client, "/api/v1/admin/escalation-policies", map[string]any{
		"team_id": teamID,
		"levels": []map[string]any{
			{"wait_seconds": 0, "targets": []string{teamID}},
		},
	}, http.StatusCreated)

	// ---- ingest critical 告警：复用既有种子集成（token 与团队路由无关，路由只看
	// 告警内容里的 alertname/severity，不看走的是哪个 integration）。RouteStage 有
	// ≤10s 本地规则缓存（routeCacheTTL），刚创建的路由不一定立即对下一个事件生效——
	// 与场景②处理 SilenceStage 10s 缓存同样的手法：固定 startsAt 反复重投同一条
	// 事件（dedup 按 fingerprint 合并，不会产生重复告警）。
	//
	// 陷阱（回归测试中实测发现）：缓存过期前的重投会真实路由到种子兜底团队，触发
	// 该团队自己的一整条编排（分组 -> 人员升级 -> 发送），落到种子 admin 的邮箱——
	// 这不是脏数据，是另一个团队的真实通知。所以下面把"路由生效"和"到人发送
	// 生效"合并成一个等待条件：只要 mock 渠道里出现的 target 还不是本场景用户的
	// 邮箱，就视为尚未成功、继续重投，而不是抓到第一条匹配 alert.ID 的记录就收工
	// （那样会把种子团队那条误判为断言通过）。----
	startsAt := time.Now()
	post := func() {
		if _, _, err := client.postIngest(seedResult.AlertmanagerIntegrationID, seedResult.AlertmanagerToken,
			alertmanagerPayload(alertname, "critical", "firing", startsAt, nil)); err != nil {
			t.Fatalf("e2e: ingest 请求失败: %v", err)
		}
	}

	var alert alertDTO
	var sentTarget string
	waitFor(t, 20*time.Second, time.Second, func() bool {
		post()
		a, ok, err := findAlertByLabel(client, "alertname", alertname)
		if err != nil || !ok || a.TeamID != teamID {
			return false
		}
		alert = a
		// ---- 断言一：mock email 渠道收到发送，target 是解析后的邮箱地址（而非
		// TargetUserID 这个 UUID）。链路：组 flush -> NotifyGroup 启动人员升级链
		// （chain.Start）-> 第 0 级 wait=0 立即触发 -> NotifyEscalation（ChannelKind=
		// personnelChannel="email"，TargetUserID=userID）-> notify_send 定时器 ->
		// Sender.Send 用 ContactResolver 把 userID 解析成 email 并覆写 Target。----
		for _, s := range mockEmail.Sent() {
			if containsString(s.Msg.AlertIDs, alert.ID) && s.Target == email {
				sentTarget = s.Target
				return true
			}
		}
		return false
	}, "告警路由到新建团队且人员升级第 0 级到人发送成功")

	if sentTarget != email {
		t.Fatalf("e2e: mock email 渠道收到的 target 应为解析后的邮箱地址 %q（而非 UUID），实际是 %q", email, sentTarget)
	}
	if sentTarget == userID {
		t.Fatalf("e2e: mock email 渠道收到的 target 不应等于原始 UserID %q（说明到人解析没有生效）", userID)
	}

	// ---- 断言二：notification_log 落 status=sent，target 与幂等键都含解析后的
	// 真实地址（幂等键的地址分量绝不是 UUID，与 outbox 侧 refID 用 UserID 做定时器去重
	// 是两回事——见 internal/app/outbox.go 关于 notifySendRefID 与 idempotencyKey
	// 各自口径的注释）。----
	logTarget, idemKey, found := notificationLogSent(t, alert.ID, "email")
	if !found {
		t.Fatalf("e2e: notification_log 未找到 alert=%s channel=email status=sent 的记录", alert.ID)
	}
	if logTarget != email {
		t.Fatalf("e2e: notification_log.target 应为解析后邮箱 %q，实际 %q", email, logTarget)
	}
	if !strings.Contains(idemKey, email) {
		t.Fatalf("e2e: notification_log.idempotency_key 应包含解析后地址 %q，实际 %q", email, idemKey)
	}

	// ---- 断言三：时间线出现 notify_sent，detail.target 呈邮箱形态（脱敏后仍可辨认，
	// 见 timelineHasNotifySentToEmail 文档）。----
	if !timelineHasNotifySentToEmail(t, alert.ID) {
		t.Fatalf("e2e: 时间线未出现 notify_sent 记录，或其 detail.target 不是邮箱形态")
	}
}
