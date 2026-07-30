package notify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Message 是渠道无关的通知载荷：策略引擎（任务 22）按通知策略算出要发什么之后，
// 组装出一个 Message 交给某个 Channel 去渲染成自己的格式（webhook JSON、飞书卡片……）。
// 核心管道以外的渠道实现只依赖这个结构体，不感知告警/分组的内部模型。
type Message struct {
	AlertIDs   []string          // 本次通知覆盖的告警 ID；单告警通知长度为 1，分组通知为组内全部告警的 ID
	GroupKey   string            // 分组键；单告警通知（未分组）时为空
	Severity   string            // domain.Severity 的字符串值，冒泡给渠道用于展示（如卡片配色、emoji）
	Title      string            // 摘要标题：单告警为告警标题，分组为"N 条 xxx 级告警"一类摘要
	Body       string            // 正文/摘要内容
	DetailURL  string            // 详情跳转链接（告警详情页/分组详情页）
	Labels     map[string]string // 公共标签（分组场景下为 group_by 收敛后的公共标签集）
	OccurredAt time.Time         // 触发/最近一次事件发生时间
}

// Channel 是一个可发送通知的出站渠道（webhook/飞书/短信/电话……）。
// Send 只负责把 msg 老老实实发到 target，出错返回 error；幂等去重与重试编排是
// Sender（sender.go）的职责，Channel 实现不需要关心这些。
type Channel interface {
	// Kind 返回渠道类型标识（如 "webhook"），对应 notification_policies.steps 里的 channel 名。
	Kind() string
	// Send 把 msg 发送到 target（含义随渠道而定：webhook 是 URL，飞书是群/用户 open_id……）。
	Send(ctx context.Context, target string, msg Message) error
}

// ErrChannelNotRegistered 在 Get/Validate 遇到未注册的渠道类型时返回。
var ErrChannelNotRegistered = errors.New("notify: 渠道未注册")

// registry 是进程内渠道注册表：由启动时的 wiring 代码 Register 具体实现，
// Sender 与策略引擎（任务 22）按 kind 通过 Get 取用，两者都不需要感知具体渠道类型。
var (
	registryMu sync.RWMutex
	registry   = map[string]Channel{}
)

// Register 注册一个渠道实现；kind 相同会覆盖之前的注册（便于测试用 fake 替换）。
func Register(c Channel) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[c.Kind()] = c
}

// Get 按 kind 取渠道实现；未注册时返回 ErrChannelNotRegistered。
func Get(kind string) (Channel, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	c, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrChannelNotRegistered, kind)
	}
	return c, nil
}

// Validate 校验 kinds 里的每个渠道类型都已注册（R13.6：策略引用了未启用的渠道时，
// 在配置校验阶段就报错，而不是等到实际发送才失败）。电话渠道本期只在注册表/Validate
// 这一层留位、不提供 Channel 实现——策略里配置了 "phone" 会像对待任何未注册渠道一样
// 被这里拦下来，供任务 22/27 的配置校验复用。
func Validate(kinds []string) error {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var missing []string
	for _, k := range kinds {
		if _, ok := registry[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %v", ErrChannelNotRegistered, missing)
	}
	return nil
}
