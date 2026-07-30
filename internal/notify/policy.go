package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// TimerKindChannelEscalation 是渠道升级引擎在 scheduler 里注册定时器的 kind，
// refID 是 alert.ID（本任务以单告警维度实现，见 Engine.Start 的文档）。与 dedup 的
// "alert_ttl"、oncall 的 "personnel_escalation"、分组 stage 的 "group_wait" 分属不同
// 命名空间——渠道升级（本引擎）与人员升级（oncall）是两个正交维度，彼此不感知对方
// 的 kind，只都由「超时未 Ack」驱动、被 Ack 终止（R13 与 R5.4 的关系说明）。
const TimerKindChannelEscalation = "channel_escalation"

// modeSpread 是 store.NotificationStep.Mode 的"扩散"取值（R13.2）：本级渠道叠加到
// 上一级已生效的渠道集合上。除 modeSpread 外的任何取值（含 "replace" 与未识别的
// 空值/拼写错误）一律按"替换"处理——替换是更安全的默认：叠加是显式选择，配置写错
// 时退回替换不会让通知面意外扩大。
const modeSpread = "spread"

// ErrNoPolicy 表示 R13.5 的三级求值链（绑定 matcher -> 团队默认 -> 全局默认）全部
// 未命中：告警没有可用的渠道升级策略。Start 遇到此错误视为正常的"不启动升级"
// （告警仍走管道其余流程，只是没有渠道升级），不是需要向上传播的失败。
var ErrNoPolicy = errors.New("notify: 未找到适用的通知策略")

// stepPayload 是渠道升级定时器 payload 的 JSON 结构：只携带"当前推进到第几级"
// （从 0 开始，对应 NotificationStep 序列下标），其余状态（告警当前状态、团队、
// 策略本身）都在 Handle 触发时重新查询，避免 payload 里的快照与数据库当前状态
// 不一致（例如策略在升级进行中被管理端修改）——与 oncall.escalationPayload 同一模式。
type stepPayload struct {
	Step int `json:"step"`
}

// AlertRepo 是 Engine 依赖的告警仓储子集，由 store.AlertRepo 实现。
type AlertRepo interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error)
}

// TimelineRepo 是 Engine 依赖的时间线仓储子集，由 store.TimelineRepo 实现。
// ListByAlert 除供外部展示外，还被 Engine 自身用作幂等判据（见 alreadyHandledStep）。
type TimelineRepo interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
	ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error)
}

// PolicyRepo 是 Engine 依赖的通知策略仓储子集，由 store.NotificationPolicyRepo 实现。
type PolicyRepo interface {
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.NotificationPolicy, error)
	GetGlobalDefault(ctx context.Context, tx pgx.Tx) (*store.NotificationPolicy, error)
}

// TimerScheduler 是 Engine 依赖的定时器子集，由 scheduler.Scheduler 实现。
type TimerScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
	Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error
}

// Clock 抽象"当前时间"，与 scheduler.Clock/oncall.Clock 同一模式，供测试注入 fake
// clock 精确控制升级序列的等待时长，无需真的等待 wall-clock 时间流逝。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Dispatcher 是渠道升级"通知意图"与"实际发送"之间的解耦接口——本包与任务 24 的
// 契约，模式与 internal/oncall.Notifier、internal/pipeline.GroupNotifier 完全一致
// （三者刻意同形，便于任务 24 用同一套发送编排/outbox 机制适配三个调用点）。
//
// 硬约束（对齐 scheduler.Handler 的文档：handler 在 pollOnce 的数据库事务内执行，
// 期间持有 timers 行锁）：DispatchStep 的实现绝不能同步做外部 IO 或调用
// notify.Sender.Send（同步阻塞 + 指数退避重试，单次可能耗时数秒），否则会拖长
// Engine.advance 所在事务与行锁的持有时间。实现应当只做快速的本地状态写入（如落一条
// 待发送的 outbox 记录），真正的发送由该实现自己在事务之外异步完成。真实实现由
// 任务 24（发送器接入）提供；本包测试用 fake 实现验证调用参数。
type Dispatcher interface {
	// DispatchStep 记录"告警 alert 的渠道升级推进到第 step 级，应发往 channels"这一
	// 意图。step 从 0 开始，与策略 Steps 序列下标一致；tx 是 Engine.advance 内部事务，
	// 实现若需要落库应复用这个 tx，与本次升级的时间线记录/下一级定时器注册原子提交。
	DispatchStep(ctx context.Context, tx pgx.Tx, alert domain.Alert, step int, channels []string) error
}

// Engine 是渠道升级策略引擎（R13）：三级求值链选出通知策略，按告警 severity 取对应
// 的渠道升级序列，经 scheduler 逐级推进；未 Ack 前逐级推进，Ack/Resolved/静默命中
// 后停止；每级的渠道集合按 replace/spread 模式计算，只提高触达强度，不改变告警的
// severity/status，全过程记入时间线（R13.3）。
type Engine struct {
	alerts     AlertRepo
	timeline   TimelineRepo
	policies   PolicyRepo
	scheduler  TimerScheduler
	dispatcher Dispatcher
	clock      Clock
}

// NewEngine 构造 Engine。
func NewEngine(alerts AlertRepo, timeline TimelineRepo, policies PolicyRepo, sched TimerScheduler, dispatcher Dispatcher) *Engine {
	return &Engine{
		alerts:     alerts,
		timeline:   timeline,
		policies:   policies,
		scheduler:  sched,
		dispatcher: dispatcher,
		clock:      realClock{},
	}
}

// Start 为 alert 启动渠道升级序列：求值通知策略（resolvePolicy）、取该 severity 对应
// 的升级序列，用序列第 0 级的等待时长注册首个定时器（Handle 到期后统一走 advance，
// 第 0 级与后续级别用同一套推进逻辑，不特殊处理"立即发送"）。策略未命中
// （ErrNoPolicy）或该 severity 未配置升级序列时不启动，不是错误——告警仍走管道
// 其余流程，只是没有渠道升级。由路由完成后的调用方（任务 25/26 的编排逻辑）在写入
// alert 的同一事务内调用。
//
// baseTime 为零值时使用 e.clock.Now() 作为基准时间；非零值是"组维度驱动时以组首
// 时间为基准"这一硬约束的接口缝：分组 stage（任务 14）的定时器 kind/payload 契约
// 尚未定型，本引擎不实现组维度求值本身（仍按单个 alert 求策略、按 alert.ID 注册
// 定时器），但预留这个参数，供未来组维度调用方（任务 24/27）传入组首告警到达时间
// 而非当前时间，使升级序列的等待时长以组首为基准计算——回报里对此有专门说明。
func (e *Engine) Start(ctx context.Context, tx pgx.Tx, alert domain.Alert, baseTime time.Time) error {
	policy, err := e.resolvePolicy(ctx, tx, alert)
	if err != nil {
		if errors.Is(err, ErrNoPolicy) {
			return nil
		}
		return err
	}
	steps := policy.Steps[alert.Severity]
	if len(steps) == 0 {
		return nil
	}
	if err := Validate(stepChannelKinds(steps)); err != nil {
		return fmt.Errorf("notify: 渠道升级策略引用了未注册渠道（R13.6）: %w", err)
	}

	base := baseTime
	if base.IsZero() {
		base = e.clock.Now()
	}
	fireAt := base.Add(time.Duration(steps[0].WaitSeconds) * time.Second)
	payload, err := json.Marshal(stepPayload{Step: 0})
	if err != nil {
		return fmt.Errorf("notify: 序列化渠道升级 payload 失败: %w", err)
	}
	if err := e.scheduler.Schedule(ctx, tx, TimerKindChannelEscalation, alert.ID, fireAt, payload); err != nil {
		return fmt.Errorf("notify: 注册渠道升级首个定时器失败: %w", err)
	}
	return nil
}

// Stop 主动取消 alert 待触发的渠道升级定时器，供 Ack/Resolve/静默命中动作在各自的
// 事务内调用（R13.4）。幂等：不存在待触发定时器时不报错（见 scheduler.Cancel 的
// 文档）。与 oncall.Chain.Stop 同一模式：本任务只实现这个方法本身，具体在哪些动作
// 发生时调用它是集成缝隙，留给任务 25/26 接线（与任务 21 的
// "回调未取消升级定时器"是同一类缝隙，已由 team-lead 确认不是本任务范围）。
// advance 内部另有一道针对同一三个条件（firing/未 Ack/未静默）的被动检查（见
// shouldContinueEscalation），即使 Stop 未被及时调用，定时器真正到期时也不会误发。
func (e *Engine) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	if err := e.scheduler.Cancel(ctx, tx, TimerKindChannelEscalation, alertID); err != nil {
		return fmt.Errorf("notify: 取消渠道升级定时器失败: %w", err)
	}
	return nil
}

// Handle 实现 scheduler.Handler：refID 是 alert.ID，payload 是 stepPayload 的 JSON。
// tx 是 scheduler 传入的事务（同一事务内已经把这条定时器标记为 fired），本级的
// 查状态/落时间线/记发送意图/注册下一级定时器都复用这个 tx。
func (e *Engine) Handle(ctx context.Context, tx pgx.Tx, refID string, payload []byte) error {
	var p stepPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("notify: 反序列化渠道升级 payload 失败: %w", err)
	}
	return e.advance(ctx, tx, refID, p.Step)
}

// advance 是 Handle 的核心逻辑，在 Handle 开出的事务内执行。
func (e *Engine) advance(ctx context.Context, tx pgx.Tx, alertID string, step int) error {
	alert, err := e.alerts.GetByID(ctx, tx, alertID)
	if err != nil {
		if errors.Is(err, store.ErrAlertNotFound) {
			return nil // 告警已不存在（理论上不应发生）：无需重投，直接结束
		}
		return fmt.Errorf("notify: 查询告警失败: %w", err)
	}
	if !shouldContinueEscalation(alert) {
		return nil // 已 Ack/Resolved/静默命中：停止，不再注册下一级定时器（R13.4/R10.3）
	}

	entries, err := e.timeline.ListByAlert(ctx, tx, alertID)
	if err != nil {
		return fmt.Errorf("notify: 查询时间线失败: %w", err)
	}
	if alreadyHandledStep(entries, step) {
		return nil // 幂等：本级已经处理过（at-least-once 重投），不重复发送、不重复注册定时器
	}

	policy, err := e.resolvePolicy(ctx, tx, alert)
	if err != nil {
		if errors.Is(err, ErrNoPolicy) {
			return nil // 策略在升级进行中被删除：不报错，直接停止
		}
		return err
	}
	steps := policy.Steps[alert.Severity]
	if step >= len(steps) {
		return nil // 策略在升级进行中被缩短，当前级已超出新序列长度：正常结束
	}

	channels := effectiveChannels(steps, step)
	now := e.clock.Now()

	if _, err := e.timeline.Append(ctx, tx, domain.TimelineEntry{
		AlertID: alert.ID,
		Actor:   escalationActor,
		Action:  domain.TimelineActionNotify,
		At:      now,
		Detail: map[string]any{
			escalationDetailKind:     escalationKindChannel,
			escalationDetailStep:     step,
			escalationDetailChannels: channels,
			escalationDetailMode:     steps[step].Mode,
		},
	}); err != nil {
		return fmt.Errorf("notify: 写入渠道升级时间线失败: %w", err)
	}
	if err := e.dispatcher.DispatchStep(ctx, tx, alert, step, channels); err != nil {
		return fmt.Errorf("notify: 记录渠道升级发送意图失败: %w", err)
	}

	next := step + 1
	if next >= len(steps) {
		return nil // 已到序列末级，正常结束
	}
	fireAt := now.Add(time.Duration(steps[next].WaitSeconds) * time.Second)
	nextPayload, err := json.Marshal(stepPayload{Step: next})
	if err != nil {
		return fmt.Errorf("notify: 序列化渠道升级 payload 失败: %w", err)
	}
	if err := e.scheduler.Schedule(ctx, tx, TimerKindChannelEscalation, alert.ID, fireAt, nextPayload); err != nil {
		return fmt.Errorf("notify: 注册下一级渠道升级定时器失败: %w", err)
	}
	return nil
}

// escalationActor/escalationDetail* 是落时间线时 Detail 的固定键与 Action 复用约定：
// 渠道升级复用 domain.TimelineActionNotify（而非 TimelineActionEscalate），因为后者
// 已被 oncall 包用于人员升级——两条正交的升级维度若共用同一个 Action 常量，时间线
// 视图上会把"换渠道"和"换责任人"混在一起，无法区分。TimelineActionNotify 本身已被
// Sender.markFailed 用于失败发送记账，这里用 Detail[kind]="channel_escalation" 与
// Sender 的失败记账条目（无此字段）区分开，读者可按 Detail 内容判断具体来源。
const (
	escalationActor          = "notify-policy-engine"
	escalationDetailKind     = "kind"
	escalationKindChannel    = "channel_escalation"
	escalationDetailStep     = "step"
	escalationDetailChannels = "channels"
	escalationDetailMode     = "mode"
)

// shouldContinueEscalation 报告 alert 是否仍处于需要渠道升级的状态：firing 且未被
// 任何静默规则命中。Acknowledged/Resolved/静默命中三者任一即停止（R13.4/R10.3），
// 与 oncall 包的同名私有函数是同一判据（两个正交升级维度共享相同的停止条件）。
func shouldContinueEscalation(alert domain.Alert) bool {
	return alert.Status == domain.StatusFiring && len(alert.SilencedBy) == 0
}

// alreadyHandledStep 检查时间线里是否已经存在"本级已处理"的记账，作为 advance 的
// 幂等判据——与 oncall.alreadyHandledLevel/notify.Sender 的 idempotency_key 是同一
// 思路：用已经落库的记录本身当幂等键。JSON 往返后数字统一是 float64，这里做兼容处理。
func alreadyHandledStep(entries []domain.TimelineEntry, step int) bool {
	for _, e := range entries {
		if e.Action != domain.TimelineActionNotify {
			continue
		}
		if kind, _ := e.Detail[escalationDetailKind].(string); kind != escalationKindChannel {
			continue
		}
		switch v := e.Detail[escalationDetailStep].(type) {
		case int:
			if v == step {
				return true
			}
		case float64:
			if int(v) == step {
				return true
			}
		}
	}
	return false
}

// resolvePolicy 实现 R13.5 的三级求值链：
//  1. alert.TeamID 非空时，查该团队全部策略（ListByTeam 一次查询覆盖 tier 1+2），
//     按 created_at 升序（repo 既有排序）对 Matchers 非空的记录做 first-match-wins；
//  2. 上一步未命中时，同一批结果里 Matchers 为空的那条是团队默认策略；
//  3. 前两级都未命中（含 alert.TeamID 为空，未路由到任何团队）时，落到全局默认
//     （GetGlobalDefault：team_id IS NULL 且 matchers 为空数组）。
//
// 三级都未命中返回 ErrNoPolicy。store 层的 notification_policies 表结构上还允许
// "team_id IS NULL 但 matchers 非空"的第四种记录（全局范围的业务绑定策略，见
// store.NotificationPolicyRepo.GetGlobalDefault 的注释），但 requirements.md R13.5
// 明确只定义了"绑定 matcher -> 团队默认 -> 全局默认"三级兜底，未提及这第四类记录
// 参与求值——本引擎按 R13.5 字面实现三级链，不求值这类记录，留作已知局限在回报里
// 披露（若后续证实这确实是遗漏的第四级，需要 store 新增一个按 team_id IS NULL 查询
// 的方法，属于纯增量）。
func (e *Engine) resolvePolicy(ctx context.Context, tx pgx.Tx, alert domain.Alert) (*store.NotificationPolicy, error) {
	labels := mergedMatchLabels(alert)

	if alert.TeamID != "" {
		teamPolicies, err := e.policies.ListByTeam(ctx, tx, alert.TeamID, 0)
		if err != nil {
			return nil, fmt.Errorf("notify: 查询团队通知策略失败: %w", err)
		}
		var teamDefault *store.NotificationPolicy
		for i := range teamPolicies {
			p := teamPolicies[i]
			if len(p.Matchers) == 0 {
				if teamDefault == nil {
					teamDefault = &p
				}
				continue
			}
			if p.Matchers.Matches(labels) {
				return &p, nil
			}
		}
		if teamDefault != nil {
			return teamDefault, nil
		}
	}

	global, err := e.policies.GetGlobalDefault(ctx, tx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoPolicy
		}
		return nil, fmt.Errorf("notify: 查询全局默认通知策略失败: %w", err)
	}
	return global, nil
}

// mergedMatchLabels 把 Alert.Labels 与 Alert.ResourceRef 合并为一份用于 matcher 求值
// 的标签视图，与 internal/pipeline 包内 stage_route.go 的同名私有函数保持完全一致的
// 语义（ResourceRef 打底、Labels 覆盖，键冲突时以 Labels 为准）——按 team-lead 的
// 允许在本包本地重新实现一份，不为此跨包引入 pipeline 依赖。
func mergedMatchLabels(alert domain.Alert) map[string]string {
	merged := make(map[string]string, len(alert.Labels)+len(alert.ResourceRef))
	for k, v := range alert.ResourceRef {
		merged[k] = v
	}
	for k, v := range alert.Labels {
		merged[k] = v
	}
	return merged
}

// effectiveChannels 计算序列第 idx 级实际生效的渠道集合：第 0 级或 Mode 不是
// modeSpread（含 "replace" 与任何未识别取值）时直接采用该级自身的 Channels（替换
// 语义）；Mode 是 modeSpread 时与上一级生效集合取并集、按首次出现顺序去重（R13.2）。
func effectiveChannels(steps []store.NotificationStep, idx int) []string {
	if idx == 0 || steps[idx].Mode != modeSpread {
		return append([]string(nil), steps[idx].Channels...)
	}
	return unionChannels(effectiveChannels(steps, idx-1), steps[idx].Channels)
}

// unionChannels 按首次出现顺序对 a、b 去重合并。
func unionChannels(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, c := range a {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range b {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// stepChannelKinds 收集 steps 序列里出现过的全部渠道类型（去重，忽略顺序），供
// Validate 校验用。
func stepChannelKinds(steps []store.NotificationStep) []string {
	seen := make(map[string]bool)
	var kinds []string
	for _, st := range steps {
		for _, ch := range st.Channels {
			if !seen[ch] {
				seen[ch] = true
				kinds = append(kinds, ch)
			}
		}
	}
	return kinds
}

// ValidatePolicySteps 校验一份完整策略（覆盖其全部 severity 分层）里引用的渠道是否
// 全部已注册（R13.6）：策略在写入（管理端配置校验 API，非本任务范围，留给任务
// 25/26）时应当调用本函数，未启用的渠道（如尚未接入供应商的电话渠道）在配置阶段
// 就报错提示，而不是留到真正升级触发时才发现发不出去。Engine.Start 内部另有一道
// 针对具体 alert 实际会用到的那个 severity 分层的同类检查，作为运行时的兜底防线
// （例如渠道在策略创建之后才被下线注册表）。
func ValidatePolicySteps(steps store.NotificationSteps) error {
	var kinds []string
	seen := make(map[string]bool)
	for _, levelSteps := range steps {
		for _, ch := range stepChannelKinds(levelSteps) {
			if !seen[ch] {
				seen[ch] = true
				kinds = append(kinds, ch)
			}
		}
	}
	return Validate(kinds)
}
