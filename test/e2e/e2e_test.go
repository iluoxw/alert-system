// Package e2e 的入口：TestMain 起 docker-compose 真实 Postgres、装配完整系统
// （buildSystem，harness.go）、种子基础配置（seed.go），再依次跑五个场景（design.md
// 三场景 + R13 样例 + Ruling 2 回归场景⑤），顺序 ①→③→②→R13→⑤（团队约定顺序）。
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"alert-system/internal/config"
)

// testDSN 与 docker-compose.yml 里 postgres 服务暴露的宿主机端口一致（15432）。
const testDSN = "postgres://alert_system:alert_system@localhost:15432/alert_system?sslmode=disable"

const composeProject = "firebreak-e2e"

var (
	sys        *system
	seedResult *seedConfigResult
	mockLark   *mockChannel
	mockEmail  *mockChannel
)

func TestMain(m *testing.M) {
	code, err := runTestMain(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: TestMain 失败:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runTestMain 是 TestMain 的主体：用 error 返回值代替到处手写 os.Exit(1)，defer 也就
// 能正常执行（docker compose down、system.close）。
func runTestMain(m *testing.M) (int, error) {
	// E2E 数据目录是测试专用 tmpfs；先清理上一次异常退出遗留的容器，
	// 再强制重建，避免固定种子名称与旧容器数据冲突。
	composeDown()
	if err := composeUp(); err != nil {
		return 0, fmt.Errorf("启动 docker compose 失败: %w", err)
	}
	defer composeDown()

	// mock 渠道必须在任何 NotificationPolicy 经 HTTP 创建之前注册——validateNotificationPolicy
	// 会调 notify.Validate 校验策略引用的渠道已注册（R13.6）。
	mockLark = newMockChannel("lark")
	mockEmail = newMockChannel("email")

	cfg := config.Config{
		DBDSN:                 testDSN,
		SchedulerPollInterval: 200 * time.Millisecond,
		SessionTTL:            time.Hour,
		SessionIdleTTL:        time.Hour,
		SessionCookieSecure:   false,
		RepeatInterval:        4 * time.Second,
		// PersonnelChannel 不走 config.Load()，struct 字面量必须显式给，否则零值 ""
		// 会让 userContactResolver.Resolve 的 channelKind switch 落进 default 分支报错
		// （任务 9：人员升级到人发送依赖这个字段，与生产 config.Load() 的默认值 "email" 对齐）。
		PersonnelChannel: "email",
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	s, err := buildSystem(cfg, "", logger)
	if err != nil {
		return 0, fmt.Errorf("装配 system 失败: %w", err)
	}
	sys = s
	defer sys.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sys.run(ctx)

	ids, err := bootstrapAdmin(ctx, sys.Store, sys.Teams, sys.Users)
	if err != nil {
		return 0, fmt.Errorf("bootstrap 管理员失败: %w", err)
	}
	sr, err := seedConfig(ctx, sys, ids)
	if err != nil {
		return 0, fmt.Errorf("种子配置失败: %w", err)
	}
	seedResult = sr

	return m.Run(), nil
}

func composeUp() error {
	cmd := exec.Command("docker", "compose", "-p", composeProject, "-f", "docker-compose.yml", "up", "-d", "--wait", "--force-recreate", "--remove-orphans")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func composeDown() {
	cmd := exec.Command("docker", "compose", "-p", composeProject, "-f", "docker-compose.yml", "down", "-v")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // 清理失败不应该掩盖测试本身的结果，只记录不 panic
}

// newLoggedInClient 是各场景测试共用的建连+登录步骤：处置动作端点
// （RegisterAlertRoutes）只要求 sessionAuth()，用种子管理员账号登录即可。
func newLoggedInClient(t *testing.T) *apiClient {
	t.Helper()
	client, err := newAPIClient(sys.Server.URL)
	if err != nil {
		t.Fatalf("e2e: 创建 HTTP 客户端失败: %v", err)
	}
	if err := client.login(seedAdminUsername, seedAdminPassword); err != nil {
		t.Fatalf("e2e: 登录失败: %v", err)
	}
	return client
}

func newLoggedInApproverClient(t *testing.T) *apiClient {
	t.Helper()
	client, err := newAPIClient(sys.Server.URL)
	if err != nil {
		t.Fatalf("e2e: 创建审批人 HTTP 客户端失败: %v", err)
	}
	if err := client.login(seedApproverUsername, seedApproverPassword); err != nil {
		t.Fatalf("e2e: 审批人登录失败: %v", err)
	}
	return client
}

// ---- 场景①：Alertmanager webhook -> 去重 -> 路由 -> 分组 -> mock 飞书收到组通知 ->
// Ack -> 人员/渠道升级定时器被取消 ----
func TestScenario1_WebhookNotifyAckCancelsEscalation(t *testing.T) {
	client := newLoggedInClient(t)

	alertname := "e2e-s1-" + uniqueSuffix()
	payload := compatibleWebhookPayload(alertname, "critical", "firing", time.Now(), nil)

	status, body, err := client.postManagedHook(seedResult.GrafanaWebhookPath, payload)
	if err != nil {
		t.Fatalf("e2e: ingest 请求失败: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("e2e: ingest 响应非 202，status=%d body=%s", status, body)
	}

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "alertname", alertname)
		if err != nil || !ok {
			return false
		}
		alert = a
		return true
	}, "告警入库（去重 stage upsert 完成）")

	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		for _, s := range mockLark.Sent() {
			if s.Msg.GroupKey != "" && containsString(s.Msg.AlertIDs, alert.ID) {
				return true
			}
		}
		return false
	}, "组到期后 mock 飞书渠道收到组通知（分组 stage flush -> NotifyGroup -> notify_send -> Sender.Send）")

	status, body, err = client.doJSON(http.MethodPost, "/api/v1/alerts/"+alert.ID+"/ack", nil, nil)
	if err != nil {
		t.Fatalf("e2e: ack 请求失败: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("e2e: ack 响应非 200，status=%d body=%s", status, body)
	}

	waitFor(t, 5*time.Second, 200*time.Millisecond, func() bool {
		return pendingTimerCount(t, []string{"personnel_escalation", "channel_escalation"}, alert.ID) == 0
	}, "Ack 后人员升级/渠道升级的 pending 定时器均被取消（EscalationStopper 组合 stopper）")
}

// ---- 场景③：resolve_policy=ttl 的告警在 TTL 到期后自动 resolved（无需上游显式推送
// resolved 事件）。必须走 generic adapter：AlertmanagerAdapter.translate 硬编码
// ResolvePolicySource，没有办法产出 resolve_policy=ttl 的事件。----
func TestScenario3_TTLAutoResolve(t *testing.T) {
	client := newLoggedInClient(t)

	ruleID := "e2e-s3-" + uniqueSuffix()
	labels := map[string]string{"alertname": ruleID}
	payload := genericAlertPayload(ruleID, "warning", "firing", "ttl", 3, time.Now(), labels)

	status, body, err := client.postManagedHook(seedResult.GenericWebhookPath, payload)
	if err != nil {
		t.Fatalf("e2e: ingest 请求失败: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("e2e: ingest 响应非 202，status=%d body=%s", status, body)
	}

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "alertname", ruleID)
		if err != nil || !ok {
			return false
		}
		alert = a
		return true
	}, "告警入库")

	waitFor(t, 15*time.Second, 300*time.Millisecond, func() bool {
		a, err := getAlert(client, alert.ID)
		if err != nil {
			return false
		}
		return a.Status == "resolved"
	}, "TTL（3s）到期后告警自动 resolved（alert_ttl 定时器 -> LifecycleHandlers.HandleTTL）")
}

// ---- 场景②：静默生效期间零通知，静默过期后 repeat 恢复。----
//
// 关键机制（详见 harness.go/wiring.go 的 NotifyGroup 文档）：组的 group_wait/
// group_interval 定时器由"是否有新事件抵达该分组"驱动，不感知静默；重新 POST 一次
// 重复事件会重新挂上组到期定时器，到期后重新调用 NotifyGroup——这既是"重复事件推动
// SilenceStage 更新 silenced_by"的必经之路，也是"静默过期后由路由/通知编排重新驱动
// 通知"的机制本身（NotifyGroup 内 activeAlerts 过滤 + ScheduleIfAbsent 重新接上
// alert_repeat）。SilenceStage 有 10 秒本地规则缓存，需要重复投递直到断言的
// silenced_by 真正生效。
//
// 断言 mock 飞书渠道恢复后收到的发送数比"确认静默生效那一刻"的基线至少多 1 条，
// 而不只是"存在过一条"：这是 Bug C（notify.Sender 的幂等键此前没有区分"轮次"，
// 导致同一 (group, channel, target) 元组终身只能成功发送一次，恢复后的组通知会被
// 误判为静默前那次的重投而悄悄丢弃，违反 R6.5 重复提醒语义）的回归断言。修复后
// SendRequest.Round 让恢复后的这一轮拿到不同的幂等键，才能真正发出。
func TestScenario2_SilenceSuppressesThenResumesRepeat(t *testing.T) {
	client := newLoggedInClient(t)

	alertname := "e2e-s2-" + uniqueSuffix()
	startsAt := time.Now()
	post := func() {
		if _, _, err := client.postManagedHook(seedResult.GrafanaWebhookPath,
			compatibleWebhookPayload(alertname, "critical", "firing", startsAt, nil)); err != nil {
			t.Fatalf("e2e: ingest 请求失败: %v", err)
		}
	}
	post()

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "alertname", alertname)
		if err != nil || !ok {
			return false
		}
		alert = a
		return true
	}, "告警入库")

	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		for _, s := range mockLark.Sent() {
			if s.Msg.GroupKey != "" && containsString(s.Msg.AlertIDs, alert.ID) {
				return true
			}
		}
		return false
	}, "首次组通知（同时启动 repeat/人员升级/渠道升级三条链）")

	// 静默窗口长度必须明显大于 SilenceStage 的 10s 本地规则缓存 TTL
	// （internal/pipeline/stage_silence.go 的 silenceCacheTTL）：缓存最坏情况下要到
	// 创建静默后将近 10s 才会刷新并第一次看见这条新规则，窗口太短（比如 6s）会出现
	// "缓存刷新时静默窗口已经过期"的竞态，导致本场景断续性失败——与 Bug C 无关，是
	// 测试自身时钟压缩比例需要给缓存 TTL 留够余量。
	silenceEnds := time.Now().Add(22 * time.Second)
	silenceBody := map[string]any{
		"matchers": []map[string]string{
			{"name": "alertname", "op": "=", "value": alertname},
		},
		"starts_at": time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
		"ends_at":   silenceEnds.Format(time.RFC3339Nano),
		"comment":   "e2e scenario2",
	}
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/admin/silences", silenceBody, nil)
	if err != nil {
		t.Fatalf("e2e: 创建静默失败: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("e2e: 创建静默响应非 201，status=%d body=%s", status, body)
	}

	// SilenceStage 的 10s 本地缓存意味着刚创建的静默不会立即对下一个到达事件生效，
	// 必须反复重投重复事件，直到某一次 Process 命中新鲜缓存并写回 silenced_by。
	waitFor(t, 18*time.Second, time.Second, func() bool {
		post()
		a, err := getAlert(client, alert.ID)
		if err != nil {
			return false
		}
		return len(a.SilencedBy) > 0
	}, "静默生效（alert.silenced_by 非空）")

	// baseline 在"确认静默已生效"之后才取样——而不是在创建静默之前：这段时间里
	// repeat/人员升级/渠道升级三条链各自的首批定时器（wait_seconds=0/6 等）可能已经
	// 独立触发过若干条与本告警相关的发送，这些都不是"静默期间的违规发送"，只是
	// 巧合发生在确认静默生效之前。用"确认生效那一刻"的计数作基线，才能精确断言
	// "此后 5 秒内不应再增长"。
	baseline := countSentForAlert(mockLark, alert.ID)
	time.Sleep(5 * time.Second) // 覆盖至少一个 repeatInterval（4s）
	if got := countSentForAlert(mockLark, alert.ID); got != baseline {
		t.Fatalf("e2e: 静默生效期间不应有新的 mock 飞书发送，确认生效时 %d 条，5s 后变为 %d 条", baseline, got)
	}

	if wait := time.Until(silenceEnds); wait > 0 {
		time.Sleep(wait + time.Second)
	}

	waitFor(t, 15*time.Second, time.Second, func() bool {
		post()
		return countSentForAlert(mockLark, alert.ID) >= baseline+1
	}, "静默过期后通知恢复：mock 飞书渠道收到的发送数比确认生效时的基线至少多 1 条（Bug C 回归）")
}

// ---- R13 样例：warning 级别告警先立即 email，3 秒后升级到 lark（同一人换渠道，
// 与人员升级链正交）。----
func TestScenarioR13_ChannelEscalationEmailThenLark(t *testing.T) {
	client := newLoggedInClient(t)

	alertname := "e2e-r13-" + uniqueSuffix()
	payload := compatibleWebhookPayload(alertname, "warning", "firing", time.Now(), nil)

	status, body, err := client.postManagedHook(seedResult.GrafanaWebhookPath, payload)
	if err != nil {
		t.Fatalf("e2e: ingest 请求失败: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("e2e: ingest 响应非 202，status=%d body=%s", status, body)
	}

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "alertname", alertname)
		if err != nil || !ok {
			return false
		}
		alert = a
		return true
	}, "告警入库")

	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		for _, s := range mockEmail.Sent() {
			if containsString(s.Msg.AlertIDs, alert.ID) {
				return true
			}
		}
		return false
	}, "第 0 级立即经 email 发送（notification-policies 里 warning 序列的第一级）")

	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		for _, s := range mockLark.Sent() {
			// 与场景①/②的组通知区分：渠道升级第 1 级发的是单告警消息，GroupKey 为空。
			if s.Msg.GroupKey == "" && containsString(s.Msg.AlertIDs, alert.ID) {
				return true
			}
		}
		return false
	}, "第 1 级（3s 后）升级到 lark（同一人换渠道，R13）")
}

// ---- 场景⑤（Ruling 2 回归）：路由未配置 group_by 时，告警必须仍能收到通知——
// GroupStage 把这种路由退化为"单例组"（route_id+fingerprint 各自独占一个组），
// 复用与分组路由完全相同的 UpdateRouting/HasFired/ScheduleIfAbsent/NotifyGroup
// 流程，不存在脱离组机制之外的"直发"路径，因此与场景①一样能等到 group_wait
// 到期后的 mock 飞书通知；唯一区别是这里发的是"单告警组"消息（GroupKey 非空，
// 但组内只有这一条告警），而不是场景①里按 alertname 聚合的多告警组消息。----
func TestScenario5_UngroupedRouteStillNotifies(t *testing.T) {
	client := newLoggedInClient(t)

	alertname := "e2e-s5-" + uniqueSuffix()
	payload := compatibleWebhookPayload(alertname, "critical", "firing", time.Now(), nil)

	status, body, err := client.postManagedHook(seedResult.GrafanaWebhookPath, payload)
	if err != nil {
		t.Fatalf("e2e: ingest 请求失败: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("e2e: ingest 响应非 202，status=%d body=%s", status, body)
	}

	var alert alertDTO
	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		a, ok, err := findAlertByLabel(client, "alertname", alertname)
		if err != nil || !ok {
			return false
		}
		alert = a
		return true
	}, "告警入库（命中场景⑤专用的无 group_by 路由）")

	if alert.GroupKey == "" {
		t.Fatal("无 group_by 路由也应写入非空 group_key（单例组，Ruling 2）")
	}

	waitFor(t, 10*time.Second, 200*time.Millisecond, func() bool {
		for _, s := range mockLark.Sent() {
			if containsString(s.Msg.AlertIDs, alert.ID) {
				return true
			}
		}
		return false
	}, "无 group_by 路由的告警仍能等到 group_wait 到期后的 mock 飞书通知（Ruling 2 回归）")
}
