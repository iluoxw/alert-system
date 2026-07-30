// Command server 是告警管理系统的主入口：组装 ingest/pipeline/scheduler/notify/oncall/api
// 全部依赖并启动 HTTP 服务与两个后台 worker（scheduler 轮询、pipeline 处理事件队列）。
//
// 六个 scheduler 定时器 kind 的绑定全部集中在这里（唯一同时知道全部六个 kind 字符串的
// 装配层，各业务包只知道自己关心的那一个）：
//   - "alert_ttl"            -> notify.LifecycleHandlers.HandleTTL
//   - "alert_repeat"         -> notify.LifecycleHandlers.HandleRepeat
//   - "personnel_escalation" -> oncall.Chain.Handle
//   - "group_wait"           -> pipeline.GroupStage.Handle
//   - "channel_escalation"   -> notify.Engine.Handle
//   - "notify_send"          -> notifySendPayloadHandler（wiring.go，outbox 的发送适配）
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"alert-system/internal/api"
	"alert-system/internal/config"
	"alert-system/internal/domain"
	"alert-system/internal/hook"
	"alert-system/internal/ingest"
	"alert-system/internal/notify"
	"alert-system/internal/oncall"
	"alert-system/internal/pipeline"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入，默认值用于本地开发。
var version = "dev"

func main() {
	configPath := flag.String("config", "", "YAML 配置文件路径（可选）")
	defaultTeamID := flag.String("default-team-id", "", "路由未命中任何规则时的兜底团队 ID（可选，留空表示不路由）")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}

	logger := slog.Default()
	fmt.Printf("alert-system %s\n", version)
	fmt.Printf("配置概要: http_port=%d db_dsn=%s scheduler_poll_interval=%s\n",
		cfg.HTTPPort, maskDSN(cfg.DBDSN), cfg.SchedulerPollInterval)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, *defaultTeamID, logger); err != nil {
		logger.Error("alert-system: 退出", "err", err)
		os.Exit(1)
	}
}

// run 组装全部依赖，启动 HTTP 服务与后台 worker，阻塞直到 ctx 被取消（收到中断信号）
// 或 HTTP 服务异常退出。
func run(ctx context.Context, cfg config.Config, defaultTeamID string, logger *slog.Logger) error {
	st, err := store.New(ctx, cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer st.Close()

	// ---- 仓储：每个仓储都是无状态的，构造零参数即可，事务边界由调用方通过
	// store.Store.WithTx 组织 ----
	alerts := store.NewAlertRepo()
	timeline := store.NewTimelineRepo()
	teams := store.NewTeamRepo()
	users := store.NewUserRepo()
	routes := store.NewRouteRepo()
	schedules := store.NewScheduleRepo()
	shifts := store.NewShiftRepo()
	escalationPolicies := store.NewEscalationPolicyRepo()
	notificationPolicies := store.NewNotificationPolicyRepo()
	integrations := store.NewIntegrationRepo()
	silences := store.NewSilenceRepo()
	changeEvents := store.NewChangeEventRepo()
	notificationLogs := store.NewNotificationLogRepo()
	sessions := store.NewSessionRepo()
	memberships := store.NewMembershipRepo()

	// ---- 定时器：pipeline/notify/oncall 共用的唯一 scheduler 实例 ----
	sched := scheduler.New(st, cfg.SchedulerPollInterval)

	// ---- 生产通知渠道（E2E 用 mock_channel.go 顶替，notify.Register 对同 kind 是
	// 覆盖语义，测试装配不会调用这个函数）----
	registerNotifyChannels(cfg)

	// ---- 通知发送 outbox：四个通知意图接口（oncall.Notifier、pipeline.GroupNotifier、
	// notify.RepeatNotifier、notify.Dispatcher）与实际发送解耦的适配器（见 wiring.go）。
	// outbox 依赖 chain/engine，chain/engine 又依赖 outbox 作为通知意图的实现——用零值
	// 字段构造 outbox 打破这个环：NotifyEscalation/NotifyRepeat/DispatchStep 不touch
	// o.engine/o.chain，真正用到它们的 NotifyGroup 要到 pipeline 起转后才会被调用，
	// 那时 engine/chain 已经赋值完毕。----
	sender := notify.NewSender(st, notificationLogs, timeline)
	outbox := &notifyOutbox{
		scheduler:      sched,
		repeatInterval: cfg.RepeatInterval,
		now:            time.Now,
	}

	resolver := oncall.NewResolver(schedules, shifts)
	chain := oncall.NewChain(st, alerts, timeline, escalationPolicies, resolver, sched, outbox)
	engine := notify.NewEngine(st, alerts, timeline, notificationPolicies, sched, outbox)
	outbox.engine = engine
	outbox.chain = chain

	stopper := newCompositeStopper(chain, engine)
	lifecycle := notify.NewLifecycleHandlers(st, alerts, timeline, sched, stopper, outbox, cfg.RepeatInterval)

	// ---- 管道 stage：顺序 dedup -> route -> group -> silence -> inhibit（route 必须
	// 先于 group——group stage 读取 pctx.Route.GroupBy/GroupWait/GroupInterval，这三个
	// 字段由 route stage 填充；stage_inhibit.go 顺序注释与代码依赖一致，pipeline.go 头
	// 部注释顺序已过时，不采信）----
	eventQueue := store.NewEventQueue()
	hooks := hook.New()
	dedupStage := pipeline.NewDedupStage(alerts, timeline, sched, nil)
	routeStage := pipeline.NewRouteStage(routes, alerts, defaultTeamID)
	groupStage := pipeline.NewGroupStage(st, alerts, sched, outbox)
	silenceStage := pipeline.NewSilenceStage(silences, alerts)
	inhibitStage := pipeline.NewInhibitStage()

	pl := pipeline.New(st, eventQueue, hooks, []pipeline.Stage{
		dedupStage, routeStage, groupStage, silenceStage, inhibitStage,
	})

	// ---- 六个 scheduler kind 全部在这里绑定 ----
	sched.RegisterHandler(notify.TimerKindAlertTTL, lifecycle.HandleTTL)
	sched.RegisterHandler(notify.TimerKindAlertRepeat, lifecycle.HandleRepeat)
	sched.RegisterHandler(oncall.TimerKindPersonnelEscalation, chain.Handle)
	sched.RegisterHandler("group_wait", groupStage.Handle) // groupTimerKind 未导出（pipeline 包内部常量），字面量需与其保持一致
	sched.RegisterHandler(notify.TimerKindChannelEscalation, engine.Handle)
	sched.RegisterHandler(notifySendTimerKind, notifySendPayloadHandler(sender))

	// ---- ingest：三个上游 adapter，兜底等级统一取 domain.SeverityWarning（与
	// integrations.default_severity 列的 DEFAULT 对齐；每 integration 各自的
	// default_severity 覆盖值目前未接入，见任务报告的已知缺口）----
	registry := ingest.NewRegistry()
	registry.Register(ingest.NewAlertmanagerAdapter(domain.SeverityWarning))
	registry.Register(ingest.NewGrafanaAdapter(domain.SeverityWarning))
	registry.Register(ingest.NewGenericAdapter())
	ingestServer := ingest.NewServer(registry, ingest.NewPGStore(st), logger)

	// ---- 飞书卡片回调：Ack/Resolve 从卡片按钮触发。已知缺口（见任务报告）：这条路径
	// 目前没有注入点停止 oncall.Chain/notify.Engine 的定时器；REST API
	// （/api/v1/alerts/{id}/ack|resolve，RegisterAlertRoutes 下面）已经接好 stopper。----
	larkServer := api.NewServer(alerts, timeline, st, cfg.Lark.VerificationToken, cfg.Lark.EncryptKey, logger)

	auth := api.NewPasswordAuthenticator(users)
	router := api.NewRouter(cfg, users, sessions, memberships, st, auth, larkServer.Handler(), logger)
	router.RegisterConfigRoutes(api.ConfigDeps{
		Teams:                teams,
		Users:                users,
		Routes:               routes,
		Schedules:            schedules,
		EscalationPolicies:   escalationPolicies,
		NotificationPolicies: notificationPolicies,
		Integrations:         integrations,
		Silences:             silences,
		AlertSilences:        alerts,
	})
	router.RegisterAlertRoutes(alerts, timeline, changeEvents, stopper, lifecycle)
	router.MountUnauthenticated(http.MethodPost, "/ingest/:integration_id", ingestServer.Handler())

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler: router,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go sched.Run(ctx)
	go pl.Run(ctx)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// registerNotifyChannels 注册生产环境的通知渠道（R6.2 飞书为主力渠道 + webhook）。
// E2E 测试不调用这个函数，改用 test/e2e/mock_channel.go 的 mockChannel 顶替同名 kind
// （notify.Register 对同 kind 是覆盖语义）。
func registerNotifyChannels(cfg config.Config) {
	notify.Register(notify.NewLarkChannel(cfg.Lark.AppID, cfg.Lark.AppSecret))
	notify.Register(notify.NewWebhookChannel(notify.DefaultWebhookTimeout))
}

// maskDSN 隐去 DSN 中的密码部分，避免敏感信息落日志（NFR-Security）。
// 用字符串直接替换而非 net/url 重新编码，避免 "****" 被转义为 %2A 系列字符。
func maskDSN(dsn string) string {
	schemeEnd := strings.Index(dsn, "://")
	at := strings.Index(dsn, "@")
	if schemeEnd < 0 || at < 0 || at < schemeEnd {
		return dsn
	}
	userinfo := dsn[schemeEnd+3 : at]
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return dsn // 无密码部分，无需掩码
	}
	username := userinfo[:colon]
	return dsn[:schemeEnd+3] + username + ":****" + dsn[at:]
}
