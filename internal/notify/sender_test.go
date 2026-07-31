package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// fakeTx 让 WithTx 的 fake 实现直接以 nil pgx.Tx 调用 fn：本包测试里的 fake
// logStore/timelineAppender 都不真正使用 tx 参数，只是透传满足签名。
type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return fn(nil)
}

// fakeLogStore 是 logStore 的内存实现，按 idempotency_key 去重，
// 用 mutex 保护以兼容 -race。
type fakeLogStore struct {
	mu      sync.Mutex
	byKey   map[string]*store.NotificationLogEntry
	nextID  int
	inserts int // Insert 被调用的次数（含冲突命中），用于断言不重复占位
}

func newFakeLogStore() *fakeLogStore {
	return &fakeLogStore{byKey: map[string]*store.NotificationLogEntry{}}
}

func (f *fakeLogStore) Insert(ctx context.Context, tx pgx.Tx, e store.NotificationLogEntry) (*store.NotificationLogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserts++
	if _, ok := f.byKey[e.IdempotencyKey]; ok {
		return nil, store.ErrIdempotencyConflict
	}
	f.nextID++
	e.ID = string(rune('a' - 1 + f.nextID))
	e.Status = "pending"
	f.byKey[e.IdempotencyKey] = &e
	cp := e
	return &cp, nil
}

func (f *fakeLogStore) GetByIdempotencyKey(ctx context.Context, tx pgx.Tx, key string) (*store.NotificationLogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.byKey[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (f *fakeLogStore) UpdateStatus(ctx context.Context, tx pgx.Tx, id, status string, attempts int, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byKey {
		if e.ID == id {
			e.Status = status
			e.Attempts = attempts
			e.LastError = lastError
			return nil
		}
	}
	return store.ErrNotFound
}

// fakeTimeline 记录 Append 调用，用于断言失败时是否补写了时间线。
type fakeTimeline struct {
	mu      sync.Mutex
	entries []domain.TimelineEntry
}

func (f *fakeTimeline) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return "tl-1", nil
}

func (f *fakeTimeline) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// fakeChannel 记录 Send 调用次数，可配置固定失败或按序列返回结果。
type fakeChannel struct {
	kind string

	mu    sync.Mutex
	calls int
	fn    func(callIndex int) error // callIndex 从 1 开始
}

func (c *fakeChannel) Kind() string { return c.kind }

func (c *fakeChannel) Send(ctx context.Context, target string, msg Message) error {
	c.mu.Lock()
	c.calls++
	idx := c.calls
	c.mu.Unlock()
	if c.fn != nil {
		return c.fn(idx)
	}
	return nil
}

func (c *fakeChannel) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newSenderForTest(logs *fakeLogStore, tl *fakeTimeline, ch Channel) *Sender {
	s := NewSender(fakeTx{}, logs, tl)
	s.lookup = func(kind string) (Channel, error) {
		if ch != nil && kind == ch.Kind() {
			return ch, nil
		}
		return nil, ErrChannelNotRegistered
	}
	// 测试里不需要真实等待，退避时长压到极小以加快用例执行。
	s.InitialBackoff = time.Millisecond
	s.MaxBackoff = 5 * time.Millisecond
	return s
}

// 场景①：重试耗尽 → notification_log 标记 failed，且（告警级发送）补一条时间线记录。
func TestSender_RetryExhaustion_MarksFailedAndAppendsTimeline(t *testing.T) {
	ch := &fakeChannel{kind: "webhook", fn: func(int) error { return errors.New("boom") }}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)
	s.MaxAttempts = 3

	req := SendRequest{AlertID: "alert-1", ChannelKind: "webhook", Target: "http://x"}
	err := s.Send(context.Background(), req)
	if err == nil {
		t.Fatal("期望 Send 返回错误（重试耗尽）")
	}
	if got := ch.callCount(); got != 3 {
		t.Fatalf("期望 Channel.Send 被调用 3 次，实际 %d 次", got)
	}

	entry, getErr := logs.GetByIdempotencyKey(context.Background(), nil, idempotencyKey(req))
	if getErr != nil {
		t.Fatalf("查询通知记录失败: %v", getErr)
	}
	if entry.Status != "failed" {
		t.Fatalf("期望状态为 failed，实际 %q", entry.Status)
	}
	if entry.Attempts != 3 {
		t.Fatalf("期望 attempts=3，实际 %d", entry.Attempts)
	}
	if got := tl.count(); got != 1 {
		t.Fatalf("期望写入 1 条时间线记录，实际 %d 条", got)
	}
}

// 场景②：同一幂等键二次 Send，round 不变（模拟进程崩溃后重投同一条 notify_send
// 定时器——payload 没变，Round 就没变，见 SendRequest.Round 的文档），且第一次已经
// 成功发送过，第二次不应重复调用 Channel.Send。
func TestSender_SameRound_ResentAfterSent_DoesNotResend(t *testing.T) {
	ch := &fakeChannel{kind: "webhook"}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)

	req := SendRequest{AlertID: "alert-1", ChannelKind: "webhook", Target: "http://x", Round: 1000}
	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("第一次 Send 失败: %v", err)
	}
	if got := ch.callCount(); got != 1 {
		t.Fatalf("第一次发送后期望 Channel.Send 调用 1 次，实际 %d 次", got)
	}

	// 模拟崩溃重投：同样的 req（同样的幂等键，round 不变）再次调用 Send。
	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("第二次（重投）Send 失败: %v", err)
	}
	if got := ch.callCount(); got != 1 {
		t.Fatalf("已 sent 的记录不应重发，期望 Channel.Send 仍为 1 次，实际 %d 次", got)
	}
}

// 场景②'：AlertID/Step/Channel/Target 完全相同，但 Round 不同（代表下一轮 repeat/
// 升级的合法新发送意图，而不是同一条定时器的崩溃重投）时，应当被当成一次全新的
// 发送——这正是 Bug C 的回归用例：幂等键如果不区分 round，R6.5 要求的重复提醒会
// 在第一次成功发送之后永久失效。
func TestSender_DifferentRound_SameOtherFields_SendsAgain(t *testing.T) {
	ch := &fakeChannel{kind: "webhook"}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)

	req1 := SendRequest{AlertID: "alert-1", ChannelKind: "webhook", Target: "http://x", Round: 1000}
	if err := s.Send(context.Background(), req1); err != nil {
		t.Fatalf("第一轮 Send 失败: %v", err)
	}

	req2 := req1
	req2.Round = 2000
	if err := s.Send(context.Background(), req2); err != nil {
		t.Fatalf("第二轮（不同 round）Send 失败: %v", err)
	}

	if got := ch.callCount(); got != 2 {
		t.Fatalf("不同 round 应各自发送一次，期望 Channel.Send 调用 2 次，实际 %d 次", got)
	}
}

// 场景③：Validate 对未注册渠道报错（如占位的 "phone" 渠道）。
func TestValidate_UnregisteredChannel_ReturnsError(t *testing.T) {
	kind := "test-kind-unregistered-xyz"
	err := Validate([]string{"webhook", kind})
	// webhook 未必已被本测试进程注册，这里只关心明确未注册的 kind 会被判定失败。
	if err == nil {
		t.Fatalf("期望 Validate 对未注册渠道 %q 报错", kind)
	}
	if !errors.Is(err, ErrChannelNotRegistered) {
		t.Fatalf("期望错误链包含 ErrChannelNotRegistered，实际 %v", err)
	}
}

// 场景④：退避重试次数正确，且 ctx 取消能提前中止重试。
func TestSender_BackoffRetryCount_AndCtxCancellation(t *testing.T) {
	t.Run("重试次数等于 MaxAttempts", func(t *testing.T) {
		ch := &fakeChannel{kind: "webhook", fn: func(int) error { return errors.New("boom") }}
		logs := newFakeLogStore()
		tl := &fakeTimeline{}
		s := newSenderForTest(logs, tl, ch)
		s.MaxAttempts = 4

		req := SendRequest{AlertID: "alert-2", ChannelKind: "webhook", Target: "http://x"}
		_ = s.Send(context.Background(), req)
		if got := ch.callCount(); got != 4 {
			t.Fatalf("期望重试 4 次，实际 %d 次", got)
		}
	})

	t.Run("ctx 取消提前中止重试", func(t *testing.T) {
		ch := &fakeChannel{kind: "webhook", fn: func(int) error { return errors.New("boom") }}
		logs := newFakeLogStore()
		tl := &fakeTimeline{}
		s := newSenderForTest(logs, tl, ch)
		s.MaxAttempts = 10
		s.InitialBackoff = 50 * time.Millisecond
		s.MaxBackoff = time.Second

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()

		req := SendRequest{AlertID: "alert-3", ChannelKind: "webhook", Target: "http://x"}
		err := s.Send(ctx, req)
		if err == nil {
			t.Fatal("期望 ctx 取消后 Send 返回错误")
		}
		if got := ch.callCount(); got >= 10 {
			t.Fatalf("期望 ctx 取消提前中止重试（远少于 MaxAttempts=10），实际调用了 %d 次", got)
		}
	})
}

// 场景⑤：webhook 渠道对非 2xx 状态码返回 error。
func TestWebhookChannel_NonSuccessStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ch := NewWebhookChannel(time.Second)
	err := ch.Send(context.Background(), srv.URL, Message{Title: "t"})
	if err == nil {
		t.Fatal("期望非 2xx 状态码返回错误")
	}
}

func TestWebhookChannel_SuccessStatus_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhookChannel(time.Second)
	if err := ch.Send(context.Background(), srv.URL, Message{Title: "t"}); err != nil {
		t.Fatalf("期望 2xx 状态码不返回错误，实际: %v", err)
	}
}

// 场景⑥：发送成功后，单告警发送在 req.AlertID 上追加一条 notify_sent 时间线记录（R3）。
func TestSender_Success_AppendsNotifySentTimeline(t *testing.T) {
	ch := &fakeChannel{kind: "webhook"}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)

	req := SendRequest{AlertID: "alert-1", ChannelKind: "webhook", Target: "http://x", Round: 1}
	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := tl.count(); got != 1 {
		t.Fatalf("期望写入 1 条时间线记录，实际 %d 条", got)
	}
	entry := tl.entries[0]
	if entry.Action != domain.TimelineActionNotifySent {
		t.Fatalf("期望 Action=notify_sent，实际 %q", entry.Action)
	}
	if entry.AlertID != "alert-1" {
		t.Fatalf("期望 AlertID=alert-1，实际 %q", entry.AlertID)
	}
	if entry.Detail["status"] != "sent" {
		t.Fatalf("期望 detail.status=sent，实际 %v", entry.Detail["status"])
	}
}

// 场景⑦：分组通知没有单一 AlertID，成功后应按 Msg.AlertIDs 逐条扇出（3 个 ID → 3 条）。
func TestSender_GroupSuccess_FansOutTimelinePerAlert(t *testing.T) {
	ch := &fakeChannel{kind: "webhook"}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)

	req := SendRequest{
		GroupKey:    "group-1",
		ChannelKind: "webhook",
		Target:      "http://x",
		Round:       1,
		Msg:         Message{AlertIDs: []string{"a1", "a2", "a3"}},
	}
	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := tl.count(); got != 3 {
		t.Fatalf("期望分组扇出 3 条时间线记录，实际 %d 条", got)
	}
	seen := map[string]bool{}
	for _, e := range tl.entries {
		seen[e.AlertID] = true
		if e.Action != domain.TimelineActionNotifySent {
			t.Fatalf("期望 Action=notify_sent，实际 %q", e.Action)
		}
	}
	for _, id := range req.Msg.AlertIDs {
		if !seen[id] {
			t.Fatalf("期望包含告警 %s 的时间线记录", id)
		}
	}
}

// 场景⑧：已 sent 记录被重投（崩溃重投同一条 notify_send 定时器）时，alreadySent
// 短路直接返回，不应重复追加时间线（幂等口径见 Sender.Send 文档）。
func TestSender_AlreadySent_Resend_DoesNotAppendTimelineAgain(t *testing.T) {
	ch := &fakeChannel{kind: "webhook"}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)

	req := SendRequest{AlertID: "alert-1", ChannelKind: "webhook", Target: "http://x", Round: 1}
	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("第一次 Send 失败: %v", err)
	}
	if got := tl.count(); got != 1 {
		t.Fatalf("第一次发送后期望 1 条时间线记录，实际 %d 条", got)
	}

	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("第二次（重投）Send 失败: %v", err)
	}
	if got := tl.count(); got != 1 {
		t.Fatalf("已 sent 重投不应重复追加时间线，期望仍为 1 条，实际 %d 条", got)
	}
}

// 场景⑨：AlertID 与 Msg.AlertIDs 皆为空（ops 运营任务用合成 GroupKey 发送、无关联
// 告警）时，不追加任何时间线记录。
func TestSender_OpsGroupKeyWithoutAlertIDs_DoesNotAppendTimeline(t *testing.T) {
	ch := &fakeChannel{kind: "webhook"}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)

	req := SendRequest{GroupKey: "ops:team-1:daily_report", ChannelKind: "webhook", Target: "http://x", Round: 1}
	if err := s.Send(context.Background(), req); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := tl.count(); got != 0 {
		t.Fatalf("ops 合成消息无关联告警，期望不追加时间线，实际 %d 条", got)
	}
}

// 场景⑩：分组发送失败也要按 Msg.AlertIDs 扇出（补上旧版本"分组失败不落时间线"的
// 缺口），且 detail.error 截断到 200 字符。
func TestSender_GroupFailure_FansOutTimelineAndTruncatesError(t *testing.T) {
	longErr := strings.Repeat("x", 250)
	ch := &fakeChannel{kind: "webhook", fn: func(int) error { return errors.New(longErr) }}
	logs := newFakeLogStore()
	tl := &fakeTimeline{}
	s := newSenderForTest(logs, tl, ch)
	s.MaxAttempts = 1

	req := SendRequest{
		GroupKey:    "group-2",
		ChannelKind: "webhook",
		Target:      "http://x",
		Round:       1,
		Msg:         Message{AlertIDs: []string{"a1", "a2"}},
	}
	if err := s.Send(context.Background(), req); err == nil {
		t.Fatal("期望 Send 返回错误")
	}
	if got := tl.count(); got != 2 {
		t.Fatalf("期望失败分组扇出 2 条时间线记录，实际 %d 条", got)
	}
	for _, e := range tl.entries {
		if e.Action != domain.TimelineActionNotifyFailed {
			t.Fatalf("期望 Action=notify_failed，实际 %q", e.Action)
		}
		errText, _ := e.Detail["error"].(string)
		if len(errText) != 200 {
			t.Fatalf("期望 detail.error 截断为 200 字符，实际长度 %d", len(errText))
		}
	}
}
