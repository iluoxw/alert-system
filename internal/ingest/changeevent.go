package ingest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// changeEventMaxBodyBytes 与 maxBodyBytes（server.go）取同样的上限，理由一致：
// 变更事件 payload 也可能携带任意大小的 signal_context 式扩展字段。
const changeEventMaxBodyBytes = 1 << 20 // 1MB

// ChangeEventStore 是 ChangeEventHandler 依赖的持久化接口，只暴露写入操作，方便测试
// 用内存 fake 替换（无需 Docker/testcontainers）。生产实现见 ChangeEventPGStore。
type ChangeEventStore interface {
	Insert(ctx context.Context, event store.ChangeEvent) (*store.ChangeEvent, error)
}

// ChangeEventPGStore 是 ChangeEventStore 的生产实现，包装 *store.Store 与
// *store.ChangeEventRepo，把仓储方法要求的显式事务参数封装成单次调用。
type ChangeEventPGStore struct {
	st   *store.Store
	repo *store.ChangeEventRepo
}

// NewChangeEventPGStore 构造 ChangeEventPGStore。
func NewChangeEventPGStore(st *store.Store) *ChangeEventPGStore {
	return &ChangeEventPGStore{st: st, repo: store.NewChangeEventRepo()}
}

func (p *ChangeEventPGStore) Insert(ctx context.Context, event store.ChangeEvent) (*store.ChangeEvent, error) {
	var out *store.ChangeEvent
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		inserted, err := p.repo.Insert(ctx, tx, event)
		if err != nil {
			return err
		}
		out = inserted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// changeEventPayload 是变更事件 webhook 接受的 JSON 结构：本期只实现通用 webhook
// 一种来源（R7.4 允许"本期可仅实现通用 webhook 一种"），字段名与 store.ChangeEvent
// 的持久化字段一一对应。
type changeEventPayload struct {
	Kind        string            `json:"kind"`
	ResourceRef map[string]string `json:"resource_ref"`
	Payload     json.RawMessage   `json:"payload,omitempty"`
	OccurredAt  *time.Time        `json:"occurred_at,omitempty"`
}

// changeEventHandler 是 /change-events 端点的实现。与告警接入管道（Server）完全隔离：
// 变更事件不是 domain.AlertEvent、不经过 InboundAdapter/Registry、不进 alert_events
// 队列，鉴权通过后直接写 change_events 表（R7.4）。
//
// 鉴权采用单一静态 token（构造时注入，装配/配置留给任务 25），不复用告警 ingest 的
// per-integration token 机制——任务原文未要求按 integration 区分变更事件来源，引入
// 该维度对本期是过度设计。
type changeEventHandler struct {
	store  ChangeEventStore
	token  string
	logger *slog.Logger
}

// NewChangeEventHandler 构造变更事件 webhook 的 http.Handler。main 装配（任务 25）
// 只需要 mux.Handle("/change-events", ingest.NewChangeEventHandler(...))；具体挂载
// 路径由调用方决定，这里不内置路由。
//
// token 为空字符串会导致所有请求都被拒绝（视为"未配置"而非"无需鉴权"），避免上线时
// 忘记配置 token 却被误当成公开端点。logger 为 nil 时使用 slog.Default()。
func NewChangeEventHandler(st ChangeEventStore, token string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &changeEventHandler{store: st, token: token, logger: logger}
}

func (h *changeEventHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.authorized(r) {
		h.reject(w, "鉴权失败", http.StatusUnauthorized, nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, changeEventMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.reject(w, "body 超出大小限制或读取失败", http.StatusRequestEntityTooLarge, nil)
		return
	}

	event, err := parseChangeEvent(body)
	if err != nil {
		h.reject(w, "payload 解析失败: "+err.Error(), http.StatusBadRequest, body)
		return
	}

	if _, err := h.store.Insert(r.Context(), event); err != nil {
		h.logger.Error("change-event: 写入失败", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// authorized 校验 Authorization: Bearer / X-Ingest-Token 头（extractToken 定义于
// server.go，同包复用）与构造时注入的静态 token 是否一致，常量时间比较避免时序侧信道，
// 约定与 validToken（server.go）一致。
func (h *changeEventHandler) authorized(r *http.Request) bool {
	if h.token == "" {
		return false
	}
	token := extractToken(r)
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

// reject 写入统一错误响应并记录结构化拒绝日志（对齐 server.go 的 reject：不落 token，
// body 只在与 payload 内容相关的拒绝路径上传入且被截断）。
func (h *changeEventHandler) reject(w http.ResponseWriter, reason string, status int, body []byte) {
	attrs := []any{"reason", reason, "status", status}
	if len(body) > 0 {
		attrs = append(attrs, "body_snippet", truncateSnippet(body, 200))
	}
	h.logger.Warn("change-event: 拒绝请求", attrs...)
	http.Error(w, reason, status)
}

// parseChangeEvent 解析请求体并翻译为 store.ChangeEvent：校验 kind/resource_ref 非空，
// resource_ref 键名按 OTel semantic conventions 规范化（复用 alertmanager.go 的
// normalizeResourceRef），occurred_at 缺省取当前时间。
//
// normalizeResourceRef 的既有约定是"只保留能映射到 OTel 键名的原始 label"（未命中的
// key 会被丢弃，与 AlertmanagerAdapter 的行为一致）。若上游已经直接使用 OTel 键名
// 投递（如 "service.name"），规范化会因命中不到任何别名而返回空——为避免这种情况下
// 把调用方已经规范化好的数据整体丢空，规范化结果为空时回退为原始 resource_ref。
func parseChangeEvent(body []byte) (store.ChangeEvent, error) {
	var p changeEventPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return store.ChangeEvent{}, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	if p.Kind == "" {
		return store.ChangeEvent{}, errors.New("kind 不能为空")
	}
	if len(p.ResourceRef) == 0 {
		return store.ChangeEvent{}, errors.New("resource_ref 不能为空")
	}

	resourceRef := normalizeResourceRef(p.ResourceRef)
	if len(resourceRef) == 0 {
		resourceRef = p.ResourceRef
	}

	occurredAt := time.Now()
	if p.OccurredAt != nil {
		occurredAt = *p.OccurredAt
	}

	payload := p.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}

	return store.ChangeEvent{
		Kind:        p.Kind,
		ResourceRef: resourceRef,
		Payload:     payload,
		OccurredAt:  occurredAt,
	}, nil
}
