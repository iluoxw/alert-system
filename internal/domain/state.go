package domain

import (
	"errors"
	"fmt"
	"time"
)

// TimelineAction 标识时间线条目对应的动作类型（R4.6）。
// ack/resolve/handoff 由本任务产生；其余为预留给后续任务（通知策略/升级链/静默）
// 落时间线时复用的常量，本任务不产生。
type TimelineAction string

const (
	TimelineActionAck     TimelineAction = "ack"
	TimelineActionResolve TimelineAction = "resolve"
	TimelineActionHandoff TimelineAction = "handoff"

	TimelineActionNotify   TimelineAction = "notify"
	TimelineActionEscalate TimelineAction = "escalate"
	TimelineActionSilence  TimelineAction = "silence"
)

// ResolveReason 标识 Resolve 动作的来源（R4.3/R4.4/R12.5）。
type ResolveReason string

const (
	ResolveReasonManual          ResolveReason = "manual"
	ResolveReasonSource          ResolveReason = "source"
	ResolveReasonTTL             ResolveReason = "ttl"
	ResolveReasonAutoRemediation ResolveReason = "auto-remediation"
)

// IsValid 报告 r 是否为已定义的 resolve 来源取值。
func (r ResolveReason) IsValid() bool {
	switch r {
	case ResolveReasonManual, ResolveReasonSource, ResolveReasonTTL, ResolveReasonAutoRemediation:
		return true
	default:
		return false
	}
}

// TimelineEntry 是不可篡改的时间线条目：谁、何时、什么动作、详情（R4.6）。
// 由状态机函数产出，供 store 层 append-only 落库；本包不做任何 I/O。
type TimelineEntry struct {
	AlertID string
	Actor   string
	Action  TimelineAction
	Detail  map[string]any
	At      time.Time
}

// ErrInvalidTransition 是非法状态转移的哨兵错误，调用方可用 errors.Is(err, ErrInvalidTransition) 判断。
var ErrInvalidTransition = errors.New("state: 非法状态转移")

// TransitionError 携带一次非法转移的起止状态，调用方可用 errors.As 取出明细。
type TransitionError struct {
	From Status
	To   Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("state: 非法状态转移 %s -> %s", e.From, e.To)
}

// Is 使 errors.Is(err, ErrInvalidTransition) 对任意 *TransitionError 成立。
func (e *TransitionError) Is(target error) bool {
	return target == ErrInvalidTransition
}

// ErrAlertResolved 表示对已 resolved 的告警执行了不允许的动作（如 Handoff）。
var ErrAlertResolved = errors.New("state: 告警已 resolved")

// allowedTransitions 显式定义 R4.1 的全转移矩阵：firing -> acknowledged -> resolved，
// 且 firing 可直达 resolved。未列出的组合（任何状态转回 firing、resolved 转回
// acknowledged、以及任何自转移）均为非法。
var allowedTransitions = map[Status]map[Status]bool{
	StatusFiring:       {StatusAcknowledged: true, StatusResolved: true},
	StatusAcknowledged: {StatusResolved: true},
	StatusResolved:     {},
}

// Transition 校验 from -> to 是否为合法的状态转移；非法时返回 *TransitionError。
func Transition(from, to Status) error {
	if allowedTransitions[from][to] {
		return nil
	}
	return &TransitionError{From: from, To: to}
}

// Ack 执行认领动作：firing -> acknowledged（R4.1/R4.2）。
// 响应耗时（now - alert.StartsAt）写入 TimelineEntry.Detail["response_time_seconds"]。
// 纯函数：不修改入参 alert，返回更新后的副本与待落库的 TimelineEntry。
func Ack(alert Alert, actor string, now time.Time) (Alert, TimelineEntry, error) {
	if actor == "" {
		return alert, TimelineEntry{}, fmt.Errorf("state: ack 的 actor 不能为空")
	}
	if err := Transition(alert.Status, StatusAcknowledged); err != nil {
		return alert, TimelineEntry{}, err
	}

	updated := alert
	updated.Status = StatusAcknowledged
	updated.AckedBy = actor
	updated.AckedAt = now
	updated.UpdatedAt = now

	responseTime := now.Sub(alert.StartsAt)
	entry := TimelineEntry{
		AlertID: alert.ID,
		Actor:   actor,
		Action:  TimelineActionAck,
		At:      now,
		Detail: map[string]any{
			"response_time_seconds": responseTime.Seconds(),
		},
	}
	return updated, entry, nil
}

// Resolve 执行解决动作：firing/acknowledged -> resolved（R4.1）。
// reason 标识来源：manual（人工）/source（上游 resolved 事件，R4.3）/ttl（TTL 自动恢复，R4.4）/
// auto-remediation（自愈回调确认恢复，R12.5）；ttl 与 auto-remediation 会在时间线详情中
// 打上可读标记（"TTL 自动恢复"/"自愈恢复"）。actor 对人工 Resolve 为操作人，系统自动触发时
// 由调用方传入系统标识（如 "ttl-scheduler"）。纯函数：不修改入参，不做任何 I/O。
func Resolve(alert Alert, actor string, reason ResolveReason, now time.Time) (Alert, TimelineEntry, error) {
	if actor == "" {
		return alert, TimelineEntry{}, fmt.Errorf("state: resolve 的 actor 不能为空")
	}
	if !reason.IsValid() {
		return alert, TimelineEntry{}, fmt.Errorf("state: 非法 resolve reason %q", reason)
	}
	if err := Transition(alert.Status, StatusResolved); err != nil {
		return alert, TimelineEntry{}, err
	}

	updated := alert
	updated.Status = StatusResolved
	updated.EndsAt = now
	updated.UpdatedAt = now

	detail := map[string]any{"reason": string(reason)}
	switch reason {
	case ResolveReasonTTL:
		detail["label"] = "TTL 自动恢复"
	case ResolveReasonAutoRemediation:
		detail["label"] = "自愈恢复"
	}

	entry := TimelineEntry{
		AlertID: alert.ID,
		Actor:   actor,
		Action:  TimelineActionResolve,
		At:      now,
		Detail:  detail,
	}
	return updated, entry, nil
}

// Handoff 将告警责任人转移给 target（用户或团队标识），不改变 Status（R4.5）。
// Alert 结构未单独建模 assignee 字段，责任人落在 AckedBy 上（Ack 后的响应人即当前责任人；
// 未 Ack 时 Handoff 同样可用于团队/人员间的提前转移）。已 resolved 的告警不可 Handoff。
func Handoff(alert Alert, actor, target string, now time.Time) (Alert, TimelineEntry, error) {
	if actor == "" {
		return alert, TimelineEntry{}, fmt.Errorf("state: handoff 的 actor 不能为空")
	}
	if target == "" {
		return alert, TimelineEntry{}, fmt.Errorf("state: handoff 的 target 不能为空")
	}
	if alert.Status == StatusResolved {
		return alert, TimelineEntry{}, ErrAlertResolved
	}

	previousOwner := alert.AckedBy
	updated := alert
	updated.AckedBy = target
	updated.UpdatedAt = now

	entry := TimelineEntry{
		AlertID: alert.ID,
		Actor:   actor,
		Action:  TimelineActionHandoff,
		At:      now,
		Detail: map[string]any{
			"from": previousOwner,
			"to":   target,
		},
	}
	return updated, entry, nil
}
