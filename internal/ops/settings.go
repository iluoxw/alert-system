// Package ops 实现团队级运营周期任务（日报/悬置扫描/值班播报）的纯函数层：
// 配置模型、下次触发时刻计算、通知文案构建。全部无 IO、不依赖 scheduler/store，
// 供任务 7 的 scheduler handler 调用。
package ops

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 三个 ops 周期任务对应的 timer kind，与既有六个 kind 同处 internal/app 注册。
const (
	TimerKindDailyReport     = "ops_daily_report"
	TimerKindStaleSweep      = "ops_stale_sweep"
	TimerKindOncallBroadcast = "ops_oncall_broadcast"
)

// ChannelTarget 对应 team_ops_settings.channels JSONB 数组元素 {"kind","target"}。
// json tag 必须小写，与迁移注释里的示例形状及 store 层的序列化/反序列化对齐。
type ChannelTarget struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// Settings 是 team_ops_settings 一行的领域表示，字段与迁移列一一对应（任务 5 的 store 负责映射）。
type Settings struct {
	TeamID   string
	Timezone string // IANA 时区名，如 "Asia/Shanghai"；解析失败按 NextFire/PeriodRound 的约定回退 UTC

	DailyReportEnabled bool
	DailyReportTime    string // "HH:MM"，按 Timezone 解释

	StaleSweepEnabled         bool
	StaleSweepIntervalSeconds int

	OncallBroadcastEnabled bool
	OncallBroadcastTime    string // "HH:MM"，按 Timezone 解释

	Channels []ChannelTarget
}

// loadLocationOrUTC 解析时区，失败回退 UTC。纯函数层不打日志，是否记 warning 由调用方
// 依据 NextFire 返回的 error 决定；PeriodRound 不返回 error，直接静默回退。
func loadLocationOrUTC(tz string) (*time.Location, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC, err
	}
	return loc, nil
}

// parseHHMM 解析 "HH:MM" 格式的时刻配置，返回小时与分钟。
func parseHHMM(hhmm string) (hour, minute int, err error) {
	parts := strings.SplitN(hhmm, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("ops: 时刻格式非法（期望 HH:MM）: %q", hhmm)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("ops: 时刻格式非法（期望 HH:MM）: %q", hhmm)
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("ops: 时刻格式非法（期望 HH:MM）: %q", hhmm)
	}
	return hour, minute, nil
}

// nextDailyTime 求 now 之后（不含 now 本身）最近一个 hhmm 时刻，按 loc 解释。
// 今天该时刻已过（或恰为 now）则取明天——错过轮次语义（跳过、不补发）依赖这个
// "永远从当前时间之后算"的性质：调用方每次都用真实 now 重算，停机期间错过的
// 轮次不会被回补，也不会因为"从上轮时刻累加"而产生时刻漂移。
func nextDailyTime(hhmm string, now time.Time, loc *time.Location) (time.Time, error) {
	hour, minute, err := parseHHMM(hhmm)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(loc)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate, nil
}

// nextInterval 求 now 之后最近一个 interval 网格点（相对 Unix 纪元对齐，而非相对 now 累加），
// 使 PeriodRound 对同一时间窗算出的截断值与这里选中的触发时刻天然一致。
func nextInterval(intervalSeconds int, now time.Time) time.Time {
	interval := int64(intervalSeconds)
	grid := now.Unix()/interval + 1 // 严格晚于 now 的下一个网格点
	return time.Unix(grid*interval, 0).UTC()
}

// NextFire 按 kind 计算 s 在 now 之后应触发的下一时刻。
//
// 日报/值班播报：取团队时区下 now 之后最近一个 HH:MM 时刻（今天已过则明天）。
// 悬置扫描：取 now 之后最近一个按 interval 对齐的网格点（窗口起点对齐，保证 PeriodRound 确定性）。
//
// err 非 nil 时表示 s.Timezone 解析失败，已回退 UTC 算出 time（可用），是否记录 warning
// 由调用方决定——纯函数层不做 IO/日志。
func NextFire(kind string, s Settings, now time.Time) (time.Time, error) {
	loc, tzErr := loadLocationOrUTC(s.Timezone)
	switch kind {
	case TimerKindDailyReport:
		t, err := nextDailyTime(s.DailyReportTime, now, loc)
		if err != nil {
			return time.Time{}, err
		}
		return t, tzErr
	case TimerKindOncallBroadcast:
		t, err := nextDailyTime(s.OncallBroadcastTime, now, loc)
		if err != nil {
			return time.Time{}, err
		}
		return t, tzErr
	case TimerKindStaleSweep:
		return nextInterval(s.StaleSweepIntervalSeconds, now), tzErr
	default:
		return time.Time{}, fmt.Errorf("ops: 未知的 timer kind: %s", kind)
	}
}

// PeriodRound 计算 fireAt 所属周期的确定性标识，用于 outbox 幂等键的 round 维度。
// handler 崩溃回滚重投时会重新调用本函数而不是复用发送时的值，只有"同一轮必然算出
// 同一个 Round"才能保证幂等（同日/同轮绝不重发，见不变量 4）。
//
// 日报/值班播报：团队时区下 fireAt 所在日期，格式 YYYYMMDD。
// 悬置扫描：fireAt 按 interval 截断的窗口起点 Unix 秒（NextFire 已对齐到网格点，
// 这里再次截断是为了即使入参并非精确网格点也能收敛到同一个窗口标识）。
func PeriodRound(kind string, s Settings, fireAt time.Time) int64 {
	switch kind {
	case TimerKindDailyReport, TimerKindOncallBroadcast:
		loc, _ := loadLocationOrUTC(s.Timezone)
		local := fireAt.In(loc)
		return int64(local.Year())*10000 + int64(local.Month())*100 + int64(local.Day())
	case TimerKindStaleSweep:
		interval := int64(s.StaleSweepIntervalSeconds)
		if interval <= 0 {
			interval = 1
		}
		sec := fireAt.Unix()
		return sec - sec%interval
	default:
		return fireAt.Unix()
	}
}
