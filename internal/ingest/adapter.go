package ingest

import (
	"sync"

	"alert-system/internal/domain"
)

// InboundAdapter 是接入层的适配器契约：把某个来源（vendor webhook 或通用格式）的原始
// 请求体翻译为统一告警信封。一次 webhook 请求可能携带多条告警，Parse 因此返回切片。
//
// Parse 只负责格式翻译，不做鉴权、不做落库；Fingerprint 未设置时由 server 统一补全
// （见 normalizeAndValidate），adapter 也无需自行计算。任务 18/19 的 vendor adapter
// （Alertmanager/Grafana 等）都实现该接口，Kind() 的返回值与 integrations 表的 kind
// 列一一对应，决定某个 integration 的请求交给哪个 adapter 处理。
type InboundAdapter interface {
	// Kind 返回该 adapter 的注册名（如 "generic"、"alertmanager"、"grafana"）。
	Kind() string
	// Parse 把一次 webhook 请求体翻译为一条或多条统一告警信封。
	Parse(body []byte) ([]domain.AlertEvent, error)
}

// Registry 是 InboundAdapter 的注册表，按 Kind() 查找。Register 通常只在启动装配阶段
// 调用一次，但 Get 会被并发的请求处理 goroutine 频繁调用，因此用 RWMutex 保护，
// 与 scheduler.Scheduler 的 handlers map 采用同样的读写锁模式。
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]InboundAdapter
}

// NewRegistry 构造一个空 Registry。
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]InboundAdapter)}
}

// Register 注册一个 adapter，重复注册同一个 Kind() 会覆盖旧的。
func (r *Registry) Register(a InboundAdapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Kind()] = a
}

// Get 按 kind 查找已注册的 adapter；未注册返回 (nil, false)。
func (r *Registry) Get(kind string) (InboundAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[kind]
	return a, ok
}
