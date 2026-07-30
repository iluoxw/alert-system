package enrich

import (
	"context"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/hook"
)

// DefaultLookupTimeout 是 Lookup 自身对 CMDB 查询施加的超时上限。它由 Lookup 自己
// 持有，不依赖调用方传入的 ctx 有没有设置 deadline——即便 hook.Registry.Fire 本身也会
// 给每个 Func 套一层 2s 超时（见 internal/hook/registry.go），Lookup 独立自带同等约束
// 是为了在 hook 管道之外（如未来 R11.3 的 LLM 根因调查链直接调用 Lookup）也能保证
// 同样的「CMDB 不可用/响应慢时快速旁路，不阻塞调用方」的语义（R11.6/R12.6）。
const DefaultLookupTimeout = 2 * time.Second

// Enrichment 是一次 CMDB 上下文查询的结果：resource_ref 关联的组件/业务信息
// （对应 R11.3 根因调查链的第一跳：告警 instance -> 关联组件/业务）。字段本期为空
// 占位，真实 CMDB 客户端接入（二期）时再补充具体结构。
type Enrichment struct {
	ResourceRef map[string]string // 原样回传的查询键，便于调用方核对
	Attributes  map[string]any    // CMDB 返回的关联属性，本期恒为空 map
}

// Lookup 是 CMDB 上下文富化的扩展点接口（R7/R11.3）：按 resourceRef 查询关联的组件/
// 业务信息。实现应在内部自行施加超时并在失败/超时时返回 error，调用方（hook）据此
// 记 WARN，不阻塞主管道（R11.6/R12.6）。
type Lookup interface {
	Lookup(ctx context.Context, resourceRef map[string]string) (Enrichment, error)
}

// StubClient 是 Lookup 的空实现：真实 CMDB 客户端是二期工作（本期 CMDB 数据模型/接入
// 协议尚未确定，见 requirements.md Q4）。StubClient 恒返回空 Enrichment，不产生任何
// error、不发起任何外部调用，但仍然自带 DefaultLookupTimeout 超时保护，保持与未来
// 真实实现同样的调用约定，届时替换实现不需要改调用方代码。
type StubClient struct {
	Timeout time.Duration
}

// NewStubClient 构造 StubClient；timeout <= 0 时回退到 DefaultLookupTimeout。
func NewStubClient(timeout time.Duration) *StubClient {
	if timeout <= 0 {
		timeout = DefaultLookupTimeout
	}
	return &StubClient{Timeout: timeout}
}

// Lookup 实现 Lookup 接口：本期恒返回空结果，不做任何真实查询。仍然套用
// context.WithTimeout 并在超时时返回 ctx.Err()，与未来真实实现保持一致的超时语义。
func (c *StubClient) Lookup(ctx context.Context, resourceRef map[string]string) (Enrichment, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultLookupTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-ctx.Done():
		return Enrichment{}, ctx.Err()
	default:
		return Enrichment{ResourceRef: resourceRef, Attributes: map[string]any{}}, nil
	}
}

// RegisterHook 把 client 挂接到 hook.Registry 的 (stage, phase) 扩展点上（R7/R11.3/R12.6）。
//
// 挂载点选择：与 action.RegisterHook 相同，挂在 "dedup" stage 的 After 阶段——理由同样是
// pctx.Alert.ResourceRef 只有在 dedup upsert 之后才第一次被填充，此前挂钩子拿到的
// resourceRef 是空 map，查了也没有意义（见 internal/pipeline/pipeline.go process()）。
//
// Lookup 的返回值（error）直接作为 hook.Func 的返回值：hook.Registry.Fire 会把非 nil
// error 记成一条 WARN 日志，不会向管道其余部分传播（R11.6/R12.6）。本期没有落库承接
// 富化结果的表——annotations 表是 R11 LLM 标注场景的容器，CMDB 富化结果的持久化留给
// 二期真实客户端接入时一并设计。
func RegisterHook(registry *hook.Registry, client Lookup) {
	registry.Register("dedup", hook.After, func(ctx context.Context, stage string, phase hook.Phase, alert domain.Alert) error {
		_, err := client.Lookup(ctx, alert.ResourceRef)
		return err
	})
}
