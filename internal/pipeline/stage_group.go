package pipeline

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// groupTimerKind 是分组 stage 在 scheduler 里注册"组到期"定时器的 kind，ref_id 是
// group_key（migrations/000003_aux.up.sql 对 timers.ref_id 的注释已预留这个用法）。
// 与 dedup 的 "alert_ttl"、oncall 的 "personnel_escalation" 分属不同命名空间。
// 组首等待（group_wait）与组内后续更新的节流窗口（group_interval）复用同一个 kind：
// 二者的区别只是 fire_at 的延迟长度不同，见 Process 里对 HasFired 的判断。
const groupTimerKind = "group_wait"

// stormSummaryTimerKind 是风暴摘要的 timer kind（design.md C3，第 11 个 kind），
// refID 固定用 routeID。字面量必须与 internal/app/storm.go 的
// TimerKindStormSummary 保持一致——两包各自维护一份未导出/局部常量，字面量重复
// 是因为 pipeline 包不导出它（与 internal/app 对 groupTimerKind 的处理同一手法）。
const stormSummaryTimerKind = "storm_summary"

// stormCountWindow 是风暴判定"近期新告警数"的统计窗口，与 storm_summary handler
// 的速率复查窗口同量级（internal/app/storm.go）。
const stormCountWindow = time.Minute

// stormSummaryDelay 是首次进入风暴熔断时注册 storm_summary 定时器的延迟。
const stormSummaryDelay = time.Minute

// GroupAlertRepo 是 GroupStage 依赖的告警仓储子集：UpdateRouting 复用路由 stage
// 已有的方法写回 group_key（透传 pctx.Alert.TeamID/RouteID 防止误清空 team_id/
// route_id——与路由 stage 透传 GroupKey 是对称操作）；ListByGroupKey 供到期
// handler 取出组内全部活跃成员交给 GroupNotifier；CountByRouteSince 供 Handle 的
// 风暴判定统计该路由近 stormCountWindow 内的新告警数（R3/C3）。
type GroupAlertRepo interface {
	RouteAlertRepo
	ListByGroupKey(ctx context.Context, tx pgx.Tx, groupKey string) ([]domain.Alert, error)
	CountByRouteSince(ctx context.Context, tx pgx.Tx, routeID string, since time.Time) (int, error)
}

// GroupRouteRepo 是 GroupStage.Handle 依赖的路由仓储子集：取该路由的
// storm_threshold_per_min 配置（R3/C3），由 store.RouteRepo 实现（结构化满足）。
type GroupRouteRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error)
}

// GroupStormRepo 是 GroupStage.Handle 依赖的风暴状态仓储子集，由
// store.StormStateRepo 实现（结构化满足）。Get 无历史记录时返回 (nil, nil)（不是
// ErrNotFound）；Activate 对已 active 的路由重复调用是安全的（不重置
// suppressed_count）；AddSuppressed 是相对累加。MarkSummary/Deactivate 是
// storm_summary handler（internal/app，任务 10）的职责，本 stage 不需要。
type GroupStormRepo interface {
	Get(ctx context.Context, tx pgx.Tx, routeID string) (*store.StormState, error)
	Activate(ctx context.Context, tx pgx.Tx, routeID string, now time.Time) error
	AddSuppressed(ctx context.Context, tx pgx.Tx, routeID string, n int) error
}

// GroupTimerScheduler 是 GroupStage 依赖的定时器子集，由 scheduler.Scheduler 实现。
// ScheduleIfAbsent 保证"组内新告警不重置已注册的组定时器基准时间"；HasFired 用于
// 区分某个 group_key 是组首（从未触发过，走 group_wait）还是组内后续更新（此前已
// 触发过至少一次，走 group_interval 节流）。
type GroupTimerScheduler interface {
	ScheduleIfAbsent(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) (bool, error)
	HasFired(ctx context.Context, tx pgx.Tx, kind, refID string) (bool, error)
}

// GroupNotifier 是分组 stage"组到期，应聚合发送"这一通知意图与实际发送之间的解耦
// 接口——本包与任务 22/24 的契约，模式与 internal/oncall.Notifier 一致。
//
// 硬约束（对齐 scheduler.Handler 的文档：handler 在 pollOnce 的数据库事务内执行，
// 期间持有 timers 行锁）：NotifyGroup 的实现绝不能同步做外部 IO（HTTP/短信等）或
// 调用 notify.Sender.Send（同步阻塞 + 指数退避重试），否则会拖长 Handle 所在事务
// 与行锁的持有时间。实现应当只做快速的本地状态写入，真正的聚合发送由该实现自己
// 在事务之外异步完成。真实实现由任务 22/24 提供；本包测试用 fake 实现验证调用参数。
type GroupNotifier interface {
	NotifyGroup(ctx context.Context, tx pgx.Tx, groupKey string, alerts []domain.Alert) error
}

// groupClock 抽象"当前时间"，供测试注入 fake clock 精确控制定时器基准时间，
// 与 dedupClock/routeClock 同一模式。
type groupClock interface {
	Now() time.Time
}

type realGroupClock struct{}

func (realGroupClock) Now() time.Time { return time.Now() }

// GroupStage 是管道的分组 stage（R9，组键计算的三分支见 aiops-foundation R1/R2 扩展，
// 详见 Process 内注释）：
//
//   - route.GroupMode == "window" 时窗口全归并：该路由所有告警不论 fingerprint/
//     labels 归入同一个固定组（group_key 见 windowGroupKey）；
//
//   - 否则 route 未配置 group_by 时，仍按 Alertmanager 语义形成"单例组"（该 route+
//     该告警独占一个组，group_key 见 singletonGroupKey）；
//
//   - 否则配置了 group_by 时，group_key = route_id + hash(group_by 取值)（见
//     computeGroupKey），保留键 __template__ 取 domain.TemplateFingerprint(alert.Title)
//     而非 label 值，可与普通 label 键混用；
//
//     三种 group_key 都写回 alerts.group_key，继续走完全相同的
//     UpdateRouting/HasFired/ScheduleIfAbsent 流程——group_wait/group_interval 依旧
//     生效，不存在脱离组机制之外的"直发"路径；组首（该 group_key 从未触发过定时器）
//     注册 group_wait 延迟的到期定时器，组内后续加入者若已有 pending 定时器则不重置
//     其 fire_at（ScheduleIfAbsent 的语义），保证升级序列以组首时间为基准；组已完成
//     过至少一轮到期通知后的后续更新改用 group_interval 延迟注册（节流）；
//
//   - resolved 告警不再入组、不注册新定时器，但不清空其 group_key（组视图仍需
//     看到已恢复成员）；
//
//   - 零值 Alert（迟到 resolved 未命中现存告警）直接跳过。
//
// 组状态的唯一真相是 alerts.group_key 列 + timers 表，本 stage 不维护任何内存态。
type GroupStage struct {
	alerts    GroupAlertRepo
	scheduler GroupTimerScheduler
	notifier  GroupNotifier
	routes    GroupRouteRepo
	storms    GroupStormRepo
	clock     groupClock
}

// NewGroupStage 构造 GroupStage。routes/storms 供 Handle 的风暴熔断判定（R3/C3）
// 使用；routes 取 storm_threshold_per_min 配置，storms 读写 route_storm_state。
func NewGroupStage(alerts GroupAlertRepo, sched GroupTimerScheduler, notifier GroupNotifier, routes GroupRouteRepo, storms GroupStormRepo) *GroupStage {
	return &GroupStage{
		alerts:    alerts,
		scheduler: sched,
		notifier:  notifier,
		routes:    routes,
		storms:    storms,
		clock:     realGroupClock{},
	}
}

// Name 返回 "group"。
func (s *GroupStage) Name() string { return "group" }

// Process 实现 Stage 接口。
func (s *GroupStage) Process(ctx context.Context, pctx *PipelineContext) error {
	// 迟到的 resolved 事件在去重 stage 里因找不到现存告警而被忽略时，pctx.Alert
	// 仍是零值（ID==""）：没有告警可分组，直接跳过。
	if pctx.Alert.ID == "" {
		return nil
	}
	// resolved 告警不再入组、不注册新定时器；不清空 group_key，保留组视图里
	// 已恢复成员的可见性（与 dedup/路由 stage 对迟到 resolved 的处理口径一致：
	// 只跳过后续动作，不回退已经落库的字段）。
	if pctx.Alert.Status == domain.StatusResolved {
		return nil
	}

	route := pctx.Route
	var groupKey string
	switch {
	case route.GroupMode == "window":
		// 窗口全归并模式（R1/C1）：该路由窗口内所有告警不论 fingerprint/labels 全部
		// 归入同一个固定组，用路由 ID 区分不同路由。GroupMode 是防御性判断——空串/
		// "label"/任何未识别的值都不落入本分支，一律走下面的既有 label 语义路径。
		groupKey = windowGroupKey(route.RouteID)
	case len(route.GroupBy) == 0:
		// 未配置 group_by：与真实 Alertmanager 一致，这不是一条独立的"直发"路径，
		// 而是退化为"每条告警各自一个单例组"——下面完全复用分组路径的
		// UpdateRouting/HasFired/ScheduleIfAbsent 逻辑，group_wait/group_interval
		// 对未分组路由同样生效。
		groupKey = singletonGroupKey(route.RouteID, pctx.Alert.Fingerprint)
	default:
		groupKey = computeGroupKey(route.RouteID, route.GroupBy, groupByLabels(pctx.Alert))
	}

	if pctx.Alert.GroupKey != groupKey {
		if err := s.alerts.UpdateRouting(ctx, pctx.Tx, pctx.Alert.ID, pctx.Alert.TeamID, groupKey, pctx.Alert.RouteID); err != nil {
			return fmt.Errorf("group: 写入 group_key 失败: %w", err)
		}
		pctx.Alert.GroupKey = groupKey
	}

	// flapping 告警跳过组定时器注册（R4/C4）：group_key 仍照常写入（组视图需要看到
	// 抖动中的成员），只是不再驱动/续期组到期定时器，避免每次翻转都重置升级基准或
	// 触发新一轮通知。FlappingStarted 是例外——本轮去重刚把 flapping 从 false 置为
	// true 那一次事件，仍要照常注册，让"进入抖动"这一次组通知能送出去（Handle 里
	// 该次通知会把这条告警的 Flapping 标注出来，见 Handle 文档）；此后同一告警在
	// 抖动窗口内的后续事件才真正跳过。
	if pctx.Alert.Flapping && !pctx.FlappingStarted {
		return nil
	}

	hasFired, err := s.scheduler.HasFired(ctx, pctx.Tx, groupTimerKind, groupKey)
	if err != nil {
		return fmt.Errorf("group: 查询组历史触发状态失败: %w", err)
	}
	delay := route.GroupWait
	if hasFired {
		delay = route.GroupInterval
	}
	fireAt := s.clock.Now().Add(delay)
	if _, err := s.scheduler.ScheduleIfAbsent(ctx, pctx.Tx, groupTimerKind, groupKey, fireAt, nil); err != nil {
		return fmt.Errorf("group: 注册组到期定时器失败: %w", err)
	}
	return nil
}

// Handle 实现 scheduler.Handler：refID 是 group_key。tx 是 scheduler 传入的事务
// （同一事务内已经把这条定时器标记为 fired），取组内全部活跃（非 resolved）成员、
// 把"该组到期，应聚合发送"的意图交给 GroupNotifier 都复用这个 tx——不做实际发送
// （见 GroupNotifier 文档的硬约束）。组内成员已全部 resolved/迁移出组时无需通知，
// 不算错误。
//
// R4/C4 flapping 过滤：先剔除仍在抖动中的成员算出"有意义可通知"的子集，滤后为空
// （组内全是抖动中的告警，没有值得播报的内容）则整组跳过通知；只要还有非抖动成员，
// 传给 NotifyGroup 的仍是完整的原始 members（含抖动中的），而不是过滤后的子集——
// 这样"进入抖动"当次那条通知（Process 里 FlappingStarted 例外放行的那次）才能把
// 这条告警带到消息里，供 NotifyGroup 的真实实现（任务 22/24）在构造 Message 时按
// alert.Flapping 标注"抖动中"（接口不改，domain.Alert 已带 Flapping 字段随
// members 一并传递）。filtered 只用作"是否还有必要通知"的判断依据，不改变发给
// NotifyGroup 的实参——这一点与字面理解可能存在歧义，已在交付报告里向 team-lead
// 说明取舍理由。
//
// R3/C3 风暴熔断：仅当该组关联的路由（同一 group_key 下所有成员的 RouteID 均相同，
// 取 members[0] 即可，见三种 group_key 格式都以 routeID 为前缀）非空且
// storm_threshold_per_min>0 时才判定；routeID 为空（默认团队兜底路径无路由）直接
// 跳过风暴判定，不影响上面的 flapping 过滤。判定：近 stormCountWindow 内该路由新
// 告警数达到阈值，或风暴状态已是 active（即使当前速率已回落，也维持熔断直到
// storm_summary handler 的速率复查解除），则跳过 NotifyGroup，改为 Activate（对已
// active 的路由重复调用是安全的幂等操作）+ AddSuppressed(len(filtered))（用滤后
// 的非抖动成员数计数——抖动中的成员本就不计划通知，不算被风暴"额外"抑制）+
// ScheduleIfAbsent(stormSummaryTimerKind, routeID, ...)。CountByRouteSince 查询失败
// 是本函数唯一的 fail-open 分支：不能让一次统计故障吞掉该组通知，因此记日志后跳过
// 风暴判定、落到末尾的正常通知路径，并返回 nil——**绝不能把这个错误向上返回**：
// scheduler 对 handler 错误的语义是整体回滚（NotifyGroup 的 Schedule 意图一并撤销）
// 后把定时器标 failed 且不重试（见 scheduler.markFailed 注释），返回错误反而使通知
// 永久丢失，与 fail-open 的意图正好相反。风暴状态 Get 失败按普通错误处理（不
// fail-open），与"仅计数查询 fail-open"的既定口径一致。
func (s *GroupStage) Handle(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	members, err := s.alerts.ListByGroupKey(ctx, tx, refID)
	if err != nil {
		return fmt.Errorf("group: 查询组内成员失败: %w", err)
	}
	if len(members) == 0 {
		return nil
	}

	filtered := filterFlapping(members)
	if len(filtered) == 0 {
		return nil
	}

	if routeID := members[0].RouteID; routeID != "" {
		route, err := s.routes.GetByID(ctx, tx, routeID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// 路由在组定时器 pending 期间被管理员删除：storm 阈值配置已不存在，
			// 视同未配置（零值路由），跳过风暴判定落到下面的正常通知——返回 error
			// 会把本定时器焊死成 failed（scheduler 不重试），组通知永久丢失。
			route = &store.Route{}
		case err != nil:
			return fmt.Errorf("group: 查询路由失败: %w", err)
		}
		if route.StormThresholdPerMin > 0 {
			since := s.clock.Now().Add(-stormCountWindow)
			count, err := s.alerts.CountByRouteSince(ctx, tx, routeID, since)
			if err != nil {
				// fail-open：计数故障时记日志、跳过风暴判定，落到下面的正常通知路径。
				// 这里返回 error 会让 NotifyGroup 意图随事务回滚、定时器被标 failed
				// 不重试（scheduler 语义），通知反而永久丢失——所以只能吞掉。
				log.Printf("group: 查询路由风暴计数失败（fail-open 放行本次通知，group_key=%s route=%s）: %v", refID, routeID, err)
			} else {
				state, err := s.storms.Get(ctx, tx, routeID)
				if err != nil {
					return fmt.Errorf("group: 查询风暴状态失败: %w", err)
				}
				if count >= route.StormThresholdPerMin || (state != nil && state.Active) {
					now := s.clock.Now()
					if err := s.storms.Activate(ctx, tx, routeID, now); err != nil {
						return fmt.Errorf("group: 激活风暴熔断状态失败: %w", err)
					}
					if err := s.storms.AddSuppressed(ctx, tx, routeID, len(filtered)); err != nil {
						return fmt.Errorf("group: 累计风暴抑制计数失败: %w", err)
					}
					if _, err := s.scheduler.ScheduleIfAbsent(ctx, tx, stormSummaryTimerKind, routeID, now.Add(stormSummaryDelay), nil); err != nil {
						return fmt.Errorf("group: 注册风暴摘要定时器失败: %w", err)
					}
					return nil
				}
			}
		}
	}

	if err := s.notifier.NotifyGroup(ctx, tx, refID, members); err != nil {
		return fmt.Errorf("group: 记录组聚合通知意图失败: %w", err)
	}
	return nil
}

// filterFlapping 剔除仍在抖动中的成员，返回"值得通知"的子集（R4/C4）。未配置
// flapping 阈值时 domain.Alert.Flapping 恒为 false，本函数原样返回全部成员——对
// 未开启 R4 的路由零行为变化。
func filterFlapping(members []domain.Alert) []domain.Alert {
	filtered := make([]domain.Alert, 0, len(members))
	for _, m := range members {
		if !m.Flapping {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// groupKeySep 是 computeGroupKey 哈希输入中取值之间的分隔符，避免相邻取值拼接
// 产生歧义（与 domain.Fingerprint 的 sep 同一手法，取值不同仅因两者是不同包、
// 互不依赖对方的私有常量）。
const groupKeySep = '\x1e'

// singletonGroupKey 计算未配置 group_by 路由的单例组键：route_id + "-single-" +
// fingerprint，保证同一路由下每条告警各自独占一个组（同一告警的后续更新复用
// 同一个 key，从而落到同一个 group_wait/group_interval 定时器上）。"-single-"
// 中缀与 computeGroupKey 的 "%s-%016x" 十六进制哈希格式不会碰撞。
func singletonGroupKey(routeID, fingerprint string) string {
	return fmt.Sprintf("%s-single-%s", routeID, fingerprint)
}

// windowGroupKey 计算窗口全归并模式（R1/C1）的固定组键：route_id + "-window"，
// 不掺入 fingerprint 或任何标签取值——这正是"该路由窗口内全部告警不论差异归同一
// 组"的语义。"-window" 后缀与 singletonGroupKey 的 "-single-" 中缀、computeGroupKey
// 的 "%s-%016x" 十六进制哈希格式都不会碰撞（三者格式互斥，见本文件测试的显式断言）。
func windowGroupKey(routeID string) string {
	return routeID + "-window"
}

// templateGroupByKey 是 group_by 里的保留键名：其取值不查 labels，而是取
// domain.TemplateFingerprint(alert.Title)（aiops-foundation R2/C2 的模板归一化
// 指纹），供"同模板不同变量的告警归并到同一组"这一场景使用。
const templateGroupByKey = "__template__"

// groupByLabels 构造 group_by 求值用的标签视图：在 mergedMatchLabels（ResourceRef
// 打底、Labels 覆盖）基础上，把保留键 __template__ 的取值固定改写为
// domain.TemplateFingerprint(alert.Title)——即使 alert 本身恰好带有一个真实业务
// 标签也叫 __template__，也会被这里覆盖，因为 __template__ 在 group_by 语义里是
// 保留名，不能与业务标签混淆（mergedMatchLabels 每次调用都返回新 map，这里原地
// 覆盖不会影响调用方持有的其他引用）。
func groupByLabels(alert domain.Alert) map[string]string {
	labels := mergedMatchLabels(alert)
	labels[templateGroupByKey] = domain.TemplateFingerprint(alert.Title)
	return labels
}

// computeGroupKey 计算 group_key = route_id + "-" + hash(group_by 标签值)：按
// route.GroupBy 声明的顺序，从 labels 里取出对应值（缺失的 key 取空字符串），
// 逐个以 groupKeySep 分隔写入 FNV-64a。以 route_id 作前缀是为了避免不同路由碰巧
// 算出相同标签值组合时的 group_key 冲突；hash 只覆盖标签值本身、不含标签名，
// 因为同一路由的 group_by 组合在其生命周期内是固定的（来自 routes.group_by
// 配置列，路由规则一变整批告警的 group_key 计算口径就一起变，不存在新旧口径
// 混杂的问题）。labels 入参用 RouteStage 同款的 mergedMatchLabels（ResourceRef
// 打底、Labels 覆盖），与匹配路由时使用的标签视图保持一致。
func computeGroupKey(routeID string, groupBy []string, labels map[string]string) string {
	h := fnv.New64a()
	for _, k := range groupBy {
		h.Write([]byte(labels[k]))
		h.Write([]byte{groupKeySep})
	}
	return fmt.Sprintf("%s-%016x", routeID, h.Sum64())
}
