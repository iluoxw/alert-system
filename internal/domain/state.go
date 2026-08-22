package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// TimelineAction 标识时间线条目对应的动作类型（R4.6）。
// ack/resolve/handoff 由本任务产生；其余为预留给后续任务（通知策略/升级链/静默）
// 落时间线时复用的常量，本任务不产生。
type TimelineAction string

const (
	TimelineActionAck     TimelineAction = "ack"
	TimelineActionResolve TimelineAction = "resolve"
	TimelineActionHandoff TimelineAction = "handoff"

	// TimelineActionNotify 已废弃：仅保留供历史数据渲染兼容，新代码改用
	// TimelineActionNotifySent/TimelineActionNotifyFailed 表示实际发送结果；没有真实
	// 渠道、尚未尝试发送时使用 TimelineActionNotifySkipped。
	//
	// Deprecated: 使用 TimelineActionNotifySent 或 TimelineActionNotifyFailed。
	TimelineActionNotify   TimelineAction = "notify"
	TimelineActionEscalate TimelineAction = "escalate"
	TimelineActionSilence  TimelineAction = "silence"

	TimelineActionNotifySent   TimelineAction = "notify_sent"
	TimelineActionNotifyFailed TimelineAction = "notify_failed"
	// TimelineActionNotifySkipped 表示发送意图因缺少可解析的真实渠道目标而没有进入
	// notify_send。它与外部发送失败正交：attempts 恒为 0，不能伪装成 failed。
	TimelineActionNotifySkipped TimelineAction = "notify_skipped"

	// TimelineActionFlappingStart/TimelineActionFlappingEnd 标记 flapping 防抖
	// （aiops-foundation R4/C4）的置位与惰性解除，由 dedup stage 在状态转移点产生。
	TimelineActionFlappingStart TimelineAction = "flapping_start"
	TimelineActionFlappingEnd   TimelineAction = "flapping_end"

	// TimelineActionNote 标记独立备注（ui-enrichment，AddNote 产出）：不改变告警
	// 状态，仅在排查过程中沉淀中间信息（如"已联系业务方等回复"），与 Ack/Resolve/
	// Handoff 等状态转移动作在语义上分离。
	TimelineActionNote            TimelineAction = "note"
	TimelineActionSourceUnknown   TimelineAction = "source_unknown"
	TimelineActionSourceRecovered TimelineAction = "source_recovered"
)

// ResolveReason 标识 Resolve 动作的来源（R4.3/R4.4/R12.5）。
type ResolveReason string

const (
	ResolveReasonManual           ResolveReason = "manual"
	ResolveReasonSource           ResolveReason = "source"
	ResolveReasonTTL              ResolveReason = "ttl"
	ResolveReasonAutoRemediation  ResolveReason = "auto-remediation"
	ResolveReasonSourceReconciled ResolveReason = "source-reconciled"
)

// IsValid 报告 r 是否为已定义的 resolve 来源取值。
func (r ResolveReason) IsValid() bool {
	switch r {
	case ResolveReasonManual, ResolveReasonSource, ResolveReasonTTL, ResolveReasonAutoRemediation, ResolveReasonSourceReconciled:
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

// ErrAuthoritativeSourceManualResolve 表示用户试图在 SRE Control 内关闭一个由
// Alertmanager 掌握生命周期的 occurrence。调用方应返回 409 并引导用户去上游处理。
var ErrAuthoritativeSourceManualResolve = errors.New("state: Alertmanager 权威来源不允许手工 Resolve")

// ErrInvalidNote 表示 AddNote 收到的备注去首尾空白后为空串，或超出长度上限。
var ErrInvalidNote = errors.New("state: 备注内容为空或超出长度上限")

// maxNoteLength 是独立备注的字符数（rune 计数）上限，与 Resolve 处置说明沿用的
// design.md C5 1000 字符口径一致；两处各自维护常量而非共用同一符号——api 包的
// maxResolveNoteLength 校验的是 Resolve.note（可选，空串合法），domain 包这里额外
// 要求去空白后非空，语义不同，不应绑定同一个符号。
const maxNoteLength = 1000

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
// actorID/actorName 分开是因为 alerts.acked_by 是 UUID 列，只能接受内部 users.id；
// actorName 是 timeline 展示用的可读标识。actorID 允许为空（外部渠道操作者无法
// 映射到内部用户账号时用，落库时 store 层用 NULLIF 转成 NULL），但 actorName
// 必须非空——时间线不能有一条不知道是谁做的 ack 记录。
// 响应耗时（now - alert.StartsAt）写入 TimelineEntry.Detail["response_time_seconds"]。
// 纯函数：不修改入参 alert，返回更新后的副本与待落库的 TimelineEntry。
func Ack(alert Alert, actorID, actorName string, now time.Time) (Alert, TimelineEntry, error) {
	if actorName == "" {
		return alert, TimelineEntry{}, fmt.Errorf("state: ack 的 actorName 不能为空")
	}
	if err := Transition(alert.Status, StatusAcknowledged); err != nil {
		return alert, TimelineEntry{}, err
	}

	updated := alert
	updated.ResponseState = ResponseStateAcknowledged
	if !updated.SourceState.IsValid() {
		updated.SourceState = SourceStateFiring
	}
	updated.Status = ProjectLegacyStatus(updated.SourceState, updated.ResponseState)
	updated.AckedBy = actorID
	updated.AckedAt = now
	updated.UpdatedAt = now

	responseTime := now.Sub(alert.StartsAt)
	entry := TimelineEntry{
		AlertID: alert.ID,
		Actor:   actorName,
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
// 由调用方传入系统标识（如 "ttl-scheduler"）。note 是可选的处置说明（R5，"怎么修的"），
// 非空时写入 entry.Detail["note"]；空串时 entry 形态与 note 引入前完全一致——系统侧调用
// （TTL/dedup 的上游 resolved 事件/飞书回调）一律传空串，只有 REST 端点会传用户输入。
// 纯函数：不修改入参，不做任何 I/O；幂等短路（已 resolved 时不再调用本函数）由调用方负责，
// 本函数不感知、不参与。
func Resolve(alert Alert, actor string, reason ResolveReason, note string, now time.Time) (Alert, TimelineEntry, error) {
	if actor == "" {
		return alert, TimelineEntry{}, fmt.Errorf("state: resolve 的 actor 不能为空")
	}
	if !reason.IsValid() {
		return alert, TimelineEntry{}, fmt.Errorf("state: 非法 resolve reason %q", reason)
	}
	if reason == ResolveReasonManual && alert.IsAuthoritativeSource() {
		return alert, TimelineEntry{}, ErrAuthoritativeSourceManualResolve
	}
	if err := Transition(alert.Status, StatusResolved); err != nil {
		return alert, TimelineEntry{}, err
	}

	updated := alert
	updated.Status = StatusResolved
	if !updated.IsAuthoritativeSource() {
		updated.SourceState = SourceStateResolved
	}
	updated.EndsAt = now
	updated.UpdatedAt = now

	detail := map[string]any{"reason": string(reason)}
	switch reason {
	case ResolveReasonTTL:
		detail["label"] = "TTL 自动恢复"
	case ResolveReasonAutoRemediation:
		detail["label"] = "自愈恢复"
	case ResolveReasonSourceReconciled:
		detail["label"] = "Alertmanager 对账确认恢复"
	}
	if note != "" {
		detail["note"] = note
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
	if alert.Status == StatusResolved || alert.SourceState == SourceStateResolved {
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

// AddNote 生成一条独立备注的 TimelineEntry（ui-enrichment 任务2）：详情页排查过程中
// 沉淀中间信息（如"已联系业务方等回复"），不代表任何状态转移。
//
// 不接收也不返回 Alert——这是与 Ack/Resolve/Handoff 的本质区别：那几个函数都要做
// Transition 校验并返回更新后的 Alert 副本，AddNote 完全不触碰状态机，纯粹是一条
// 追加时间线记录的动作。也不校验告警当前状态（是否已 resolved）——resolved 后补记
// 是合法场景（事后复盘、补充根因），alertID 的存在性由 API/store 层兜，本函数不
// 做任何 I/O。
//
// note 去首尾空白后为空串、或长度（rune 计数）超过 maxNoteLength，返回
// ErrInvalidNote；去空白后非空是必须的——备注是这条记录的全部内容，与 Resolve.note
// "可选，不填就不写"不同，AddNote 场景下空备注没有意义。
func AddNote(alertID, actorName, note string, now time.Time) (TimelineEntry, error) {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > maxNoteLength {
		return TimelineEntry{}, ErrInvalidNote
	}

	return TimelineEntry{
		AlertID: alertID,
		Actor:   actorName,
		Action:  TimelineActionNote,
		At:      now,
		Detail: map[string]any{
			"note": trimmed,
		},
	}, nil
}
