package pipeline

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// groupTimerKind 是分组 stage 在 scheduler 里注册"组到期"定时器的 kind，ref_id 是
// group_key（migrations/000003_aux.up.sql 对 timers.ref_id 的注释已预留这个用法）。
// 与 dedup 的 "alert_ttl"、oncall 的 "personnel_escalation" 分属不同命名空间。
// 组首等待（group_wait）与组内后续更新的节流窗口（group_interval）复用同一个 kind：
// 二者的区别只是 fire_at 的延迟长度不同，见 Process 里对 HasFired 的判断。
const groupTimerKind = "group_wait"

// GroupAlertRepo 是 GroupStage 依赖的告警仓储子集：UpdateRouting 复用路由 stage
// 已有的方法写回 group_key（透传 pctx.Alert.TeamID 防止误清空 team_id——与路由
// stage 透传 GroupKey 是对称操作）；ListByGroupKey 供到期 handler 取出组内全部
// 活跃成员交给 GroupNotifier。
type GroupAlertRepo interface {
	RouteAlertRepo
	ListByGroupKey(ctx context.Context, tx pgx.Tx, groupKey string) ([]domain.Alert, error)
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

// txRunner 抽象 store.Store.WithTx。scheduler.Handler 签名不带 tx 参数（handler 在
// pollOnce 自己的事务内被调用，但没有拿到那个 tx 的引用），所以 GroupStage.Handle
// 必须自己开一个独立的短事务完成"取组内成员 + 记录聚合发送意图"，与
// internal/oncall.Chain 的 txRunner 是同一模式。
type txRunner interface {
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// groupClock 抽象"当前时间"，供测试注入 fake clock 精确控制定时器基准时间，
// 与 dedupClock/routeClock 同一模式。
type groupClock interface {
	Now() time.Time
}

type realGroupClock struct{}

func (realGroupClock) Now() time.Time { return time.Now() }

// GroupStage 是管道的分组 stage（R9）：
//   - route 未配置 group_by 时直发：不分组、不写 group_key、不注册定时器；
//   - 配置了 group_by 时，group_key = route_id + hash(group_by 标签值)（见
//     computeGroupKey），写回 alerts.group_key；组首（该 group_key 从未触发过
//     定时器）注册 group_wait 延迟的到期定时器，组内后续加入者若已有 pending
//     定时器则不重置其 fire_at（ScheduleIfAbsent 的语义），保证升级序列以组首
//     时间为基准；组已完成过至少一轮到期通知后的后续更新改用 group_interval
//     延迟注册（节流）；
//   - resolved 告警不再入组、不注册新定时器，但不清空其 group_key（组视图仍需
//     看到已恢复成员）；
//   - 零值 Alert（迟到 resolved 未命中现存告警）直接跳过。
//
// 组状态的唯一真相是 alerts.group_key 列 + timers 表，本 stage 不维护任何内存态。
type GroupStage struct {
	tx        txRunner
	alerts    GroupAlertRepo
	scheduler GroupTimerScheduler
	notifier  GroupNotifier
	clock     groupClock
}

// NewGroupStage 构造 GroupStage。tx 通常直接传 *store.Store（结构上满足 txRunner），
// 供 Handle 在到期时开独立事务使用。
func NewGroupStage(tx txRunner, alerts GroupAlertRepo, sched GroupTimerScheduler, notifier GroupNotifier) *GroupStage {
	return &GroupStage{
		tx:        tx,
		alerts:    alerts,
		scheduler: sched,
		notifier:  notifier,
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
	if len(route.GroupBy) == 0 {
		return s.directDispatch(ctx, pctx)
	}

	groupKey := computeGroupKey(route.RouteID, route.GroupBy, mergedMatchLabels(pctx.Alert))
	if pctx.Alert.GroupKey != groupKey {
		if err := s.alerts.UpdateRouting(ctx, pctx.Tx, pctx.Alert.ID, pctx.Alert.TeamID, groupKey); err != nil {
			return fmt.Errorf("group: 写入 group_key 失败: %w", err)
		}
		pctx.Alert.GroupKey = groupKey
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

// directDispatch 处理"未配置 group_by"分支：告警不分组，若此前曾经属于某个组
// （例如路由规则被改配置，去掉了 group_by），清空其 group_key，使其走单告警
// 通知路径（下游任务 22 处理），不注册/触碰任何组定时器。
func (s *GroupStage) directDispatch(ctx context.Context, pctx *PipelineContext) error {
	if pctx.Alert.GroupKey == "" {
		return nil
	}
	if err := s.alerts.UpdateRouting(ctx, pctx.Tx, pctx.Alert.ID, pctx.Alert.TeamID, ""); err != nil {
		return fmt.Errorf("group: 清空 group_key 失败: %w", err)
	}
	pctx.Alert.GroupKey = ""
	return nil
}

// Handle 实现 scheduler.Handler：refID 是 group_key。独立开一个短事务取出组内
// 全部活跃（非 resolved）成员，把"该组到期，应聚合发送"的意图交给 GroupNotifier——
// 不做实际发送（见 GroupNotifier 文档的硬约束）。组内成员已全部 resolved/迁移
// 出组时无需通知，不算错误。
func (s *GroupStage) Handle(ctx context.Context, refID string, payload []byte) error {
	return s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		members, err := s.alerts.ListByGroupKey(ctx, tx, refID)
		if err != nil {
			return fmt.Errorf("group: 查询组内成员失败: %w", err)
		}
		if len(members) == 0 {
			return nil
		}
		if err := s.notifier.NotifyGroup(ctx, tx, refID, members); err != nil {
			return fmt.Errorf("group: 记录组聚合通知意图失败: %w", err)
		}
		return nil
	})
}

// groupKeySep 是 computeGroupKey 哈希输入中取值之间的分隔符，避免相邻取值拼接
// 产生歧义（与 domain.Fingerprint 的 sep 同一手法，取值不同仅因两者是不同包、
// 互不依赖对方的私有常量）。
const groupKeySep = '\x1e'

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
