// Command server 是告警管理系统的主入口：组装 ingest/pipeline/scheduler/notify/oncall/api
// 全部依赖并启动 HTTP 服务与两个后台 worker（scheduler 轮询、pipeline 处理事件队列）。
//
// pipeline/notify/oncall 的装配（含六个 scheduler 定时器 kind 的绑定）收拢在
// internal/app.Build 里，与 test/e2e/harness.go 共用同一份逻辑——本文件只负责生产
// 独有的部分：真实通知渠道注册、真实 http.Server、三个 ingest adapter（含 Grafana）。
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

	"alert-system/internal/alertsource"
	"alert-system/internal/api"
	"alert-system/internal/app"
	"alert-system/internal/config"
	"alert-system/internal/domain"
	"alert-system/internal/events"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/ingest"
	"alert-system/internal/notify"
	"alert-system/internal/runbook"
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

	if err := run(ctx, cfg, *defaultTeamID, runbook.DisabledExecutor{}, logger); err != nil {
		logger.Error("alert-system: 退出", "err", err)
		os.Exit(1)
	}
}

// run 组装全部依赖，启动 HTTP 服务与后台 worker，阻塞直到 ctx 被取消（收到中断信号）
// 或 HTTP 服务异常退出。
func run(ctx context.Context, cfg config.Config, defaultTeamID string, runbookExecutor runbook.Executor, logger *slog.Logger) error {
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
	alertSources := store.NewAlertSourceRepo()
	silences := store.NewSilenceRepo()
	changeEvents := store.NewChangeEventRepo()
	notificationLogs := store.NewNotificationLogRepo()
	sessions := store.NewSessionRepo()
	memberships := store.NewMembershipRepo()
	opsRepo := store.NewOpsRepo()
	faults := store.NewFaultRepo()
	topology := store.NewTopologyRepo()
	incidents := store.NewIncidentRepo()
	runbooks := store.NewRunbookRepo()
	serviceCatalog := store.NewServiceCatalogRepo()
	alertMappings := store.NewAlertMappingRepo()

	// ---- 生产通知渠道（E2E 用 mock_channel.go 顶替，notify.Register 对同 kind 是
	// 覆盖语义，测试装配不会调用这个函数）----
	registerNotifyChannels(cfg)

	// ---- pipeline/notify/oncall 全部依赖 + 六个 scheduler kind 的绑定，
	// 与 test/e2e/harness.go 共用同一份装配逻辑，见 internal/app.Build ----
	a := app.Build(st, app.Repos{
		Alerts:               alerts,
		Timeline:             timeline,
		Routes:               routes,
		Schedules:            schedules,
		Shifts:               shifts,
		EscalationPolicies:   escalationPolicies,
		NotificationPolicies: notificationPolicies,
		Silences:             silences,
		NotificationLogs:     notificationLogs,
		Incidents:            incidents,
		Teams:                teams,
		Users:                users,
		Ops:                  opsRepo,
		ServiceCatalog:       serviceCatalog,
		AlertMappings:        alertMappings,
		Topology:             topology,
		Faults:               faults,
	}, cfg, defaultTeamID)
	sched, pl, stopper, lifecycle := a.Scheduler, a.Pipeline, a.Stopper, a.Lifecycle

	// ---- 启动对账（design.md R2）：补建已启用但缺失 pending 定时器的 ops 周期
	// 任务；失败不阻断启动——下一次进程重启或任务 10 的保存端点仍会重试，见
	// app.ReconcileOpsTimers 文档。----
	if err := app.ReconcileOpsTimers(ctx, st, opsRepo, sched); err != nil {
		logger.Error("alert-system: ops 定时器启动对账失败", "err", err)
	}
	// ---- 同样的启动对账，补建所有 open Incident 缺失的 incident_recovery_nudge
	// 定时器（INCIDENT_NUDGE_INTERVAL<=0 时函数内部直接跳过）----
	if err := app.ReconcileIncidentNudgeTimers(ctx, st, incidents, sched, cfg.IncidentNudgeInterval); err != nil {
		logger.Error("alert-system: Incident 恢复提醒定时器启动对账失败", "err", err)
	}
	// ---- retention_sweep 启动对账：此前从未被调用，retention_sweep 从未启动，
	// timers / notification_log 留存清理实际没有运行 ----
	if err := app.ReconcileRetentionTimer(ctx, st, sched, cfg.Retention); err != nil {
		logger.Error("alert-system: retention_sweep 定时器启动对账失败", "err", err)
	}

	// ---- ingest：三个上游 adapter，兜底等级统一取 domain.SeverityWarning（与
	// integrations.default_severity 列的 DEFAULT 对齐；每 integration 各自的
	// default_severity 覆盖值目前未接入，见任务报告的已知缺口）----
	registry := ingest.NewRegistry()
	registry.Register(ingest.NewAlertmanagerAdapter(domain.SeverityWarning))
	registry.Register(ingest.NewGrafanaAdapter(domain.SeverityWarning))
	registry.Register(ingest.NewGenericAdapter())
	ingestServer := ingest.NewServer(registry, ingest.NewPGStore(st), logger)
	alertSourceReconciler := alertsource.NewReconciler(st, alertSources, alerts, timeline, nil, logger).WithProjector(a.AlertProjector)

	// ---- 飞书卡片回调：Ack/Resolve 从卡片按钮触发，与下面 REST API
	// （/api/v1/alerts/{id}/ack|resolve，RegisterAlertRoutes）共用同一套
	// stopper/lifecycle，保证两条处置路径的"停止升级链/生命周期定时器"不变量一致。----
	larkServer := api.NewServer(alerts, timeline, st, cfg.Lark.VerificationToken, cfg.Lark.EncryptKey, logger, stopper, lifecycle).
		SetLarkIdentityStore(users).
		SetLarkActionStore(store.NewLarkCardActionRepo()).
		SetLarkFaultStore(faults)

	// ---- 告警变更实时推送：PG 触发器 pg_notify（migrations/000011）→ 专用连接
	// LISTEN → 进程内 Bus → SSE。RunListener 是常驻 goroutine，独立于 st 的连接池，
	// 不占用业务查询的连接配额。----
	bus := events.NewBus()
	go events.RunListener(ctx, cfg.DBDSN, bus, logger)

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
		CacheInvalidator:     a.CacheInvalidator,
	})
	router.RegisterAlertRoutes(alerts, timeline, changeEvents, stopper, lifecycle)
	router.RegisterAlertSourceRoutes(alertSources, alertSourceReconciler)
	incidentService := incidentservice.NewService(st, faults, incidents, a.IncidentOutbox)
	larkServer.SetLarkIncidentTransitioner(incidentService)
	router.RegisterIncidentRoutes(incidentService)
	runbookService := runbook.NewService(st, incidents, runbooks, runbookExecutor)
	router.RegisterRunbookRoutes(runbookService)
	router.RegisterInfrastructureRoutes(topology, faults, incidents, alerts)
	router.RegisterServiceCatalogRoutes(serviceCatalog, alertMappings, a.AlertProjector, a.ImpactProjector, alerts)
	router.RegisterOpsSettingsRoutes(teams, opsRepo, sched)
	router.RegisterMemberRoutes(teams, users, memberships)
	router.RegisterShiftRoutes(schedules, users, shifts)
	router.RegisterDirectoryRoutes(teams, users)
	router.RegisterEventRoutes(bus)
	router.RegisterHealthRoutes(st.Pool.Ping)
	router.MountUnauthenticated(http.MethodPost, "/ingest/v1/hook/:hook_id", ingestServer.Handler())

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
	go alertSourceReconciler.Run(ctx)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// registerNotifyChannels 注册生产环境的通知渠道（R6.2 飞书为主力渠道 + webhook + 任务 9
// 新增的 email——第一个真正"到人"的渠道）。E2E 测试不调用这个函数，改用
// test/e2e/mock_channel.go 的 mockChannel 顶替同名 kind（notify.Register 对同 kind 是
// 覆盖语义）。cfg.SMTP.Host 为空时 EmailChannel 仍正常注册，Send 时返回
// notify.ErrSMTPNotConfigured（走既有 markFailed 链路，不是启动期报错）。
func registerNotifyChannels(cfg config.Config) {
	notify.Register(notify.NewLarkChannel(cfg.Lark.AppID, cfg.Lark.AppSecret))
	notify.Register(notify.NewLarkWebhookChannel(notify.DefaultWebhookTimeout))
	notify.Register(notify.NewWebhookChannel(notify.DefaultWebhookTimeout))
	notify.Register(notify.NewEmailChannel(cfg.SMTP.Host, cfg.SMTP.Port, cfg.SMTP.Username, cfg.SMTP.Password, cfg.SMTP.From, cfg.SMTP.StartTLS))
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
