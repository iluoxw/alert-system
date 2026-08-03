package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/hook"
)

// fakeStage 是可编程的测试用 Stage：记录被调用的顺序、可注入错误、可观察/篡改
// PipelineContext。
type fakeStage struct {
	name string
	err  error
	fn   func(pctx *PipelineContext)
}

type authoritativeFakeStage struct {
	*fakeStage
	allowed bool
}

func (s *authoritativeFakeStage) RunForAuthoritativeSource() bool { return s.allowed }

func (s *fakeStage) Name() string { return s.name }

func (s *fakeStage) Process(ctx context.Context, pctx *PipelineContext) error {
	if s.fn != nil {
		s.fn(pctx)
	}
	return s.err
}

func TestPipeline_Process_StagesRunInOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(name string) func(*PipelineContext) {
		return func(*PipelineContext) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
		}
	}

	p := &Pipeline{
		hooks: hook.New(),
		stages: []Stage{
			&fakeStage{name: "dedup", fn: record("dedup")},
			&fakeStage{name: "group", fn: record("group")},
			&fakeStage{name: "silence", fn: record("silence")},
		},
	}

	pctx := &PipelineContext{}
	if err := p.process(context.Background(), pctx); err != nil {
		t.Fatalf("process() 返回意外错误: %v", err)
	}

	want := []string{"dedup", "group", "silence"}
	if len(order) != len(want) {
		t.Fatalf("stage 执行顺序 = %v, 期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("stage 执行顺序 = %v, 期望 %v", order, want)
		}
	}
}

func TestPipeline_Process_AuthoritativeSourceSkipsLocalControlStages(t *testing.T) {
	var order []string
	p := &Pipeline{
		hooks: hook.New(),
		stages: []Stage{
			&authoritativeFakeStage{fakeStage: &fakeStage{name: "dedup", fn: func(pctx *PipelineContext) {
				order = append(order, "dedup")
				pctx.AuthoritativeSource = true
			}}, allowed: true},
			&fakeStage{name: "route", fn: func(*PipelineContext) { order = append(order, "route") }},
			&authoritativeFakeStage{fakeStage: &fakeStage{name: "fault-mapping", fn: func(*PipelineContext) {
				order = append(order, "fault-mapping")
			}}, allowed: true},
			&fakeStage{name: "group", fn: func(*PipelineContext) { order = append(order, "group") }},
		},
	}
	if err := p.process(context.Background(), &PipelineContext{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(order) != 2 || order[0] != "dedup" || order[1] != "fault-mapping" {
		t.Fatalf("权威来源执行顺序=%v, want [dedup fault-mapping]", order)
	}
}

func TestPipeline_Process_StageErrorStopsSubsequentStages(t *testing.T) {
	var ran []string
	record := func(name string) func(*PipelineContext) {
		return func(*PipelineContext) { ran = append(ran, name) }
	}

	wantErr := errors.New("dedup 失败")
	p := &Pipeline{
		hooks: hook.New(),
		stages: []Stage{
			&fakeStage{name: "dedup", err: wantErr, fn: record("dedup")},
			&fakeStage{name: "group", fn: record("group")},
		},
	}

	err := p.process(context.Background(), &PipelineContext{})
	if err == nil {
		t.Fatal("process() 期望返回错误，实际为 nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("process() 错误 = %v, 期望包裹 %v", err, wantErr)
	}
	if len(ran) != 1 || ran[0] != "dedup" {
		t.Fatalf("已执行 stage = %v, 期望仅 [dedup]（group 不应被执行）", ran)
	}
}

func TestPipeline_Process_HookPanicDoesNotInterruptPipeline(t *testing.T) {
	reg := hook.New()
	reg.Register("dedup", hook.Before, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		panic("hook 故意 panic")
	})

	var groupRan bool
	p := &Pipeline{
		hooks: reg,
		stages: []Stage{
			&fakeStage{name: "dedup"},
			&fakeStage{name: "group", fn: func(*PipelineContext) { groupRan = true }},
		},
	}

	if err := p.process(context.Background(), &PipelineContext{}); err != nil {
		t.Fatalf("process() 因 hook panic 返回了错误: %v", err)
	}
	if !groupRan {
		t.Fatal("hook panic 不应中断管道，group stage 应当照常执行")
	}
}

func TestPipeline_Process_HookErrorDoesNotInterruptPipeline(t *testing.T) {
	reg := hook.New()
	reg.Register("dedup", hook.After, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		return errors.New("hook 故意失败")
	})

	var groupRan bool
	p := &Pipeline{
		hooks: reg,
		stages: []Stage{
			&fakeStage{name: "dedup"},
			&fakeStage{name: "group", fn: func(*PipelineContext) { groupRan = true }},
		},
	}

	if err := p.process(context.Background(), &PipelineContext{}); err != nil {
		t.Fatalf("process() 因 hook 失败返回了错误: %v", err)
	}
	if !groupRan {
		t.Fatal("hook 失败不应中断管道，group stage 应当照常执行")
	}
}

func TestPipeline_Process_HookSeesUpdatedAlertBetweenStages(t *testing.T) {
	var beforeGroupTitle, afterDedupTitle string

	reg := hook.New()
	reg.Register("dedup", hook.After, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		afterDedupTitle = alert.Title
		return nil
	})
	reg.Register("group", hook.Before, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		beforeGroupTitle = alert.Title
		return nil
	})

	p := &Pipeline{
		hooks: reg,
		stages: []Stage{
			&fakeStage{name: "dedup", fn: func(pctx *PipelineContext) {
				pctx.Alert.Title = "写入后的标题"
			}},
			&fakeStage{name: "group"},
		},
	}

	if err := p.process(context.Background(), &PipelineContext{}); err != nil {
		t.Fatalf("process() 返回意外错误: %v", err)
	}
	if afterDedupTitle != "写入后的标题" {
		t.Fatalf("dedup 的 after hook 看到 Title = %q, 期望 %q", afterDedupTitle, "写入后的标题")
	}
	if beforeGroupTitle != "写入后的标题" {
		t.Fatalf("group 的 before hook 看到 Title = %q, 期望 %q", beforeGroupTitle, "写入后的标题")
	}
}

func TestPipeline_Process_HookTimeoutBoundedReturn(t *testing.T) {
	reg := hook.New()
	var called int32
	reg.Register("dedup", hook.Before, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		atomic.AddInt32(&called, 1)
		<-ctx.Done() // 模拟一个永不主动返回、只响应超时取消的慢插件
		return ctx.Err()
	})

	p := &Pipeline{
		hooks: reg,
		stages: []Stage{
			&fakeStage{name: "dedup"},
		},
	}

	start := time.Now()
	err := p.process(context.Background(), &PipelineContext{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("process() 返回意外错误: %v", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("hook 应当被调用了一次")
	}
	// hook 内部超时是 2s；process 不应该比这个明显更久（留出调度余量）。
	if elapsed > 3*time.Second {
		t.Fatalf("process() 耗时 %v，超过了 hook 超时应有的上界", elapsed)
	}
}

// 编译期检查 fakeStage 实现了 Stage，避免类型漂移。
var _ Stage = (*fakeStage)(nil)
