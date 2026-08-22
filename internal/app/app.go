// Package app 收拢 cmd/server/main.go 与 test/e2e/harness.go 原先各自手工复制一份的
// pipeline/notify/oncall 依赖装配：四个"通知意图"接口与实际发送解耦的 outbox、
// compositeStopper、六个 scheduler 定时器 kind 的绑定、pipeline 五个 stage 的组装
// 顺序。两处装配在任务 31 之前逐行相同，任何一处后续单独改都会造成静默漂移（正是
// scheduler.Handler 签名这次改动触发的教训）——收拢到这里后，两个调用方都只需要
// 调用 Build，签名或装配顺序变化只需要改一处。
//
// 不属于本包的部分：HTTP 路由/飞书回调服务器的组装（api.NewRouter/api.NewServer
// 及其各 RegisterXxxRoutes 调用）与 ingest 适配器注册——这两处生产与 E2E 本就存在
// 刻意差异（真实 http.Server vs httptest.Server；ingest adapter 集合不同），继续留在
// 各自调用方，不属于"改一处生效两处"的范畴。
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/config"
	"alert-system/internal/domain"
	"alert-system/internal/hook"
	impactprojection "alert-system/internal/impact"
	alertmapping "alert-system/internal/mapping"
	"alert-system/internal/notify"
	"alert-system/internal/oncall"
	"alert-system/internal/ops"
	"alert-system/internal/pipeline"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// groupTimerKind 与 internal/pipeline/stage_group.go 内未导出的同名常量必须保持
// 一致（"group_wait"），字面量在装配层重复一次是因为 pipeline 包不导出它。
const groupTimerKind = "group_wait"

// Repos 是 Build 组装 pipeline/notify/oncall/ops 依赖时用到的仓储子集。调用方各自按
// 无状态零参数构造出实际的 store.*Repo 实例后传入——两处装配的仓储构造本就完全
// 一致，这里只做一次收拢，避免 Build 的参数列表过长。Teams/Users/Ops 是任务 9 新增
// （ops.Handlers 需要团队展示名、值班人员展示名、team_ops_settings 仓储），routes/
// schedules/shifts 等仅供路由/配置 API 使用的仓储不在这里，继续由调用方自己持有。
type Repos struct {
	Alerts               *store.AlertRepo
	Timeline             *store.TimelineRepo
	Routes               *store.RouteRepo
	Schedules            *store.ScheduleRepo
	Shifts               *store.ShiftRepo
	EscalationPolicies   *store.EscalationPolicyRepo
	NotificationPolicies *store.NotificationPolicyRepo
	Silences             *store.SilenceRepo
	NotificationLogs     *store.NotificationLogRepo
	Incidents            *store.IncidentRepo
	Teams                *store.TeamRepo
	Users                *store.UserRepo
	Ops                  *store.OpsRepo
	ServiceCatalog       *store.ServiceCatalogRepo
	AlertMappings        *store.AlertMappingRepo
	Topology             *store.TopologyRepo
	Faults               *store.FaultRepo
}

// App 是 Build 组装完毕后调用方需要的公共依赖：Scheduler/Pipeline 供调用方决定
// 何时启动后台 worker（go app.Scheduler.Run(ctx)/go app.Pipeline.Run(ctx)）；
// Stopper/Lifecycle 供 api.NewServer/router.RegisterAlertRoutes 注入，保证飞书卡片
// 回调与 REST API 两条处置路径共用同一套"停止升级链/生命周期定时器"实现。
type App struct {
	Scheduler        *scheduler.Scheduler
	Pipeline         *pipeline.Pipeline
	Sender           *notify.Sender
	Stopper          CompositeStopper
	Lifecycle        *notify.LifecycleHandlers
	CacheInvalidator *pipeline.ConfigCacheInvalidator
	IncidentOutbox   *IncidentOutbox
	AlertProjector   *alertmapping.Projector
	ImpactProjector  *impactprojection.Projector
}

// Build 组装唯一的 scheduler 实例、notify.Sender、outbox、oncall.Chain/notify.Engine/
// notify.LifecycleHandlers、pipeline 五个 stage，并把六个 scheduler kind 全部绑定
// 完毕。st 仅用于 eventQueue（EventQueue 复用同一个 *store.Store 落库）与
// notify.NewSender 的独立事务；repos 由调用方按 Repos 的字段说明各自构造；
// cfg.SchedulerPollInterval 控制轮询间隔，cfg.RepeatInterval 是 R6.5 重复提醒周期；
// defaultTeamID 是路由未命中任何规则时的兜底团队（可为空）。
//
// 不启动任何后台 worker——调用方决定何时 go App.Scheduler.Run(ctx)/go
// App.Pipeline.Run(ctx)（生产用真实 ctx 直到收到中断信号；E2E 用测试自己的
// system.run(ctx) 编排，通常等 seed 完成之后）。
func Build(st *store.Store, repos Repos, cfg config.Config, defaultTeamID string) *App {
	sched := scheduler.New(st, cfg.SchedulerPollInterval)

	sender := notify.NewSender(st, repos.NotificationLogs, repos.Timeline)
	if repos.Incidents != nil {
		sender.IncidentTimeline = repos.Incidents
	}
	// 任务 9：到人发送在 Sender.Send 第 0 段解析 TargetUserID -> 实际渠道地址，
	// 具体解析实现见下方 userContactResolver（基于 UserRepo 的薄适配器）。
	sender.Resolver = userContactResolver{users: repos.Users}

	// outbox 依赖 chain/engine，chain/engine 又依赖 outbox 作为通知意图的实现——
	// 用零值字段构造 outbox 打破这个环：NotifyEscalation/NotifyRepeat/DispatchStep
	// 不 touch o.engine/o.chain，真正用到它们的 NotifyGroup 要到 pipeline 起转后才会
	// 被调用，那时 engine/chain 已经赋值完毕。
	outbox := &notifyOutbox{
		scheduler:        sched,
		repeatInterval:   cfg.RepeatInterval,
		personnelChannel: cfg.PersonnelChannel,
		now:              time.Now,
	}
	incidentOutbox := &IncidentOutbox{scheduler: sched, now: time.Now}

	resolver := oncall.NewResolver(repos.Schedules, repos.Shifts)
	chain := oncall.NewChain(repos.Alerts, repos.Timeline, repos.EscalationPolicies, resolver, sched, outbox)
	engine := notify.NewEngine(repos.Alerts, repos.Timeline, repos.NotificationPolicies, sched, outbox)
	outbox.engine = engine
	outbox.chain = chain

	stopper := NewCompositeStopper(chain, engine)
	lifecycle := notify.NewLifecycleHandlers(repos.Alerts, repos.Timeline, sched, stopper, outbox, cfg.RepeatInterval)

	// ---- ops 三个周期任务 handler（任务 9）：settings/daily/oncall 三个适配器把
	// store 仓储适配成 ops 包定义的窄接口；stale 直接复用 repos.Ops——
	// store.OpsRepo.ListUnackedCritical 与 ops.StaleAlertRepo 的方法签名逐字段
	// 相同，不需要额外适配器。resolver 复用上面 oncall.NewResolver 的同一个实例，
	// 不重复构造。----
	opsHandlers := ops.NewHandlers(
		opsSettingsRepo{repos.Ops},
		opsTeamRepo{repos.Teams},
		opsDailyStatsRepo{repos.Ops},
		repos.Ops,
		opsOncallResolver{resolver, repos.Users},
		outbox,
		sched,
	)
	// AlertListURL 留空不注入：全仓目前没有 public base URL / 部署域名配置概念，
	// 引入新配置项超出 ops-enhancements spec 范围；待后续引入该配置后在此注入
	// "/alerts 列表页链接" 生成函数即可（ops.Handlers.detailURL 对 nil 已做降级）。

	// ---- 管道 stage：顺序 dedup -> route -> group -> silence -> inhibit（route 必须
	// 先于 group——group stage 读取 pctx.Route.GroupBy/GroupWait/GroupInterval，这三个
	// 字段由 route stage 填充）----
	// stormState 是 route_storm_state 的唯一仓储实例，GroupStage（风暴判定/Activate，
	// 任务 9）与 stormHandler（摘要发送/Deactivate，任务 10）共用同一份——两者都是
	// 无状态包装（同 AlertRepo），共用实例只是避免重复构造，不涉及并发状态。
	stormState := store.NewStormStateRepo()

	eventQueue := store.NewEventQueue()
	hooks := hook.New()
	dedupStage := pipeline.NewDedupStage(repos.Alerts, repos.Timeline, sched, nil, cfg.FlapWindowSeconds, cfg.FlapThreshold)
	impactProjector := impactprojection.NewProjector(repos.Topology, repos.Faults)
	projector := alertmapping.NewProjector(repos.AlertMappings, repos.ServiceCatalog, repos.Topology, repos.Faults, impactProjector)
	mappingStage := pipeline.NewMappingStage(projector)
	routeStage := pipeline.NewRouteStage(repos.Routes, repos.Alerts, defaultTeamID)
	groupStage := pipeline.NewGroupStage(repos.Alerts, sched, outbox, repos.Routes, stormState)
	silenceStage := pipeline.NewSilenceStage(repos.Silences, repos.Alerts)
	inhibitStage := pipeline.NewInhibitStage()

	pl := pipeline.New(st, eventQueue, hooks, []pipeline.Stage{
		dedupStage, mappingStage, routeStage, groupStage, silenceStage, inhibitStage,
	})

	// ---- 全局单例的数据保留清理任务（C5，第十个 kind）----
	retention := newRetentionHandler(sched, cfg.Retention)

	// ---- 风暴摘要 handler（design.md C3，第十一个 kind）：依赖任务 4/5 落地的
	// StormStateRepo/CountByRouteSince，路由信息复用 repos.Routes；GroupStage 的
	// storm 判定（任务 9）到期时会 ScheduleIfAbsent(storm_summary, routeID, ...)
	// 触发这里注册的 handler ----
	storm := newStormHandler(stormState, repos.Routes, repos.Alerts, sched)

	// ---- 六个既有 scheduler kind + 三个 ops kind + retention_sweep + storm_summary
	// 全部在这里绑定；未注册的 kind 到期会被标 failed 不重试，新增 kind 务必在此
	// 登记全（CLAUDE.md 不变量 2）----
	sched.RegisterHandler(notify.TimerKindAlertTTL, lifecycle.HandleTTL)
	sched.RegisterHandler(notify.TimerKindAlertRepeat, lifecycle.HandleRepeat)
	sched.RegisterHandler(oncall.TimerKindPersonnelEscalation, chain.Handle)
	sched.RegisterHandler(groupTimerKind, groupStage.Handle)
	sched.RegisterHandler(notify.TimerKindChannelEscalation, engine.Handle)
	// notify_send 是唯一注册成「事务外执行」模式的 kind：它内部必然做外部 IO
	// （HTTP/SMTP + 退避重试），不能占着 scheduler 的事务与 timers 行锁执行。
	// 语义见 notifySendPayloadHandler 与 scheduler.DetachedHandler 的文档。
	sched.RegisterDetachedHandler(notifySendTimerKind, notifySendPayloadHandler(sender))
	sched.RegisterHandler(ops.TimerKindDailyReport, opsHandlers.HandleDailyReport)
	sched.RegisterHandler(ops.TimerKindStaleSweep, opsHandlers.HandleStaleSweep)
	sched.RegisterHandler(ops.TimerKindOncallBroadcast, opsHandlers.HandleOncallBroadcast)
	sched.RegisterHandler(TimerKindRetentionSweep, retention.Handle)
	sched.RegisterHandler(TimerKindStormSummary, storm.Handle)

	return &App{
		Scheduler:        sched,
		Pipeline:         pl,
		Sender:           sender,
		Stopper:          stopper,
		Lifecycle:        lifecycle,
		CacheInvalidator: pipeline.NewConfigCacheInvalidator(routeStage, silenceStage),
		IncidentOutbox:   incidentOutbox,
		AlertProjector:   projector,
		ImpactProjector:  impactProjector,
	}
}

// toOpsSettings 把 store.OpsSettings（store 层为避免 store -> ops -> notify -> store
// 导入环而定义的镜像类型，见 internal/store/ops.go 文件头注释）逐字段转换成
// ops.Settings。Build 的 opsSettingsRepo 适配器与 ReconcileOpsTimers 都要做这个
// 转换，抽成一个函数避免两处字段列表分别漂移。
func toOpsSettings(s store.OpsSettings) ops.Settings {
	channels := make([]ops.ChannelTarget, len(s.Channels))
	for i, c := range s.Channels {
		channels[i] = ops.ChannelTarget{Kind: c.Kind, Target: c.Target}
	}
	return ops.Settings{
		TeamID:                    s.TeamID,
		Timezone:                  s.Timezone,
		DailyReportEnabled:        s.DailyReportEnabled,
		DailyReportTime:           s.DailyReportTime,
		StaleSweepEnabled:         s.StaleSweepEnabled,
		StaleSweepIntervalSeconds: s.StaleSweepIntervalSeconds,
		OncallBroadcastEnabled:    s.OncallBroadcastEnabled,
		OncallBroadcastTime:       s.OncallBroadcastTime,
		Channels:                  channels,
	}
}

// opsSettingsRepo 把 *store.OpsRepo 适配成 ops.SettingsRepo：字段转换用
// toOpsSettings，未找到配置转换成 ops.ErrSettingsNotFound（handler 据此判断"团队
// 从未保存过配置"，静默返回不续期，不当错误处理）。
type opsSettingsRepo struct{ repo *store.OpsRepo }

func (a opsSettingsRepo) GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*ops.Settings, error) {
	s, err := a.repo.GetSettings(ctx, tx, teamID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ops.ErrSettingsNotFound
	}
	if err != nil {
		return nil, err
	}
	settings := toOpsSettings(*s)
	return &settings, nil
}

// opsDailyStatsRepo 把 *store.OpsRepo 适配成 ops.DailyStatsRepo：只有 DailyStats
// 的返回类型需要转换（store.OpsDailyStats -> ops.DailyStats，字段一一对应，见
// internal/store/ops.go 镜像类型注释）；ListOpenLongerThan 双方签名相同，直接透传。
type opsDailyStatsRepo struct{ repo *store.OpsRepo }

func (a opsDailyStatsRepo) DailyStats(ctx context.Context, tx pgx.Tx, teamID string, dayStart, dayEnd time.Time) (ops.DailyStats, error) {
	s, err := a.repo.DailyStats(ctx, tx, teamID, dayStart, dayEnd)
	if err != nil {
		return ops.DailyStats{}, err
	}
	return ops.DailyStats{
		NewCount:        s.NewCount,
		ResolvedCount:   s.ResolvedCount,
		BySeverity:      s.BySeverity,
		MTTAAvg:         s.MTTAAvg,
		MTTAAvgValid:    s.MTTAAvgValid,
		MTTAMedian:      s.MTTAMedian,
		MTTAMedianValid: s.MTTAMedianValid,
		Prev7DayAvgNew:  s.Prev7DayAvgNew,
	}, nil
}

func (a opsDailyStatsRepo) ListOpenLongerThan(ctx context.Context, tx pgx.Tx, teamID string, olderThan time.Time) ([]domain.Alert, error) {
	return a.repo.ListOpenLongerThan(ctx, tx, teamID, olderThan)
}

// opsTeamRepo 把 *store.TeamRepo 适配成 ops.TeamRepo，只取展示名。
type opsTeamRepo struct{ repo *store.TeamRepo }

func (a opsTeamRepo) TeamName(ctx context.Context, tx pgx.Tx, teamID string) (string, error) {
	t, err := a.repo.GetByID(ctx, tx, teamID)
	if err != nil {
		return "", err
	}
	return t.Name, nil
}

// opsOncallResolver 把 *oncall.Resolver 适配成 ops.OncallResolver：组合
// Resolver.OnCallAt（求出 UserID 列表）与 *store.UserRepo.GetByID（UserID ->
// 展示名），使 ops 包不需要 import internal/oncall 或 internal/store 就能拿到
// BuildOncallBroadcast 需要的 []string（team-lead 任务 7 验收裁决 1 批准的设计）。
// 值班表未配置/该时刻存在班次空洞时 OnCallAt 返回空切片而非 error，这里原样传递
// 空 slice 给 handler——BuildOncallBroadcast 已处理"今日无值班安排"文案。
type opsOncallResolver struct {
	resolver *oncall.Resolver
	users    *store.UserRepo
}

func (a opsOncallResolver) ResolveOnCallNames(ctx context.Context, tx pgx.Tx, teamID string, at time.Time) ([]string, error) {
	entries, err := a.resolver.OnCallAt(ctx, tx, teamID, at)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		u, err := a.users.GetByID(ctx, tx, e.UserID)
		if err != nil {
			return nil, fmt.Errorf("app: 查询值班人员展示名失败(user=%s): %w", e.UserID, err)
		}
		names = append(names, u.DisplayName)
	}
	return names, nil
}

// userContactResolver 把 *store.UserRepo 适配成 notify.ContactResolver（任务 9）：
// email 渠道取 users.email，lark 渠道取 users.lark_open_id；对应字段为空、用户不存在
// 或渠道种类未知都归 notify.ErrNoContact——对发送层而言三种情况同样是"解析不出地址"，
// 不需要区分原因（用户不存在多半是脏引用/已删除账号，与"没填联系方式"处理口径一致）。
type userContactResolver struct {
	users *store.UserRepo
}

func (r userContactResolver) Resolve(ctx context.Context, tx pgx.Tx, userID, channelKind string) (string, error) {
	u, err := r.users.GetByID(ctx, tx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return "", notify.ErrNoContact
	}
	if err != nil {
		return "", fmt.Errorf("app: 查询用户联系方式失败(user=%s): %w", userID, err)
	}

	var addr string
	switch channelKind {
	case "email":
		addr = u.Email
	case "lark":
		addr = u.LarkOpenID
	default:
		return "", fmt.Errorf("app: 不支持解析渠道 %q 的用户联系方式", channelKind)
	}
	if addr == "" {
		return "", notify.ErrNoContact
	}
	return addr, nil
}

// ReconcileOpsTimers 是启动对账的入口（design.md R2）：扫描 team_ops_settings 里
// 任一周期任务开关为 true 的团队，对每个启用任务 ScheduleIfAbsent 补建 pending
// 定时器。用 ScheduleIfAbsent 而非 Schedule 是关键——已有 pending 行的 (kind,
// teamID) 不会被打断/重新排期（不打乱正在倒计时的 fire_at），只补建缺失的那些
// （比如首次上线、或运维误清过 timers 表）；被禁用的任务不出现在
// ListEnabledSettings 结果里，天然不会被重建，与"禁用即断链"的既有语义
// （handler 读到 !Enabled 直接返回不续期）一致。nextFire 计算复用 ops.NextFire——
// 与设置保存端点（任务 10）共用同一份函数，避免出现两套"下次触发时刻"公式。
//
// Build 本身是不接触 ctx/事务的纯同步装配函数，对账需要独立开一个事务，因此设计
// 成调用方（cmd/server/main.go、test/e2e/harness.go）在 Build 之后自行调用的
// 独立步骤，而不是塞进 Build 内部。
func ReconcileOpsTimers(ctx context.Context, st *store.Store, opsRepo *store.OpsRepo, sched *scheduler.Scheduler) error {
	return st.WithTx(ctx, func(tx pgx.Tx) error {
		settingsList, err := opsRepo.ListEnabledSettings(ctx, tx)
		if err != nil {
			return fmt.Errorf("app: 查询已启用运营配置失败: %w", err)
		}

		now := time.Now()
		for _, s := range settingsList {
			settings := toOpsSettings(s)
			tasks := []struct {
				enabled bool
				kind    string
			}{
				{s.DailyReportEnabled, ops.TimerKindDailyReport},
				{s.StaleSweepEnabled, ops.TimerKindStaleSweep},
				{s.OncallBroadcastEnabled, ops.TimerKindOncallBroadcast},
			}
			for _, task := range tasks {
				if !task.enabled {
					continue
				}
				// err 只表示时区解析失败已回退 UTC（仍可用），纯函数层不做日志
				// （见 ops.NextFire 文档），对账场景同样无需特殊处理。
				fireAt, _ := ops.NextFire(task.kind, settings, now)
				payload, err := ops.EncodePayload(fireAt)
				if err != nil {
					return err
				}
				if _, err := sched.ScheduleIfAbsent(ctx, tx, task.kind, s.TeamID, fireAt, payload); err != nil {
					return fmt.Errorf("app: 对账 ops 定时器失败(team=%s kind=%s): %w", s.TeamID, task.kind, err)
				}
			}
		}
		return nil
	})
}

// ReconcileRetentionTimer 是 retention_sweep 的启动对账入口（design.md C5），与
// ReconcileOpsTimers 同一模式：用 ScheduleIfAbsent 而非 Schedule，已存在 pending
// 记录时不打断正在倒计时的清理任务，只在从未注册过（首次上线、或运维误清过
// timers 表）时补建一条。retention_sweep 是全局单例任务（固定 refID="global"），
// 不像 ops 三个 kind 那样需要按团队枚举 settings，因此不需要查表，直接对账即可。
func ReconcileRetentionTimer(ctx context.Context, st *store.Store, sched *scheduler.Scheduler, cfg config.RetentionConfig) error {
	return st.WithTx(ctx, func(tx pgx.Tx) error {
		fireAt := time.Now().Add(cfg.SweepInterval)
		if _, err := sched.ScheduleIfAbsent(ctx, tx, TimerKindRetentionSweep, retentionSweepRefID, fireAt, nil); err != nil {
			return fmt.Errorf("app: 对账 retention_sweep 定时器失败: %w", err)
		}
		return nil
	})
}
