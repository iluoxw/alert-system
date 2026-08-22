package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// 本文件的单元测试（无 build tag，不需要 docker）只覆盖 handler 注册/分发/panic
// 恢复这部分纯逻辑：invoke() 只依赖内存里的 handlers map 和一个 dueTimer 值，
// 与数据库完全无关，可以直接构造 &Scheduler{handlers: ...} 测试。
//
// Schedule/Cancel 的 upsert 替换语义、fetchDue 的 FOR UPDATE SKIP LOCKED、
// Run 的轮询-触发-状态回写全流程，都依赖真实 PostgreSQL 的事务/锁行为，
// 没有把它们抽象成可 fake 的接口——那样做需要在 fake 里重新实现一遍局部唯一索引
// 冲突处理和行锁语义，本质上是重新发明数据库，价值有限。这些放在同目录下
// scheduler_integration_test.go（`integration` build tag）里用 testcontainers-go
// 起真实 postgres 验证，和 store 包 events_test.go 的做法一致。
//
// 注：这里额外多出 scheduler_integration_test.go 一个文件，是因为 build tag
// 只能按文件生效——无法在同一个 scheduler_test.go 里既让部分测试默认运行、
// 又让另一部分测试只在 `-tags=integration` 时编译。已提前告知这一处理方式。

func TestScheduler_Invoke_DispatchesToRegisteredHandler(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler)}
	var gotRefID string
	var gotPayload []byte
	s.RegisterHandler("ttl", func(_ context.Context, _ pgx.Tx, refID string, payload []byte) error {
		gotRefID = refID
		gotPayload = payload
		return nil
	})

	err := s.invoke(context.Background(), nil, dueTimer{kind: "ttl", refID: "alert-1", payload: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatalf("invoke err = %v, want nil", err)
	}
	if gotRefID != "alert-1" {
		t.Fatalf("handler 收到的 refID = %q, want %q", gotRefID, "alert-1")
	}
	if string(gotPayload) != `{"x":1}` {
		t.Fatalf("handler 收到的 payload = %s, want %s", gotPayload, `{"x":1}`)
	}
}

func TestScheduler_Invoke_UnknownKind_ReturnsError(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler)}
	if err := s.invoke(context.Background(), nil, dueTimer{kind: "unregistered", refID: "x"}); err == nil {
		t.Fatal("未注册 kind 应返回错误，而不是静默成功")
	}
}

func TestScheduler_Invoke_HandlerError_Propagates(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler)}
	wantErr := errors.New("boom")
	s.RegisterHandler("x", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		return wantErr
	})

	err := s.invoke(context.Background(), nil, dueTimer{kind: "x"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("invoke err = %v, want 包含 %v", err, wantErr)
	}
}

func TestScheduler_Invoke_HandlerPanic_RecoveredAsError(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler)}
	s.RegisterHandler("x", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		panic("boom")
	})

	err := s.invoke(context.Background(), nil, dueTimer{kind: "x"})
	if err == nil {
		t.Fatal("handler panic 必须被 recover 并转换为 error，而不是继续向上传播、杀死调用方")
	}
}

func TestScheduler_InvokeDetached_DispatchesWithoutTx(t *testing.T) {
	var gotRefID string
	var gotPayload []byte
	h := func(_ context.Context, refID string, payload []byte) error {
		gotRefID = refID
		gotPayload = payload
		return nil
	}

	err := invokeDetached(context.Background(), h, dueTimer{kind: "notify_send", refID: "alert-1", payload: []byte(`{"x":1}`)})
	if err != nil {
		t.Fatalf("invokeDetached err = %v, want nil", err)
	}
	if gotRefID != "alert-1" {
		t.Fatalf("handler 收到的 refID = %q, want %q", gotRefID, "alert-1")
	}
	if string(gotPayload) != `{"x":1}` {
		t.Fatalf("handler 收到的 payload = %s, want %s", gotPayload, `{"x":1}`)
	}
}

func TestScheduler_InvokeDetached_HandlerError_Propagates(t *testing.T) {
	wantErr := errors.New("boom")
	h := func(_ context.Context, _ string, _ []byte) error { return wantErr }

	if err := invokeDetached(context.Background(), h, dueTimer{kind: "notify_send"}); !errors.Is(err, wantErr) {
		t.Fatalf("invokeDetached err = %v, want 包含 %v", err, wantErr)
	}
}

// 事务外执行的 handler panic 同样必须被 recover 成一个普通失败：它跑在轮询 worker 的
// goroutine 上（不是 fire-and-forget 协程），panic 逃逸会直接杀掉整个进程。
func TestScheduler_InvokeDetached_HandlerPanic_RecoveredAsError(t *testing.T) {
	h := func(_ context.Context, _ string, _ []byte) error { panic("boom") }

	if err := invokeDetached(context.Background(), h, dueTimer{kind: "notify_send"}); err == nil {
		t.Fatal("事务外 handler panic 必须被 recover 并转换为 error，而不是继续向上传播、杀死 worker")
	}
}

// 分发时"事务外模式"优先：同一个 kind 两种模式都注册属于装配错误，但行为必须是确定的。
func TestScheduler_DetachedHandlerFor_LookupIsIndependentOfTxHandlers(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler), detached: make(map[string]DetachedHandler)}
	s.RegisterHandler("notify_send", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error { return nil })

	if _, ok := s.detachedHandlerFor("notify_send"); ok {
		t.Fatal("只注册了事务内 handler，detachedHandlerFor 不应命中")
	}

	s.RegisterDetachedHandler("notify_send", func(_ context.Context, _ string, _ []byte) error { return nil })
	if _, ok := s.detachedHandlerFor("notify_send"); !ok {
		t.Fatal("注册了事务外 handler 后 detachedHandlerFor 应命中")
	}
	if _, ok := s.handlerFor("notify_send"); !ok {
		t.Fatal("RegisterDetachedHandler 不应静默删掉同 kind 的事务内 handler 注册")
	}
}

// TestScheduler_RegisterHandler_ConcurrentSafe 用 -race 验证 handlers map 的并发
// 读写安全：RegisterHandler 可能在 Run 已经启动、invoke 正在并发读取 handlers 时
// 被调用（例如运行时动态补注册新 kind）。
func TestScheduler_RegisterHandler_ConcurrentSafe(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler)}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := fmt.Sprintf("kind-%d", i)
			s.RegisterHandler(kind, func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error { return nil })
			_ = s.invoke(context.Background(), nil, dueTimer{kind: kind})
		}(i)
	}
	wg.Wait()
}

// 同上，但覆盖 detached 注册表：processOneDue 在轮询 goroutine 里读它，
// RegisterDetachedHandler 可能来自另一个 goroutine。
func TestScheduler_RegisterDetachedHandler_ConcurrentSafe(t *testing.T) {
	s := &Scheduler{handlers: make(map[string]Handler), detached: make(map[string]DetachedHandler)}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := fmt.Sprintf("kind-%d", i)
			s.RegisterDetachedHandler(kind, func(_ context.Context, _ string, _ []byte) error { return nil })
			if h, ok := s.detachedHandlerFor(kind); ok {
				_ = invokeDetached(context.Background(), h, dueTimer{kind: kind})
			}
		}(i)
	}
	wg.Wait()
}
