// Package e2e 驱动任务 31 的四个端到端场景：真实 Postgres + 真实 scheduler/pipeline
// worker + mock 通知渠道，只用秒级配置压缩时钟等待（design.md 三场景 + R13 样例）。
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"alert-system/internal/alertsource"
	"alert-system/internal/api"
	"alert-system/internal/app"
	"alert-system/internal/ccloud"
	"alert-system/internal/config"
	"alert-system/internal/domain"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/ingest"
	"alert-system/internal/pipeline"
	"alert-system/internal/runbook"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// 本文件是 cmd/server/main.go 装配逻辑在 test/e2e 包内的复刻（而不是 import）：
// mock 渠道通过 notify.Register 是进程内全局副作用，必须与被测的 scheduler/pipeline
// worker 运行在同一个进程里；而 cmd/server 是 package main，无法被 test/e2e 导入。
// pipeline/notify/oncall 的装配（含六个 scheduler kind 绑定、"意图/发送解耦" outbox）
// 已收拢进 internal/app.Build，与 cmd/server/main.go 共用同一份逻辑——本文件只负责
// E2E 独有的部分：httptest.Server、mock 渠道生效所需的 ingest adapter。

// system 持有一次 E2E 运行组装出的全部依赖：既需要经真实 HTTP（httptest.Server）
// 驱动四个场景，也需要偶尔越过 HTTP 直接调用 store 仓储做 bootstrap seed（见
// seed.go 的"HTTP admin API 鸡生蛋"说明）。
type system struct {
	Store  *store.Store
	Server *httptest.Server

	Teams                *store.TeamRepo
	Users                *store.UserRepo
	Routes               *store.RouteRepo
	Schedules            *store.ScheduleRepo
	Shifts               *store.ShiftRepo
	EscalationPolicies   *store.EscalationPolicyRepo
	NotificationPolicies *store.NotificationPolicyRepo
	Integrations         *store.IntegrationRepo
	AlertSources         *store.AlertSourceRepo
	Silences             *store.SilenceRepo
	Alerts               *store.AlertRepo
	Ops                  *store.OpsRepo
	Faults               *store.FaultRepo
	Topology             *store.TopologyRepo
	Incidents            *store.IncidentRepo
	Runbooks             *store.RunbookRepo
	Services             *store.ServiceCatalogRepo
	AlertMappings        *store.AlertMappingRepo

	scheduler    *scheduler.Scheduler
	pipeline     *pipeline.Pipeline
	reconciler   *alertsource.Reconciler
	ccloudSyncer *ccloud.Syncer
}

// buildSystem 组装全部依赖并起一个 httptest.Server，但不启动 scheduler/pipeline
// 后台 worker（由调用方决定何时 run，通常是 seed 完成之后）。
func buildSystem(cfg config.Config, defaultTeamID string, logger *slog.Logger) (*system, error) {
	st, err := store.New(context.Background(), cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("e2e: 连接数据库失败: %w", err)
	}

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

	// pipeline/notify/oncall 全部依赖 + 六个 scheduler kind 的绑定，与
	// cmd/server/main.go 共用同一份装配逻辑，见 internal/app.Build。
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

	// 启动对账（design.md R2），与生产 main.go 共用同一个 app.ReconcileOpsTimers；
	// E2E 场景目前不预置 ops 设置，通常是空操作，保留是为了不让两处装配出现分叉。
	if err := app.ReconcileOpsTimers(context.Background(), st, opsRepo, sched); err != nil {
		return nil, fmt.Errorf("e2e: ops 定时器启动对账失败: %w", err)
	}
	// 与生产 main.go 共用同一个 app.ReconcileIncidentNudgeTimers；E2E 默认关闭
	// INCIDENT_NUDGE_INTERVAL 时函数内部直接跳过，保留调用只是不让两处装配分叉。
	if err := app.ReconcileIncidentNudgeTimers(context.Background(), st, incidents, sched, cfg.IncidentNudgeInterval); err != nil {
		return nil, fmt.Errorf("e2e: Incident 恢复提醒定时器启动对账失败: %w", err)
	}
	// 与生产 main.go 保持同一装配：retention_sweep 必须启动对账，否则清理任务永远不会运行。
	if err := app.ReconcileRetentionTimer(context.Background(), st, sched, cfg.Retention); err != nil {
		return nil, fmt.Errorf("e2e: retention_sweep 定时器启动对账失败: %w", err)
	}

	// 既有本地路由/分组/通知场景改走 Grafana 兼容入口；Alertmanager 在新架构中
	// 是唯一 Alert 权威来源，只镜像事实，不再重复执行本地路由/静默/通知。
	// generic 继续用于显式指定 resolve_policy=ttl 的兼容场景。
	registry := ingest.NewRegistry()
	registry.Register(ingest.NewAlertmanagerAdapter(domain.SeverityWarning))
	registry.Register(ingest.NewGrafanaAdapter(domain.SeverityWarning))
	registry.Register(ingest.NewGenericAdapter())
	ingestServer := ingest.NewServer(registry, ingest.NewPGStore(st), logger)
	reconciler := alertsource.NewReconciler(st, alertSources, alerts, timeline, nil, logger).
		WithProjector(a.AlertProjector).WithFaultActivation(a.FaultOutbox.TriggerFromMappingResult)

	larkServer := api.NewServer(alerts, timeline, st, cfg.Lark.VerificationToken, cfg.Lark.EncryptKey, logger, stopper, lifecycle).
		SetLarkIdentityStore(users).
		SetLarkActionStore(store.NewLarkCardActionRepo()).
		SetLarkFaultStore(faults)

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
	router.RegisterAlertSourceRoutes(alertSources, reconciler)
	incidentService := incidentservice.NewService(st, faults, incidents, a.IncidentOutbox)
	larkServer.SetLarkIncidentTransitioner(incidentService)
	router.RegisterIncidentRoutes(incidentService)
	runbookService := runbook.NewService(st, incidents, runbooks, runbook.DisabledExecutor{})
	router.RegisterRunbookRoutes(runbookService)
	router.RegisterInfrastructureRoutes(topology, faults, incidents, alerts)
	router.RegisterFaultRoutes(faults)
	router.RegisterTopologyAdminRoutes(topology)
	router.RegisterServiceCatalogRoutes(serviceCatalog, alertMappings, a.AlertProjector, a.ImpactProjector, alerts)
	router.RegisterOpsSettingsRoutes(teams, opsRepo, sched)
	router.RegisterMemberRoutes(teams, users, memberships)
	router.RegisterShiftRoutes(schedules, users, shifts)
	router.RegisterDirectoryRoutes(teams, users)
	router.RegisterHealthRoutes(st.Pool.Ping)
	router.MountUnauthenticated(http.MethodPost, "/ingest/v1/hook/:hook_id", ingestServer.Handler())

	httpSrv := httptest.NewServer(router)

	var ccloudSyncer *ccloud.Syncer
	if cfg.CCloud.Enabled() {
		client := ccloud.NewClient(cfg.CCloud.BaseURL, cfg.CCloud.TokenEnv, nil).WithPaths(ccloud.ClientPaths{
			FullLink: cfg.CCloud.PathFullLink,
			Service:  cfg.CCloud.PathService,
			Health:   cfg.CCloud.PathHealth,
		})
		ccloudSyncer = ccloud.NewSyncer(st, topology, client, cfg.CCloud.ProjectIDs, cfg.CCloud.Environment, cfg.CCloud.SyncInterval, logger)
	}

	return &system{
		Store:                st,
		Server:               httpSrv,
		Teams:                teams,
		Users:                users,
		Routes:               routes,
		Schedules:            schedules,
		Shifts:               shifts,
		EscalationPolicies:   escalationPolicies,
		NotificationPolicies: notificationPolicies,
		Integrations:         integrations,
		AlertSources:         alertSources,
		Silences:             silences,
		Alerts:               alerts,
		Ops:                  opsRepo,
		Faults:               faults,
		Topology:             topology,
		Incidents:            incidents,
		Runbooks:             runbooks,
		Services:             serviceCatalog,
		AlertMappings:        alertMappings,
		scheduler:            sched,
		pipeline:             pl,
		reconciler:           reconciler,
		ccloudSyncer:         ccloudSyncer,
	}, nil
}

// run 启动 scheduler/pipeline 后台 worker，随 ctx 取消而退出。
func (s *system) run(ctx context.Context) {
	go s.scheduler.Run(ctx)
	go s.pipeline.Run(ctx)
	go s.reconciler.Run(ctx)
	if s.ccloudSyncer != nil {
		go s.ccloudSyncer.Run(ctx)
	}
}

// close 关闭 httptest.Server 与数据库连接池。
func (s *system) close() {
	s.Server.Close()
	s.Store.Close()
}
