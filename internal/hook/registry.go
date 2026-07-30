package hook

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"alert-system/internal/domain"
)

// defaultTimeout 是每个 Func 的执行超时上限（R11.1/R11.6/R12.6：LLM/自愈等旁路
// 插件不可用或响应慢时，核心告警链路不受影响）。
const defaultTimeout = 2 * time.Second

// Phase 标识钩子挂载在某个 stage 的前置还是后置。
type Phase string

const (
	Before Phase = "before"
	After  Phase = "after"
)

// Func 是一个管道扩展点插件：拿到当前 stage 名称、所处阶段与告警的当前状态，
// 可以做任意旁路操作（如异步产出 LLM 标注、指标上报、通知外部系统）。
//
// Func 不能影响主管道的处理结果：它的返回值只会被 Fire 记录为 WARN 日志，不会
// 传播给调用方；它的 panic 会被 recover；它如果超过 2s 未返回，Fire 也会直接
// 放弃等待并继续——三者都不会让核心管道感知到（R11.2/R11.6/D6）。
type Func func(ctx context.Context, stage string, phase Phase, alert domain.Alert) error

// Registry 是管道扩展点注册表：按 (stage, phase) 维度注册任意数量的 Func。
// 本期各具体扩展点（LLM 分析、自愈触发等）尚未实现，Registry 允许注册 0 个 Func，
// 此时 Fire 是空操作。
type Registry struct {
	mu    sync.RWMutex
	hooks map[string][]Func
}

// New 构造一个空的 Registry。
func New() *Registry {
	return &Registry{hooks: make(map[string][]Func)}
}

func key(stage string, phase Phase) string {
	return stage + "/" + string(phase)
}

// Register 给 stage 的 phase 阶段追加一个 Func。同一 (stage, phase) 可以注册多个，
// Fire 时按注册顺序依次调用。并发安全。
func (r *Registry) Register(stage string, phase Phase, fn Func) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(stage, phase)
	r.hooks[k] = append(r.hooks[k], fn)
}

// Fire 依次调用 stage/phase 下注册的所有 Func。每个 Func 独立套用 2s 超时与 panic
// 恢复，失败只记录 WARN，不返回错误给调用方——Fire 本身没有返回值，管道编排器
// 无法感知、也不应该感知某个扩展点是否成功（R11.6）。
func (r *Registry) Fire(ctx context.Context, stage string, phase Phase, alert domain.Alert) {
	r.mu.RLock()
	fns := append([]Func(nil), r.hooks[key(stage, phase)]...)
	r.mu.RUnlock()

	for _, fn := range fns {
		invoke(ctx, stage, phase, alert, fn)
	}
}

// invoke 执行单个 Func，保证在 defaultTimeout 内返回，无论 fn 是否响应 ctx 取消
// 或直接阻塞不归——超时后 invoke 不再等待，但 fn 所在的 goroutine 仍可能在后台
// 跑完（Go 没有强制杀死 goroutine 的手段，这里是尽力而为：done 是带缓冲的
// channel，即使 invoke 已经返回，fn 写入结果时也不会阻塞、不会泄漏 goroutine）。
func invoke(parent context.Context, stage string, phase Phase, alert domain.Alert, fn Func) {
	ctx, cancel := context.WithTimeout(parent, defaultTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("hook panic: %v", p)
			}
		}()
		done <- fn(ctx, stage, phase, alert)
	}()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("hook: stage=%s phase=%s 执行失败: %v", stage, phase, err)
		}
	case <-ctx.Done():
		log.Printf("hook: stage=%s phase=%s 执行超时（%s），已旁路跳过", stage, phase, defaultTimeout)
	}
}
