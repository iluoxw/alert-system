package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// maxBodyBytes 是单次 webhook 请求体的大小上限，超出直接拒绝，避免超大 payload 占用
// 内存（任务 17 restrictions）。
const maxBodyBytes = 1 << 20 // 1MB

// ErrIntegrationNotFound 表示 integration 不存在。ID 格式非法（如非 UUID）时也归为
// 该错误，不单独区分——两者对调用方而言都应得到同样的 401，避免通过错误类型泄露
// 某个 ID 是否对应真实记录（R1.3：integration 不存在/已禁用/token 错误统一 401）。
var ErrIntegrationNotFound = errors.New("ingest: integration 不存在")

// Integration 是鉴权与路由所需的 integration 元信息，字段对应 integrations 表的子集列。
type Integration struct {
	ID         string
	Kind       string
	Enabled    bool
	HookIDHash string // hex(sha256(hook_id))，数据库不保存 URL 中的明文凭据
}

// Store 是 Server 依赖的持久化接口，只暴露用到的操作，方便测试用 fake 实现替换
// （无需 Docker/testcontainers）。生产实现见 PGStore。
type Store interface {
	// GetIntegrationByHookIDHash 通过 URL 中 opaque hook UUID 的 SHA-256 反查来源。
	GetIntegrationByHookIDHash(ctx context.Context, hookIDHash string) (Integration, error)
	// EnqueueBatch 在单个事务内把 events 全部写入事件队列：一次 webhook 的多条事件
	// 同一事务，要么全部提交要么全部回滚（R1.6：落库成功才能返回 202）。
	EnqueueBatch(ctx context.Context, integrationID string, events []domain.AlertEvent) error
}

// PGStore 是 Store 的生产实现，包装 *store.Store 与 *store.EventQueue。
type PGStore struct {
	st     *store.Store
	events *store.EventQueue
}

// NewPGStore 构造 PGStore。
func NewPGStore(st *store.Store) *PGStore {
	return &PGStore{st: st, events: store.NewEventQueue()}
}

func (p *PGStore) GetIntegrationByHookIDHash(ctx context.Context, hookIDHash string) (Integration, error) {
	var integ Integration
	err := p.st.Pool.QueryRow(ctx, `
		SELECT id, kind, enabled, hook_id_hash FROM integrations WHERE hook_id_hash = $1
	`, hookIDHash).Scan(&integ.ID, &integ.Kind, &integ.Enabled, &integ.HookIDHash)
	if err == nil {
		return integ, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Integration{}, ErrIntegrationNotFound
	}
	return Integration{}, fmt.Errorf("ingest: 按 hook 查询 integration 失败: %w", err)
}

func (p *PGStore) EnqueueBatch(ctx context.Context, integrationID string, events []domain.AlertEvent) error {
	return p.st.WithTx(ctx, func(tx pgx.Tx) error {
		for _, e := range events {
			if _, err := p.events.Enqueue(ctx, tx, integrationID, e); err != nil {
				return err
			}
		}
		if len(events) > 0 && domain.IsAuthoritativeSourceKind(events[0].Source) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO alert_source_status (integration_id, last_webhook_at)
				VALUES ($1::uuid, now())
				ON CONFLICT (integration_id) DO UPDATE SET last_webhook_at = EXCLUDED.last_webhook_at
			`, integrationID); err != nil {
				return fmt.Errorf("ingest: 更新 Alertmanager webhook 水位失败: %w", err)
			}
		}
		return nil
	})
}

// Server 只提供 managed hook：URL 内 UUID 同时完成来源识别与鉴权。
type Server struct {
	registry *Registry
	store    Store
	logger   *slog.Logger
}

// NewServer 构造 Server。logger 为 nil 时使用 slog.Default()。
func NewServer(registry *Registry, st Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{registry: registry, store: st, logger: logger}
}

// Handler 返回装配好路由的 http.Handler，用的是 Go 1.22+ ServeMux 的方法+路径模式
// （不引入 Gin，Gin 留给任务 25 的管理 API）。main 装配（后续任务）只需要
// http.ListenAndServe(addr, srv.Handler())。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/v1/hook/{hook_id}", s.handleManagedHook)
	return mux
}

func (s *Server) handleManagedHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hookID := r.PathValue("hook_id")
	if !validManagedHookID(hookID) {
		// hook UUID 是 bearer credential，非法与不存在必须保持同一响应，且日志不能记录它。
		s.reject(w, "", "hook 不存在", http.StatusUnauthorized)
		return
	}

	integ, err := s.store.GetIntegrationByHookIDHash(ctx, hashHookID(hookID))
	if err != nil {
		if errors.Is(err, ErrIntegrationNotFound) {
			s.reject(w, "", "hook 不存在", http.StatusUnauthorized)
			return
		}
		s.logger.Error("ingest: 按 managed hook 查询 integration 失败", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !integ.Enabled {
		s.reject(w, integ.ID, "integration 已禁用", http.StatusUnauthorized)
		return
	}
	s.handleIntegration(w, r, integ)
}

func (s *Server) handleIntegration(w http.ResponseWriter, r *http.Request, integ Integration) {
	ctx := r.Context()
	integrationID := integ.ID
	adapter, ok := s.registry.Get(integ.Kind)
	if !ok {
		s.logger.Error("ingest: 未注册 adapter", "integration_id", integrationID, "kind", integ.Kind)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.reject(w, integrationID, "body 超出大小限制或读取失败", http.StatusRequestEntityTooLarge)
		return
	}

	events, err := adapter.Parse(body)
	if err != nil {
		s.reject(w, integrationID, "payload 解析失败: "+err.Error(), http.StatusBadRequest, body)
		return
	}
	if len(events) == 0 {
		s.reject(w, integrationID, "payload 不含任何事件", http.StatusBadRequest, body)
		return
	}

	// 逐条校验先于入队：只要有一条不合法就整批拒绝（400），不做部分接受——
	// 与「WithTx 内全部 Enqueue」的落库语义保持一致（要么全成功，要么什么都不落）。
	for i := range events {
		if err := normalizeAndValidate(&events[i], integ.Kind); err != nil {
			s.reject(w, integrationID, fmt.Sprintf("第 %d 条事件校验失败: %v", i, err), http.StatusBadRequest, body)
			return
		}
	}

	if err := s.store.EnqueueBatch(ctx, integ.ID, events); err != nil {
		s.logger.Error("ingest: 写入事件队列失败", "integration_id", integrationID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// validManagedHookID 只接受 SRE Control 分发的规范小写 UUID v4。source 主键即使也是
// UUID，也无法通过 hook_id_hash 反查，不能被误当成接入凭据。
func validManagedHookID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '4' {
		return false
	}
	if !strings.ContainsRune("89ab", rune(id[19])) {
		return false
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", ch) {
			return false
		}
	}
	return true
}

// hashHookID 计算 hook_id 的 hex(sha256(hook_id))，与 integrations.hook_id_hash
// 的存储编码一致。
func hashHookID(hookID string) string {
	sum := sha256.Sum256([]byte(hookID))
	return hex.EncodeToString(sum[:])
}

// reject 写入统一的错误响应，并按 R1.4 记录结构化拒绝日志：只记 integration_id 与
// 原因，绝不记 token；body 只在与 payload 内容相关的拒绝路径上传入，且会被截断为摘要。
func (s *Server) reject(w http.ResponseWriter, integrationID, reason string, status int, body ...[]byte) {
	attrs := []any{"integration_id", integrationID, "reason", reason, "status", status}
	if len(body) > 0 && len(body[0]) > 0 {
		attrs = append(attrs, "body_snippet", truncateSnippet(body[0], 200))
	}
	s.logger.Warn("ingest: 拒绝请求", attrs...)
	http.Error(w, reason, status)
}

// truncateSnippet 把 body 截断为最多 n 字节的摘要，用于拒绝日志（不落全量 payload）。
func truncateSnippet(body []byte, n int) string {
	if len(body) <= n {
		return string(body)
	}
	return string(body[:n]) + "...(truncated)"
}

// normalizeAndValidate 校验 adapter 产出的单条信封是否满足入队前置条件，并补全
// Fingerprint（adapter 未提供时按 domain.Fingerprint 计算）与 Source（未提供时回退为
// integration 的 kind）。ResolvePolicy 是信封级必备字段（D4），必须合法，不做兜底默认；
// Severity 有专门的 SeverityFallback 语义（R2.5），因此这里只校验取值合法，不强制非空。
func normalizeAndValidate(e *domain.AlertEvent, integrationKind string) error {
	if e.RuleID == "" {
		return errors.New("rule_id 不能为空")
	}
	if e.Title == "" {
		return errors.New("title 不能为空")
	}
	if !e.Status.IsValid() {
		return fmt.Errorf("非法 status: %q", e.Status)
	}
	if e.Status == domain.StatusAcknowledged {
		return errors.New("上游事件不允许携带 acknowledged 状态")
	}
	if !e.Severity.IsValid() {
		return fmt.Errorf("非法 severity: %q", e.Severity)
	}
	if !e.ResolvePolicy.IsValid() {
		return fmt.Errorf("非法 resolve_policy: %q", e.ResolvePolicy)
	}
	if e.ResolvePolicy == domain.ResolvePolicyTTL && e.TTLSeconds <= 0 {
		return errors.New("resolve_policy=ttl 时 ttl_seconds 必须为正数")
	}
	if e.SignalKind != "" && !e.SignalKind.IsValid() {
		return fmt.Errorf("非法 signal_kind: %q", e.SignalKind)
	}
	if e.StartsAt.IsZero() {
		return errors.New("starts_at 不能为空")
	}

	if e.Source == "" {
		e.Source = integrationKind
	}
	if domain.IsAuthoritativeSourceKind(e.Source) && e.UpstreamFingerprint == "" {
		return fmt.Errorf("%s fingerprint 不能为空", e.Source)
	}
	if domain.IsAuthoritativeSourceKind(e.Source) {
		if e.SuppressionState == "" {
			e.SuppressionState = domain.SuppressionStateNone
		}
		if !e.SuppressionState.IsValid() {
			return fmt.Errorf("非法 suppression_state: %q", e.SuppressionState)
		}
		if e.SourceUpdatedAt.IsZero() {
			e.SourceUpdatedAt = e.StartsAt
		}
	}
	if e.Fingerprint == "" {
		e.Fingerprint = domain.Fingerprint(*e)
	}
	return nil
}
