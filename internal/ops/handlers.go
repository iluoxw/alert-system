package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
)

// ErrSettingsNotFound 是 SettingsRepo.GetSettings 未找到团队配置时应返回的哨兵
// error；三个 handler 据此判断"团队从未保存过运营配置"，静默返回且不续期——不是
// 错误（团队原本就没有开启这些周期任务）。internal/app 的适配器需要把
// store.ErrNotFound 转换成这个值。
var ErrSettingsNotFound = errors.New("ops: 未找到团队运营配置")

// SettingsRepo 是三个 handler 共用的配置读取窄接口，由 internal/app 用
// store.OpsRepo.GetSettings 适配（store.OpsSettings -> ops.Settings 字段拷贝，
// store.ErrNotFound -> ErrSettingsNotFound）。
type SettingsRepo interface {
	GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*Settings, error)
}

// TeamRepo 是取团队展示名的窄接口，供文案标题使用（"{团队名} 团队日报"之类）。
type TeamRepo interface {
	TeamName(ctx context.Context, tx pgx.Tx, teamID string) (string, error)
}

// DailyStatsRepo 是日报 handler 依赖的统计窄接口，由 store.OpsRepo 适配。
type DailyStatsRepo interface {
	DailyStats(ctx context.Context, tx pgx.Tx, teamID string, dayStart, dayEnd time.Time) (DailyStats, error)
	ListOpenLongerThan(ctx context.Context, tx pgx.Tx, teamID string, olderThan time.Time) ([]domain.Alert, error)
}

// StaleAlertRepo 是悬置扫描 handler 依赖的窄接口，由 store.OpsRepo 适配。
type StaleAlertRepo interface {
	ListUnackedCritical(ctx context.Context, tx pgx.Tx, teamID string) ([]domain.Alert, error)
}

// OncallResolver 是值班播报 handler 依赖的窄接口，直接返回展示名而非
// oncall.OnCallEntry/UserID——由 internal/app 组合 oncall.Resolver.OnCallAt 与用户
// 仓储的 UserID -> 展示名查询来实现。这样 ops 包不需要 import internal/oncall 或
// internal/store 就能拿到 BuildOncallBroadcast 需要的 []string。
type OncallResolver interface {
	ResolveOnCallNames(ctx context.Context, tx pgx.Tx, teamID string, at time.Time) ([]string, error)
}

// OpsNotifier 是 ops 消息的"发送意图"接口（design.md R2 第五个意图接口），由
// internal/app 实现（内部落 notify_send 定时器，真正外部 IO 交给既有 Sender 异步
// 完成）。硬约束同其余四个意图接口：实现绝不能在这个持锁事务内做同步外部 IO。
//
// groupKey（design.md 幂等键设计：GroupKey = "ops:{teamID}:{kind}"）由实现自己
// 用 teamID+kind 拼出即可，这里不直接传 groupKey 字符串是为了让调用方（本文件）
// 不需要重复维护这个拼接格式的常量。
type OpsNotifier interface {
	ScheduleOpsSend(ctx context.Context, tx pgx.Tx, teamID, kind string, round int64, ch ChannelTarget, msg notify.Message) error
}

// TimerScheduler 是 handler 自续期依赖的窄接口，由 scheduler.Scheduler 实现。
type TimerScheduler interface {
	Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error
}

// Clock 抽象"当前时间"，与 oncall.Clock 同一模式，供测试注入 fake clock。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// opsPayload 是三个 ops 定时器 payload 的 JSON 结构：只携带触发时刻 FireAt。
// PeriodRound(kind, settings, fireAt) 需要一个确定性的输入——若 handler 内部用
// time.Now() 现算，同一条定时器崩溃重投时会算出不同的时刻、进而算出不同的 Round，
// 幂等键就跟着漂移（违反不变量 4）。FireAt 必须来自 Schedule 调用时那一刻的真实
// 触发时刻，随 payload 一起持久化，重投时原样反序列化取回。
type opsPayload struct {
	FireAt time.Time `json:"fire_at"`
}

// EncodePayload 序列化 fireAt 为 opsPayload 的 JSON，供 internal/app 的启动对账
// （ScheduleIfAbsent）与设置保存端点（Schedule）在首次注册/重新注册定时器时使用——
// 必须用这同一份编码，否则 handler 反序列化会失败。
func EncodePayload(fireAt time.Time) ([]byte, error) {
	b, err := json.Marshal(opsPayload{FireAt: fireAt})
	if err != nil {
		return nil, fmt.Errorf("ops: 序列化 payload 失败: %w", err)
	}
	return b, nil
}

func parseOpsPayload(payload []byte) (time.Time, error) {
	var p opsPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return time.Time{}, fmt.Errorf("ops: 反序列化 payload 失败: %w", err)
	}
	if p.FireAt.IsZero() {
		return time.Time{}, fmt.Errorf("ops: payload 缺少 fire_at")
	}
	return p.FireAt, nil
}

// Handlers 持有三个 ops scheduler handler 的全部依赖，实现 scheduler.Handler 契约
// （見 HandleDailyReport/HandleStaleSweep/HandleOncallBroadcast）。依赖全部是本文件
// 定义的窄接口，真实实现由 internal/app（任务 9）适配 store/oncall/notify 提供。
type Handlers struct {
	settings SettingsRepo
	teams    TeamRepo
	daily    DailyStatsRepo
	stale    StaleAlertRepo
	oncall   OncallResolver
	notifier OpsNotifier
	sched    TimerScheduler
	clock    Clock

	// AlertListURL 按 teamID 生成告警列表页链接，写入 notify.Message.DetailURL
	// （design.md R2）。本包不感知 web 路由/部署域名等配置，可选留空——留空时
	// DetailURL 不设置，channel 实现（如飞书卡片）按 Message.DetailURL 为空处理即可。
	AlertListURL func(teamID string) string

	// queryIndicators 默认 ops.QueryIndicators。单测注入 stub，避免在 nil tx 上跑 SQL。
	queryIndicators func(ctx context.Context, tx pgx.Tx) (Indicators, error)
}

// NewHandlers 构造 Handlers，clock 默认为真实时钟（测试可直接覆盖 h.clock 字段）。
func NewHandlers(settings SettingsRepo, teams TeamRepo, daily DailyStatsRepo, stale StaleAlertRepo, oncall OncallResolver, notifier OpsNotifier, sched TimerScheduler) *Handlers {
	return &Handlers{
		settings:        settings,
		teams:           teams,
		daily:           daily,
		stale:           stale,
		oncall:          oncall,
		notifier:        notifier,
		sched:           sched,
		clock:           realClock{},
		queryIndicators: QueryIndicators,
	}
}

// detailURL 按 teamID 求 DetailURL，AlertListURL 未配置时返回空字符串。
func (h *Handlers) detailURL(teamID string) string {
	if h.AlertListURL == nil {
		return ""
	}
	return h.AlertListURL(teamID)
}

// sendToChannels 把 msg 逐渠道调用 ScheduleOpsSend；round 由调用方按
// PeriodRound(kind, settings, fireAt) 算好传入——同一轮到期无论重投多少次都传入同一
// 个 round，天然幂等（见 opsPayload 与不变量 4 的文档）。
func (h *Handlers) sendToChannels(ctx context.Context, tx pgx.Tx, teamID, kind string, round int64, channels []ChannelTarget, msg notify.Message) error {
	for _, ch := range channels {
		if err := h.notifier.ScheduleOpsSend(ctx, tx, teamID, kind, round, ch, msg); err != nil {
			return fmt.Errorf("ops: 调度 %s 发送失败（渠道 %s）: %w", kind, ch.Kind, err)
		}
	}
	return nil
}

// scheduleNext 用真实当前时间（h.clock.Now()，而非本轮的 fireAt）计算下一次触发时刻
// 并自续期——"错过轮次跳过、不补发"（design.md R2）依赖的正是"永远从当前时间之后
// 最近一个配置时刻算起"这条性质：若改用 fireAt 递推，进程长时间停机后恢复会一次性
// 把停机期间错过的所有轮次都排进队列，与预期语义相反。
func (h *Handlers) scheduleNext(ctx context.Context, tx pgx.Tx, kind, teamID string, settings Settings) error {
	next, _ := NextFire(kind, settings, h.clock.Now()) // err 只表示时区解析失败已回退 UTC（仍可用），纯函数层不做日志，此处无需特殊处理
	payload, err := EncodePayload(next)
	if err != nil {
		return err
	}
	if err := h.sched.Schedule(ctx, tx, kind, teamID, next, payload); err != nil {
		return fmt.Errorf("ops: 注册下一轮 %s 定时器失败: %w", kind, err)
	}
	return nil
}

// HandleDailyReport 实现 scheduler.Handler：refID 即 teamID（R2：三个 ops kind 的
// refID 均为 teamID）。流程：读 settings（未配置/已禁用则直接返回，不续期，链路
// 自然断掉，由任务 9 的启动对账与任务 10 的保存端点负责重建）→ 统计 [昨天] 窗口的
// DailyStats 与超时未关闭清单 → 任务 6 的 BuildDailyReport 构文案 → 逐渠道调度发送
// → 同事务自续期下一轮。
func (h *Handlers) HandleDailyReport(ctx context.Context, tx pgx.Tx, teamID string, payload []byte) error {
	fireAt, err := parseOpsPayload(payload)
	if err != nil {
		return err
	}

	settings, err := h.settings.GetSettings(ctx, tx, teamID)
	if errors.Is(err, ErrSettingsNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ops: 查询团队运营配置失败: %w", err)
	}
	if !settings.DailyReportEnabled {
		return nil
	}

	loc, _ := loadLocationOrUTC(settings.Timezone)
	local := fireAt.In(loc)
	dayEnd := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	dayStart := dayEnd.AddDate(0, 0, -1) // 日报统计"昨天"（见 report.go BuildDailyReport 的 date 参数注释）

	stats, err := h.daily.DailyStats(ctx, tx, teamID, dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("ops: 统计日报数据失败: %w", err)
	}
	longOpen, err := h.daily.ListOpenLongerThan(ctx, tx, teamID, fireAt.Add(-24*time.Hour))
	if err != nil {
		return fmt.Errorf("ops: 查询超时未关闭告警失败: %w", err)
	}
	teamName, err := h.teams.TeamName(ctx, tx, teamID)
	if err != nil {
		return fmt.Errorf("ops: 查询团队名称失败: %w", err)
	}

	lead, err := h.queryIndicators(ctx, tx)
	if err != nil {
		return fmt.Errorf("ops: 查询先行指标失败: %w", err)
	}
	msg := BuildDailyReport(teamName, dayStart, stats, longOpen, lead)
	msg.DetailURL = h.detailURL(teamID)

	round := PeriodRound(TimerKindDailyReport, *settings, fireAt)
	if err := h.sendToChannels(ctx, tx, teamID, TimerKindDailyReport, round, settings.Channels, msg); err != nil {
		return err
	}
	return h.scheduleNext(ctx, tx, TimerKindDailyReport, teamID, *settings)
}

// HandleStaleSweep 实现 scheduler.Handler。无未 Ack critical 告警时跳过发送但仍
// 续期（design.md R2："悬置通报无未 Ack critical 时跳过发送但仍自续期"）。
func (h *Handlers) HandleStaleSweep(ctx context.Context, tx pgx.Tx, teamID string, payload []byte) error {
	fireAt, err := parseOpsPayload(payload)
	if err != nil {
		return err
	}

	settings, err := h.settings.GetSettings(ctx, tx, teamID)
	if errors.Is(err, ErrSettingsNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ops: 查询团队运营配置失败: %w", err)
	}
	if !settings.StaleSweepEnabled {
		return nil
	}

	alerts, err := h.stale.ListUnackedCritical(ctx, tx, teamID)
	if err != nil {
		return fmt.Errorf("ops: 查询未 Ack 严重告警失败: %w", err)
	}

	if len(alerts) > 0 {
		teamName, err := h.teams.TeamName(ctx, tx, teamID)
		if err != nil {
			return fmt.Errorf("ops: 查询团队名称失败: %w", err)
		}
		// now 传 fireAt（本轮触发时刻）而非 h.clock.Now()：文案里"已持续 X"的时长
		// 应锚定在这一轮意图发生的时刻，重投时重算得到相同文案，与 Round 的确定性
		// 保持同一口径（否则重投时长会漂移，虽不影响幂等去重但会造成认知混乱）。
		msg := BuildStaleSweep(teamName, alerts, fireAt)
		msg.DetailURL = h.detailURL(teamID)

		round := PeriodRound(TimerKindStaleSweep, *settings, fireAt)
		if err := h.sendToChannels(ctx, tx, teamID, TimerKindStaleSweep, round, settings.Channels, msg); err != nil {
			return err
		}
	}
	return h.scheduleNext(ctx, tx, TimerKindStaleSweep, teamID, *settings)
}

// HandleOncallBroadcast 实现 scheduler.Handler。当天无排班时 BuildOncallBroadcast
// 已处理为"今日无值班安排"文案（AC 2.3），本函数不特殊分支。
func (h *Handlers) HandleOncallBroadcast(ctx context.Context, tx pgx.Tx, teamID string, payload []byte) error {
	fireAt, err := parseOpsPayload(payload)
	if err != nil {
		return err
	}

	settings, err := h.settings.GetSettings(ctx, tx, teamID)
	if errors.Is(err, ErrSettingsNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ops: 查询团队运营配置失败: %w", err)
	}
	if !settings.OncallBroadcastEnabled {
		return nil
	}

	names, err := h.oncall.ResolveOnCallNames(ctx, tx, teamID, fireAt)
	if err != nil {
		return fmt.Errorf("ops: 解析当班人员失败: %w", err)
	}
	teamName, err := h.teams.TeamName(ctx, tx, teamID)
	if err != nil {
		return fmt.Errorf("ops: 查询团队名称失败: %w", err)
	}

	loc, _ := loadLocationOrUTC(settings.Timezone)
	local := fireAt.In(loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	msg := BuildOncallBroadcast(teamName, names, date)
	msg.DetailURL = h.detailURL(teamID)

	round := PeriodRound(TimerKindOncallBroadcast, *settings, fireAt)
	if err := h.sendToChannels(ctx, tx, teamID, TimerKindOncallBroadcast, round, settings.Channels, msg); err != nil {
		return err
	}
	return h.scheduleNext(ctx, tx, TimerKindOncallBroadcast, teamID, *settings)
}
