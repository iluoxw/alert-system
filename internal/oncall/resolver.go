// Package oncall 实现值班求值与人员升级链（R5.4/R5.5）：
//   - Resolver.OnCallAt：给定团队与时刻，按 schedules/shifts 求出当班人；
//   - Chain（escalation.go）：告警未 Ack 时经 scheduler 沿升级策略逐级推进，每级
//     用 Resolver 求出当班人再交给注入的 Notifier 记录通知意图。
//
// 本包与渠道升级（R13，同一人换渠道）正交并行：二者都借助 scheduler 的统一定时器，
// 但 kind 不同（本包用 TimerKindPersonnelEscalation），互不感知、互不影响。
package oncall

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// OnCallEntry 是某一时刻某个人正处于值班状态的具体归属：值班表 + 具体班次来源，
// 供调用方（升级链落时间线、排障）追溯"这个人为什么在值班"。
type OnCallEntry struct {
	UserID     string
	ScheduleID string
	ShiftID    string
}

// ScheduleRepo 是 Resolver 依赖的值班表仓储子集，由 store.ScheduleRepo 实现。
type ScheduleRepo interface {
	ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.Schedule, error)
}

// ShiftRepo 是 Resolver 依赖的班次仓储子集，由 store.ShiftRepo 实现。
type ShiftRepo interface {
	ListInWindow(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, limit int) ([]store.Shift, error)
}

// maxSchedulesPerTeam/maxShiftsPerInstant 是防御性上限：设计上一个团队只对应一张
// 值班表（见 migrations 000002 schedules 表注释），同一时刻覆盖同一值班表的班次
// 也理应至多一条；这里放宽到个位数以上只是为了容忍配置异常（重叠班次）而不报错。
const (
	maxSchedulesPerTeam  = 20
	maxShiftsPerInstant  = 10
	instantWindowEpsilon = time.Nanosecond
)

// Resolver 求值"某时刻谁在值班"（R5.5）。与仓储的既有约定一致：本身无状态，
// 所有方法在调用方传入的 pgx.Tx 内执行。
type Resolver struct {
	schedules ScheduleRepo
	shifts    ShiftRepo
}

// NewResolver 构造 Resolver。
func NewResolver(schedules ScheduleRepo, shifts ShiftRepo) *Resolver {
	return &Resolver{schedules: schedules, shifts: shifts}
}

// OnCallAt 返回 teamID 团队在时刻 t 值班的全部人员（起含终不含，与 SilenceRepo 同一
// 口径）。t 统一转 UTC 后与 shifts 表的 timestamptz 列比较，覆盖跨天/跨时区班次。
// 返回空切片（非 nil error）表示该时刻无人覆盖——团队没有配置值班表，或值班表在
// 该时刻存在班次空洞，调用方（Chain）据此落时间线警告，而不是当作错误处理。
func (r *Resolver) OnCallAt(ctx context.Context, tx pgx.Tx, teamID string, t time.Time) ([]OnCallEntry, error) {
	t = t.UTC()

	schedules, err := r.schedules.ListByTeam(ctx, tx, teamID, maxSchedulesPerTeam)
	if err != nil {
		return nil, fmt.Errorf("oncall: 查询团队值班表失败: %w", err)
	}

	var entries []OnCallEntry
	for _, sched := range schedules {
		// ListInWindow 按 [from, to) 重叠语义查询（starts_at < to AND ends_at > from）；
		// 取 to = t + 1ns 使 starts_at <= t 的班次（起含）也被纳入，同时 ends_at > t
		// 天然排除恰好在 t 结束的班次（终不含）。1ns 小于 timestamptz 的微秒精度，
		// 不会误纳入下一条真正在 t 之后才开始的班次。
		shifts, err := r.shifts.ListInWindow(ctx, tx, sched.ID, t, t.Add(instantWindowEpsilon), maxShiftsPerInstant)
		if err != nil {
			return nil, fmt.Errorf("oncall: 查询班次失败: %w", err)
		}
		for _, sh := range shifts {
			entries = append(entries, OnCallEntry{UserID: sh.UserID, ScheduleID: sched.ID, ShiftID: sh.ID})
		}
	}
	return entries, nil
}
