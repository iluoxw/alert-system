package notify

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// 默认重试参数（R6.3）：3 次尝试、200ms 起步的指数退避、封顶 5s，叠加 full jitter
// 避免大量并发重试同时打向下游渠道。
const (
	defaultMaxAttempts    = 3
	defaultInitialBackoff = 200 * time.Millisecond
	defaultMaxBackoff     = 5 * time.Second
)

// idempotencyKeySep 是幂等键各字段之间的分隔符，与 domain.Fingerprint 同风格
// （0x1e，ASCII record separator），避免相邻字段拼接产生歧义。
const idempotencyKeySep = "\x1e"

// ErrNoContact 表示用户在指定渠道下没有可用的联系方式（如 email 渠道但
// users.email 为空）。这是永久性失败：换个时间点重试结果不会变，Resolve 的调用方
// （Sender.Send）据此直接标记失败，不进 sendWithBackoff 重试队列。
var ErrNoContact = errors.New("notify: 用户缺少该渠道的联系方式")

// ContactResolver 把 SendRequest.TargetUserID 解析成某个渠道下的实际发送地址
// （C3「通知送达到人」）：email 渠道解析到 users.email，lark 渠道解析到
// users.lark_open_id，以此类推。实现放 internal/app（基于 UserRepo 的薄适配器，见任务 9），
// 本包只依赖这个小接口——未来统一通知网关落地时只需换实现，Sender 的编排逻辑不必跟着改。
type ContactResolver interface {
	Resolve(ctx context.Context, tx pgx.Tx, userID, channelKind string) (string, error)
}

// logStore 抽象 store.NotificationLogRepo 用到的三个方法，让 Sender 的编排逻辑可以在
// 单测里注入内存 fake，不需要真实 Postgres。*store.NotificationLogRepo 天然满足这个接口。
type logStore interface {
	Insert(ctx context.Context, tx pgx.Tx, e store.NotificationLogEntry) (*store.NotificationLogEntry, error)
	GetByIdempotencyKey(ctx context.Context, tx pgx.Tx, key string) (*store.NotificationLogEntry, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, id, status string, attempts int, lastError string) error
}

// timelineAppender 抽象 store.TimelineRepo.Append。
type timelineAppender interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
}

// txRunner 抽象 store.Store.WithTx。
type txRunner interface {
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// SendRequest 是一次具体的发送请求：某个告警/分组的某一级升级序列，要发到哪个渠道的哪个 target。
type SendRequest struct {
	AlertID  string // 与 GroupKey 二选一：单告警通知填这个
	GroupKey string // 与 AlertID 二选一：分组通知填这个

	// Step 对应升级序列的第几级；不属于升级序列的发送传 nil。用指针而非 int 是对齐
	// store.NotificationLogEntry.Step 的既有约定——0 是合法的第 0 级，nil 才表示"无关序列"。
	Step *int

	ChannelKind string
	Target      string
	Msg         Message

	// TargetUserID 非空时表示"到人"发送：Send 会在查找 Channel 之前，用注入的
	// ContactResolver 按 ChannelKind 把它解析成实际地址并覆写 Target；解析失败
	// （尤其 ErrNoContact）直接标记发送失败，不重试。为空时是团队渠道的现行为，
	// Target 就是调用方给定的字面量地址，完全不受影响——这也是旧 payload（没有
	// 这个字段）反序列化出的零值语义，升级期在途的 notify_send 定时器不会因为
	// 加了新字段就断链。
	TargetUserID string `json:"target_user_id,omitempty"`

	// Round 区分"同一个 (AlertID/GroupKey, Step, Channel, Target) 元组下的第几次发送
	// 意图"：调用方（internal/app 的 notifyOutbox）在构造 SendRequest 时用当时的
	// o.now().UnixNano() 赋值一次，随请求整体序列化进 outbox 的 notify_send 定时器
	// payload 持久化下来——绝不能在 Sender.Send 内部重新取 now() 现算，否则同一条
	// notify_send 定时器崩溃重投时 payload 不变但 Round 变了，会让幂等去重失效。
	// 没有这个字段时，重复提醒/组重复通知等"合法的下一轮发送"会被幂等键误判为
	// "同一次发送的重投"而永久不再发送（R6.5 要求的 repeat_interval 重发）。
	Round int64
}

// Sender 是可靠发送的编排层（R6.1/R6.3/R13.6）：占位写入 notification_log 做幂等去重，
// 调用 Channel 发送并按指数退避重试，最终把结果（成功/耗尽重试后失败）回写状态，
// 失败时补一条时间线记录。
type Sender struct {
	tx       txRunner
	logs     logStore
	timeline timelineAppender

	// lookup 默认是包级 Get（生产环境走全局渠道注册表）；测试可替换成指向局部 map 的
	// 闭包，避免各测试用例之间共享全局注册表状态。
	lookup func(kind string) (Channel, error)

	// Resolver 可选注入（nil 时到人发送一律按解析失败处理）：由 internal/app 装配
	// 基于 UserRepo 的实现（任务 9）。不放进 NewSender 参数列表是为了不破坏既有
	// 调用方（internal/app/app.go 已有一处 NewSender(...)），构造后按需赋值即可，
	// 与 MaxAttempts 等三个可调字段同一约定。
	Resolver ContactResolver

	// 以下三项均有默认值（见 defaultMaxAttempts 等常量），构造后可直接覆盖用于调参/测试。
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// NewSender 构造 Sender。tx/logs/timeline 通常直接传 *store.Store、
// *store.NotificationLogRepo、*store.TimelineRepo（三者结构上满足对应的小接口）。
func NewSender(tx txRunner, logs logStore, timeline timelineAppender) *Sender {
	return &Sender{
		tx:             tx,
		logs:           logs,
		timeline:       timeline,
		lookup:         Get,
		MaxAttempts:    defaultMaxAttempts,
		InitialBackoff: defaultInitialBackoff,
		MaxBackoff:     defaultMaxBackoff,
	}
}

// idempotencyKey 拼接出确定性的幂等键：同一告警/分组 + 同一级 + 同一渠道 + 同一
// target + 同一 Round（同一次"发送意图"，见 SendRequest.Round 的文档），无论调用方
// 重试多少次、进程崩溃后重投多少次，都能算出同一个键，从而命中 notification_log 的
// UNIQUE(idempotency_key) 去重；而下一轮 repeat/升级会带上不同的 Round，天然算出
// 不同的键，不会被误判为重投。
func idempotencyKey(req SendRequest) string {
	resourceKey := req.AlertID
	if resourceKey == "" {
		resourceKey = req.GroupKey
	}
	step := "-"
	if req.Step != nil {
		step = strconv.Itoa(*req.Step)
	}
	return strings.Join([]string{resourceKey, step, req.ChannelKind, req.Target, strconv.FormatInt(req.Round, 10)}, idempotencyKeySep)
}

// Send 执行一次可靠发送。流程：
//  0. TargetUserID 非空时先解析成实际地址并覆写 Target（见 resolveTarget）；解析失败
//     时跳过下面第 2 步的渠道查找与重试，直接按失败处理（不重试）。
//  1. 占位写入 notification_log（status=pending）；撞幂等键冲突时查已有记录：
//     已 sent → 直接返回成功，不重发；pending/failed → 继续走下面的发送流程（这正是
//     崩溃恢复重投的意义：之前那次要么没真正发出去，要么发送后没能确认成功）。
//  2. 查渠道、发送 + 指数退避重试——这是外部 IO，不放在数据库事务里。
//  3. 按发送结果回写状态（sent 或 failed），失败且是告警级发送时补一条时间线记录。
//
// 幂等键（含 Round，见 SendRequest.Round）实际兜的是"同一条 notify_send 定时器被
// scheduler 逻辑性地重复调度"这一类重复（如 pipeline 侧多次 ScheduleIfAbsent/Schedule
// 命中同一个业务元组），不是"进程崩溃窗口内的重投"——在当前 per-timer 事务模型下，
// notification_log 占位写入与该定时器的 markFired 同属一个事务提交，一旦事务已提交、
// Sender.Send 正在执行外部 IO（HTTP/短信）时进程崩溃，下次重投会看到 status=pending
// 而不是 sent，从而真的把这条消息再发一次——这是 at-least-once 语义 + 外部 IO
// 无法参与数据库事务的固有代价，MVP 阶段已经过 team-lead 裁决接受，不指望唯一键堵住
// 这个窗口。
func (s *Sender) Send(ctx context.Context, req SendRequest) error {
	resolveErr := s.resolveTarget(ctx, &req)

	key := idempotencyKey(req)

	logID, resumeAttempts, alreadySent, err := s.placeholder(ctx, req, key)
	if err != nil {
		return err
	}
	if alreadySent {
		return nil
	}

	if resolveErr != nil {
		return s.markFailed(ctx, req, logID, resumeAttempts, resolveErr)
	}

	channel, lookupErr := s.lookup(req.ChannelKind)
	var attemptsThisCall int
	var sendErr error
	if lookupErr != nil {
		sendErr = lookupErr
	} else {
		attemptsThisCall, sendErr = s.sendWithBackoff(ctx, channel, req)
	}
	totalAttempts := resumeAttempts + attemptsThisCall

	if sendErr == nil {
		return s.tx.WithTx(ctx, func(tx pgx.Tx) error {
			if err := s.logs.UpdateStatus(ctx, tx, logID, "sent", totalAttempts, ""); err != nil {
				return err
			}
			return s.appendResultTimeline(ctx, tx, req, domain.TimelineActionNotifySent, totalAttempts, "sent", "")
		})
	}
	return s.markFailed(ctx, req, logID, totalAttempts, sendErr)
}

// resolveTarget 在 req.TargetUserID 非空时把它解析成本次发送的实际 Target（C3
// 「通知送达到人」）。用独立的只读小事务调用 Resolver，而不是复用 outbox 侧那个
// 持锁的编排事务——outbox 的四个通知意图接口本就在持锁事务内被调（见包顶注释的
// 不变量 4：只准 Schedule 一条 notify_send 定时器，禁止同步外部 IO），把"查
// users 表解析联系方式"这种可能变慢的操作放进那把锁里会拉长持锁时间；挪到 Sender
// 这一步单开一个短事务，既不占 outbox 的锁，也让"用户查不到联系方式"这类必然失败
// 提前在这里短路，不用等到后面真正尝试外部 IO。
//
// 返回值非 nil 时，调用方（Send）应跳过渠道查找与 sendWithBackoff，直接把返回的
// error 当作发送失败处理——缺联系方式或解析出错都是"再试也没用"，重试没有意义。
// 无论解析成功还是失败，req.Target 都会被覆写：成功覆写为解析出的真实地址（幂等键
// 因此自然带上解析后地址）；失败覆写为 "user:"+TargetUserID 占位（真实地址未知，
// 但 notification_log.target 和幂等键仍需要一个确定性的值）。
func (s *Sender) resolveTarget(ctx context.Context, req *SendRequest) error {
	if req.TargetUserID == "" {
		return nil
	}
	if s.Resolver == nil {
		req.Target = "user:" + req.TargetUserID
		return fmt.Errorf("notify: 未配置 ContactResolver，无法把用户 %s 解析为 %s 渠道地址", req.TargetUserID, req.ChannelKind)
	}

	var addr string
	err := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var resolveErr error
		addr, resolveErr = s.Resolver.Resolve(ctx, tx, req.TargetUserID, req.ChannelKind)
		return resolveErr
	})
	if err != nil {
		req.Target = "user:" + req.TargetUserID
		if errors.Is(err, ErrNoContact) {
			return fmt.Errorf("notify: 用户缺 %s 渠道联系方式: %w", req.ChannelKind, err)
		}
		return fmt.Errorf("notify: 解析用户 %s 的 %s 渠道地址失败: %w", req.TargetUserID, req.ChannelKind, err)
	}
	req.Target = addr
	return nil
}

// placeholder 占位写入 notification_log。返回既有记录（撞幂等键时）或新建记录的 ID，
// resumeAttempts 是既有记录已经累计的尝试次数（新建时为 0），alreadySent 表示既有记录
// 是否已经是 sent（此时调用方应直接返回成功，不重发）。
func (s *Sender) placeholder(ctx context.Context, req SendRequest, key string) (logID string, resumeAttempts int, alreadySent bool, err error) {
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		created, insertErr := s.logs.Insert(ctx, tx, store.NotificationLogEntry{
			AlertID:        req.AlertID,
			GroupKey:       req.GroupKey,
			Channel:        req.ChannelKind,
			Target:         req.Target,
			Step:           req.Step,
			IdempotencyKey: key,
		})
		if errors.Is(insertErr, store.ErrIdempotencyConflict) {
			existing, getErr := s.logs.GetByIdempotencyKey(ctx, tx, key)
			if getErr != nil {
				return fmt.Errorf("notify: 查询既有幂等记录失败: %w", getErr)
			}
			logID = existing.ID
			resumeAttempts = existing.Attempts
			alreadySent = existing.Status == "sent"
			return nil
		}
		if insertErr != nil {
			return fmt.Errorf("notify: 占位写入通知记录失败: %w", insertErr)
		}
		logID = created.ID
		return nil
	})
	return logID, resumeAttempts, alreadySent, err
}

// markFailed 把 notification_log 标记为 failed，并按 appendResultTimeline 的扇出规则
// 补时间线记录（含分组通知——按 Msg.AlertIDs 逐条扇出，不再只处理单告警发送）。
func (s *Sender) markFailed(ctx context.Context, req SendRequest, logID string, totalAttempts int, sendErr error) error {
	lastError := sendErr.Error()
	// detail 里的 error 截断到 200 字符：时间线是给人看的摘要，不是错误堆栈全文；
	// notification_log.last_error 保留完整文本供排障，两者用途不同分开处理。
	detailError := lastError
	if len(detailError) > 200 {
		detailError = detailError[:200]
	}
	updateErr := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.logs.UpdateStatus(ctx, tx, logID, "failed", totalAttempts, lastError); err != nil {
			return err
		}
		return s.appendResultTimeline(ctx, tx, req, domain.TimelineActionNotifyFailed, totalAttempts, "failed", detailError)
	})
	if updateErr != nil {
		return fmt.Errorf("notify: 发送失败(%v)且回写状态失败: %w", sendErr, updateErr)
	}
	return sendErr
}

// appendResultTimeline 把一次发送的最终结果（sent/failed）追加到关联告警的时间线：
// 单告警发送（req.AlertID 非空）落在该告警上；分组发送没有单一 AlertID，按
// req.Msg.AlertIDs（组内全部告警，见 Message.AlertIDs 注释）逐条扇出——这也补上了
// 旧版本"分组通知失败不落时间线"的缺口。两者皆空（ops 运营任务的合成 GroupKey
// 消息，无关联告警）时不追加，符合预期。
//
// 幂等性：本函数与 UpdateStatus 同一个 WithTx 事务提交，调用方（Send）只在
// placeholder 阶段判定"非 alreadySent"之后才会走到这里，重投的判重完全靠那个
// alreadySent 短路兜底，这里不需要也不做额外去重——failed 后重投再 sent 会产生
// 一条 failed + 一条 sent，是真实发送历史，予以保留。
func (s *Sender) appendResultTimeline(ctx context.Context, tx pgx.Tx, req SendRequest, action domain.TimelineAction, attempts int, status, detailError string) error {
	alertIDs := req.Msg.AlertIDs
	if req.AlertID != "" {
		alertIDs = []string{req.AlertID}
	}
	if len(alertIDs) == 0 {
		return nil
	}

	var step any
	if req.Step != nil {
		step = *req.Step
	}
	detail := map[string]any{
		"channel":  req.ChannelKind,
		"target":   maskTarget(req.ChannelKind, req.Target),
		"step":     step,
		"attempts": attempts,
		"status":   status,
	}
	if detailError != "" {
		detail["error"] = detailError
	}

	now := time.Now()
	for _, alertID := range alertIDs {
		if _, err := s.timeline.Append(ctx, tx, domain.TimelineEntry{
			AlertID: alertID,
			Actor:   "notify-sender",
			Action:  action,
			At:      now,
			Detail:  detail,
		}); err != nil {
			return err
		}
	}
	return nil
}

// sendWithBackoff 反复调用 ch.Send，失败后按指数退避 + jitter 等待再重试，尊重 ctx 取消。
// 返回值 attempts 是本次调用实际发起的 Channel.Send 次数（成功或耗尽重试为止）。
func (s *Sender) sendWithBackoff(ctx context.Context, ch Channel, req SendRequest) (attempts int, err error) {
	maxAttempts := s.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	backoff := s.InitialBackoff
	if backoff <= 0 {
		backoff = defaultInitialBackoff
	}
	maxBackoff := s.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}

	for attempts = 1; attempts <= maxAttempts; attempts++ {
		err = ch.Send(ctx, req.Target, req.Msg)
		if err == nil {
			return attempts, nil
		}
		if attempts == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return attempts, ctx.Err()
		case <-time.After(backoffWithJitter(backoff, attempts, maxBackoff)):
		}
	}
	return attempts, err
}

// backoffWithJitter 计算第 attempt 次失败后到下一次重试之间的等待时长：以 base 为基数
// 指数增长（2^(attempt-1)），封顶 maxBackoff，再叠加 full jitter（[0, 计算值) 之间随机），
// 避免大量并发重试在同一时刻扎堆打向下游渠道。
func backoffWithJitter(base time.Duration, attempt int, maxBackoff time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}
