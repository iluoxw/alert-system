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
	"time"

	"alert-system/internal/config"
	"alert-system/internal/hook"
	"alert-system/internal/notify"
	"alert-system/internal/oncall"
	"alert-system/internal/pipeline"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// groupTimerKind 与 internal/pipeline/stage_group.go 内未导出的同名常量必须保持
// 一致（"group_wait"），字面量在装配层重复一次是因为 pipeline 包不导出它。
const groupTimerKind = "group_wait"

// Repos 是 Build 组装 pipeline/notify/oncall 依赖时用到的仓储子集。调用方各自按
// 无状态零参数构造出实际的 store.*Repo 实例后传入——两处装配的仓储构造本就完全
// 一致，这里只做一次收拢，避免 Build 的参数列表过长。routes/schedules/shifts 等
// 仅供路由/配置 API 使用的仓储不在这里，继续由调用方自己持有。
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
}

// App 是 Build 组装完毕后调用方需要的公共依赖：Scheduler/Pipeline 供调用方决定
// 何时启动后台 worker（go app.Scheduler.Run(ctx)/go app.Pipeline.Run(ctx)）；
// Stopper/Lifecycle 供 api.NewServer/router.RegisterAlertRoutes 注入，保证飞书卡片
// 回调与 REST API 两条处置路径共用同一套"停止升级链/生命周期定时器"实现。
type App struct {
	Scheduler *scheduler.Scheduler
	Pipeline  *pipeline.Pipeline
	Sender    *notify.Sender
	Stopper   CompositeStopper
	Lifecycle *notify.LifecycleHandlers
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

	// outbox 依赖 chain/engine，chain/engine 又依赖 outbox 作为通知意图的实现——
	// 用零值字段构造 outbox 打破这个环：NotifyEscalation/NotifyRepeat/DispatchStep
	// 不 touch o.engine/o.chain，真正用到它们的 NotifyGroup 要到 pipeline 起转后才会
	// 被调用，那时 engine/chain 已经赋值完毕。
	outbox := &notifyOutbox{
		scheduler:      sched,
		repeatInterval: cfg.RepeatInterval,
		now:            time.Now,
	}

	resolver := oncall.NewResolver(repos.Schedules, repos.Shifts)
	chain := oncall.NewChain(repos.Alerts, repos.Timeline, repos.EscalationPolicies, resolver, sched, outbox)
	engine := notify.NewEngine(repos.Alerts, repos.Timeline, repos.NotificationPolicies, sched, outbox)
	outbox.engine = engine
	outbox.chain = chain

	stopper := NewCompositeStopper(chain, engine)
	lifecycle := notify.NewLifecycleHandlers(repos.Alerts, repos.Timeline, sched, stopper, outbox, cfg.RepeatInterval)

	// ---- 管道 stage：顺序 dedup -> route -> group -> silence -> inhibit（route 必须
	// 先于 group——group stage 读取 pctx.Route.GroupBy/GroupWait/GroupInterval，这三个
	// 字段由 route stage 填充）----
	eventQueue := store.NewEventQueue()
	hooks := hook.New()
	dedupStage := pipeline.NewDedupStage(repos.Alerts, repos.Timeline, sched, nil)
	routeStage := pipeline.NewRouteStage(repos.Routes, repos.Alerts, defaultTeamID)
	groupStage := pipeline.NewGroupStage(repos.Alerts, sched, outbox)
	silenceStage := pipeline.NewSilenceStage(repos.Silences, repos.Alerts)
	inhibitStage := pipeline.NewInhibitStage()

	pl := pipeline.New(st, eventQueue, hooks, []pipeline.Stage{
		dedupStage, routeStage, groupStage, silenceStage, inhibitStage,
	})

	// ---- 六个 scheduler kind 全部在这里绑定 ----
	sched.RegisterHandler(notify.TimerKindAlertTTL, lifecycle.HandleTTL)
	sched.RegisterHandler(notify.TimerKindAlertRepeat, lifecycle.HandleRepeat)
	sched.RegisterHandler(oncall.TimerKindPersonnelEscalation, chain.Handle)
	sched.RegisterHandler(groupTimerKind, groupStage.Handle)
	sched.RegisterHandler(notify.TimerKindChannelEscalation, engine.Handle)
	sched.RegisterHandler(notifySendTimerKind, notifySendPayloadHandler(sender))

	return &App{
		Scheduler: sched,
		Pipeline:  pl,
		Sender:    sender,
		Stopper:   stopper,
		Lifecycle: lifecycle,
	}
}
