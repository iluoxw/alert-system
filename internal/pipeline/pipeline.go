package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/hook"
	"alert-system/internal/store"
)

// defaultBatchSize 是 Run 每一轮从 EventQueue 取出的最大事件数。
const defaultBatchSize = 20

// idleBackoff 是队列为空时的退避等待：避免忙等把连接池打满，同时比定时轮询
// 的固定 tick 更快感知到新事件（不像 scheduler 那样用固定周期的 ticker）。
const idleBackoff = 200 * time.Millisecond

// Stage 是管道处理阶段的统一接口。去重/分组/静默/抑制/路由等具体 stage
// （internal/pipeline/stage_*.go，均属于本包，由后续任务实现）都实现该接口。
// 编排器本身只依赖这个接口，不引用任何具体 stage 类型：由 main 在启动时组装好
// []Stage 传给 New，新增/替换/调整 stage 顺序都不需要改动编排器（D6 可插拔）。
type Stage interface {
	Name() string
	Process(ctx context.Context, pctx *PipelineContext) error
}

// RouteResult 是路由 stage（任务 16）的产出：命中的路由规则（或默认团队兜底）及其
// 分组/重复通知参数。这是分组 stage（任务 14，用 RouteID+GroupBy 计算 group_key、
// 用 GroupWait/GroupInterval 注册分组定时器）与通知策略引擎（任务 22，用 TeamID
// 求值策略绑定）依赖的上游契约，字段一旦确定不应轻易变动。
//   - RouteID 是命中的路由规则 ID；未命中任何规则、落到默认团队兜底时为空字符串。
//   - TeamID 始终有值：命中路由时是该路由的 TargetTeamID，否则是构造 RouteStage
//     时注入的默认团队。
//   - GroupBy/GroupWait/GroupInterval/RepeatInterval 只在命中某条路由时有意义；
//     默认团队兜底路径没有路由记录可取这些参数，均为零值——分组 stage 需要自行
//     定义零值语义（如零值 GroupWait 视为不等待、立即分组）。
type RouteResult struct {
	RouteID        string
	TeamID         string
	GroupBy        []string
	GroupWait      time.Duration
	GroupInterval  time.Duration
	RepeatInterval time.Duration
}

// PipelineContext 携带一个事件在管道各 stage 间流转的可变状态。
//
//   - Event 是原始的统一告警事件信封，理论上只有去重 stage（负责 upsert 出 Alert）
//     需要读取它；R2.3 要求去重之后的核心管道逻辑只依赖 Alert 的信封字段，不感知
//     Event.SignalContext。
//   - IntegrationID 是产生该事件的 integration（来自 alert_events.integration_id），
//     供去重 stage 调 UpsertByFingerprint 及 severity 兜底使用；未知来源时为空字符串。
//   - Alert 是去重 stage upsert/合并后的持久化告警，是分组/静默/抑制/路由等后续
//     stage 的主要读写对象。
//   - Created 标识本轮去重是新建告警（true）还是合并进已有告警（false），供分组/
//     通知等后续 stage 判断是否需要发起新通知（R3.4：合并不重复通知）。
//   - Route 是路由 stage 的产出。
//   - Tx 是本轮 pollOnce 所在的数据库事务：去重 stage 需要在同一事务内完成
//     UpsertByFingerprint/UpdateStatus 与 Schedule/Cancel TTL 定时器，才能与
//     pollOnce 最终的 MarkProcessed/MarkFailed 一并原子提交或回滚（store/scheduler
//     全套仓储方法都以 pgx.Tx 为入参，正是为了让调用方在编译期无法绕开这个约束）。
type PipelineContext struct {
	Event         domain.AlertEvent
	IntegrationID string
	Alert         domain.Alert
	Created       bool
	Route         RouteResult
	Tx            pgx.Tx
}

// Pipeline 是管道编排器：从 EventQueue 消费事件，依序驱动 stages，每个 stage
// 前后触发 hooks 里注册的扩展点（R11.1）。
type Pipeline struct {
	stages []Stage
	hooks  *hook.Registry

	store     *store.Store
	queue     *store.EventQueue
	batchSize int
}

// New 构造 Pipeline。stages 按传入顺序依次执行（典型顺序：去重 -> 分组 -> 静默 ->
// 抑制 -> 路由），具体是哪些 stage、以什么顺序组合由调用方（main）决定，编排器
// 本身不关心。
func New(st *store.Store, queue *store.EventQueue, hooks *hook.Registry, stages []Stage) *Pipeline {
	return &Pipeline{
		stages:    stages,
		hooks:     hooks,
		store:     st,
		queue:     queue,
		batchSize: defaultBatchSize,
	}
}

// process 依序执行 stages，每个 stage 前后触发 hook.Registry.Fire。
// stage 返回 error 会立即中断后续 stage（该事件处理失败，交给调用方 MarkFailed）；
// hook 的失败/超时/panic 已经在 Registry.Fire 内部被隔离，不会传播到这里、也不会
// 影响 stage 的执行结果（R11.6）。
func (p *Pipeline) process(ctx context.Context, pctx *PipelineContext) error {
	for _, s := range p.stages {
		p.hooks.Fire(ctx, s.Name(), hook.Before, pctx.Alert)
		if err := s.Process(ctx, pctx); err != nil {
			return fmt.Errorf("pipeline: stage %s 处理失败: %w", s.Name(), err)
		}
		p.hooks.Fire(ctx, s.Name(), hook.After, pctx.Alert)
	}
	return nil
}

// pollOnce 在一个事务内取出最多 batchSize 条待处理事件，逐条反序列化为
// PipelineContext 并驱动 stage 链，成功则 MarkProcessed，某个 stage 失败则
// MarkFailed（记录该 stage 的错误原因）。返回本轮实际取到的事件数，0 表示队列
// 已空，供 Run 据此退避。
func (p *Pipeline) pollOnce(ctx context.Context) (int, error) {
	var n int
	err := p.store.WithTx(ctx, func(tx pgx.Tx) error {
		batch, err := p.queue.Dequeue(ctx, tx, p.batchSize)
		if err != nil {
			return err
		}
		n = len(batch)
		for _, ev := range batch {
			pctx := &PipelineContext{Event: ev.Event, IntegrationID: ev.IntegrationID, Tx: tx}
			if err := p.process(ctx, pctx); err != nil {
				log.Printf("pipeline: 事件 %s 处理失败: %v", ev.ID, err)
				if merr := p.queue.MarkFailed(ctx, tx, ev.ID, err.Error()); merr != nil {
					return merr
				}
				continue
			}
			if merr := p.queue.MarkProcessed(ctx, tx, []string{ev.ID}); merr != nil {
				return merr
			}
		}
		return nil
	})
	return n, err
}

// Run 是轮询 worker：不断从 EventQueue 取出待处理事件并驱动 stage 链，直到 ctx
// 被取消。队列为空时退避 idleBackoff 再试，避免忙等打满连接池。
func (p *Pipeline) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := p.pollOnce(ctx)
		if err != nil {
			log.Printf("pipeline: 轮询失败: %v", err)
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(idleBackoff):
			}
		}
	}
}
