// Package e2e 驱动任务 31 的四个端到端场景：真实 Postgres + 真实 scheduler/pipeline
// worker + mock 通知渠道，只用秒级配置压缩时钟等待（design.md 三场景 + R13 样例）。
package e2e

import (
	"context"
	"sync"

	"alert-system/internal/notify"
)

// mockSend 记录一次 mockChannel.Send 调用的入参快照，供测试断言"收到了什么"。
type mockSend struct {
	Target string
	Msg    notify.Message
}

// mockChannel 是给 E2E 断言用的假渠道：不做任何外部 IO，只把发送记录追加到内存
// 切片。同一个 kind（如 "lark"/"email"）在 notify 包全局注册表里只能有一个实现，
// notify.Register 对同 kind 会直接覆盖——这正是本类型存在的意义：用它顶替生产
// 环境的 LarkChannel/未来的 email 渠道，换取可断言、无外部依赖的发送记录。
type mockChannel struct {
	kind string

	mu   sync.Mutex
	sent []mockSend
}

// newMockChannel 构造一个 mock 渠道并立即注册进 notify 包的全局渠道表。
func newMockChannel(kind string) *mockChannel {
	c := &mockChannel{kind: kind}
	notify.Register(c)
	return c
}

func (c *mockChannel) Kind() string { return c.kind }

func (c *mockChannel) Send(ctx context.Context, target string, msg notify.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, mockSend{Target: target, Msg: msg})
	return nil
}

// Sent 返回当前已记录发送的副本快照，避免调用方持有的切片与后续 Send 并发追加冲突。
func (c *mockChannel) Sent() []mockSend {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]mockSend, len(c.sent))
	copy(out, c.sent)
	return out
}

// Reset 清空已记录的发送，供各场景在共享同一渠道实例时相互隔离。
func (c *mockChannel) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = nil
}
