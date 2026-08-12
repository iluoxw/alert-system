// Package api 提供对外的回调/管理类 HTTP 端点（本任务只实现飞书卡片回调，main 装配留给
// 任务 25）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	larkcard "github.com/larksuite/oapi-sdk-go/v3/card"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"

	"alert-system/internal/domain"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/store"
)

// maxCallbackBodyBytes 是单次飞书回调请求体的大小上限，与 ingest 端点同量级（1MB 足够
// 容纳卡片回调 payload，防止异常大 body 占用内存）。
const maxCallbackBodyBytes = 1 << 20

// AlertStore 抽象 store.AlertRepo 用到的两个方法，让 Server 的回调编排逻辑可以在单测里
// 注入内存 fake，不需要真实 Postgres。*store.AlertRepo 天然满足这个接口。
type AlertStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error
}

// TimelineStore 抽象 store.TimelineRepo.Append。
type TimelineStore interface {
	Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error)
}

// TxRunner 抽象 store.Store.WithTx。
type TxRunner interface {
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// LarkUserStore 把飞书 open_id 映射到 Firebreak 用户。生产环境接入后，卡片动作
// 必须使用映射后的 users.id 写入领域字段；未映射用户不能执行处置动作。
type LarkUserStore interface {
	GetByLarkOpenID(ctx context.Context, tx pgx.Tx, openID string) (*store.User, error)
}

// LarkCardActionStore 用 request_id 对飞书卡片动作做幂等占位。它只记录动作审计，
// 领域状态仍由各自仓储和状态机负责。
type LarkCardActionStore interface {
	Insert(ctx context.Context, tx pgx.Tx, action store.LarkCardAction) (bool, error)
	UpdateResult(ctx context.Context, tx pgx.Tx, requestID string, result json.RawMessage) error
}

type LarkFaultStore interface {
	GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Fault, error)
	UpdateLifecycleStatus(ctx context.Context, tx pgx.Tx, id string, from, to domain.FaultStatus, at time.Time) (domain.Fault, error)
}

type LarkIncidentTransitioner interface {
	TransitionInTx(ctx context.Context, tx pgx.Tx, incidentID string, to domain.IncidentStatus, actor incidentservice.Actor) (domain.Incident, error)
}

// Server 是飞书卡片回调端点的 HTTP 处理器集合（R6.2/R6.4/R9.5 主力渠道的交互闭环）。
type Server struct {
	alerts   AlertStore
	timeline TimelineStore
	tx       TxRunner
	logger   *slog.Logger

	verificationToken string
	cardHandler       *larkcard.CardActionHandler

	escalation EscalationStopper
	lifecycle  LifecycleTimerCanceler
	larkUsers  LarkUserStore
	actions    LarkCardActionStore
	faults     LarkFaultStore
	incidents  LarkIncidentTransitioner
}

// NewServer 构造 Server。verificationToken/encryptKey 来自 internal/config.LarkConfig；
// 传空字符串表示不启用对应校验（与 SDK 自身约定一致，仅用于本地开发/测试）。logger 为 nil
// 时使用 slog.Default()。escalation/lifecycle 复用 alerts.go 里 REST Ack/Resolve 端点
// 同款的 EscalationStopper/LifecycleTimerCanceler 接口，让卡片回调与 REST 端点共享同一套
// "认领/解决后停止升级与生命周期定时器"不变量；传 nil 表示跳过。
func NewServer(alerts AlertStore, timeline TimelineStore, tx TxRunner, verificationToken, encryptKey string, logger *slog.Logger, escalation EscalationStopper, lifecycle LifecycleTimerCanceler) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		alerts:            alerts,
		timeline:          timeline,
		tx:                tx,
		logger:            logger,
		verificationToken: verificationToken,
		escalation:        escalation,
		lifecycle:         lifecycle,
	}
	s.cardHandler = larkcard.NewCardActionHandler(verificationToken, encryptKey, s.handleCardAction)
	return s
}

// SetLarkIdentityStore 接入飞书身份映射。保留独立 setter 是为了兼容已有本地
// 回调测试和旧装配方；生产装配必须设置该依赖。
func (s *Server) SetLarkIdentityStore(users LarkUserStore) *Server {
	s.larkUsers = users
	return s
}

// SetLarkActionStore 接入卡片动作幂等审计仓储。未设置时保留旧回调语义，仅适合
// 兼容测试，不应作为生产配置。
func (s *Server) SetLarkActionStore(actions LarkCardActionStore) *Server {
	s.actions = actions
	return s
}

func (s *Server) SetLarkFaultStore(faults LarkFaultStore) *Server {
	s.faults = faults
	return s
}

func (s *Server) SetLarkIncidentTransitioner(incidents LarkIncidentTransitioner) *Server {
	s.incidents = incidents
	return s
}

// Handler 返回装配好路由的 http.Handler。main 装配（后续任务）只需要
// http.ListenAndServe(addr, srv.Handler())。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /callbacks/lark/card", s.handleCardCallback)
	return mux
}

// handleCardCallback 是飞书卡片回调的 HTTP 入口。验签规则：
//   - url_verification 握手请求不带签名头，飞书官方约定改为核对 body 里的 verification
//     token（SDK 内部 AuthByChallenge 走的也是同一判断），此处提前做同样的核对；
//   - 其余回调（event_callback / 卡片 action）用 CardActionHandler.VerifySign 做
//     HMAC-SHA1 签名校验。
//
// 两种校验只要判定失败，一律直接返回 401，不使用 oapi-sdk-go 默认的 500
// （CardActionHandler.Handle 在 VerifySign 失败时内部走 processError，返回的是 500；
// 本任务要求"验签失败一律 401"，因此这里自己短路掉该分支，成功后才移交给 Handle 做
// 正常的 challenge/业务分发）。
//
// 已知局限：如果飞书控制台为该应用配置了 encrypt_key，回调 body 会整体加密成
// {"encrypt": "..."}，下面这个提前对 body 做的轻量 JSON 探测拿不到明文 type/token
// 字段，会被当作普通 event_callback 处理，即要求带签名头验签——多数从简部署不启用
// encrypt_key，这里判定为低概率场景，先不处理，留给后续任务按需支持解密后再探测。
func (s *Server) handleCardCallback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCallbackBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body 超出大小限制或读取失败", http.StatusRequestEntityTooLarge)
		return
	}

	eventReq := &larkevent.EventReq{
		Header:     map[string][]string(r.Header.Clone()),
		Body:       body,
		RequestURI: r.URL.Path,
	}

	var probe struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &probe) // 探测失败（如加密 body）按非 challenge 处理，走下面的验签分支

	if larkevent.ReqType(probe.Type) != larkevent.ReqTypeChallenge {
		if err := s.cardHandler.VerifySign(r.Context(), eventReq); err != nil {
			s.logger.Warn("lark_callback: 验签失败")
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
	} else if s.verificationToken != "" && probe.Token != s.verificationToken {
		s.logger.Warn("lark_callback: url_verification token 不匹配")
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	resp := s.cardHandler.Handle(r.Context(), eventReq)
	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
}

// handleCardAction 是验签通过后的业务分发：从卡片按钮的 value 里取统一资源动作，
// 调状态机（domain.Ack/Resolve），并在同一事务内落库状态与时间线（R4.6：append-only）。
// 返回值会被 SDK 序列化为回调响应体（非 nil 且非 string/*CustomResp 时走 json.Marshal），
// 这里返回固定的成功字符串，不回显任何内部错误细节。
func (s *Server) handleCardAction(ctx context.Context, action *larkcard.CardAction) (interface{}, error) {
	if action.Action == nil {
		return nil, fmt.Errorf("lark_callback: 回调缺少 action 字段")
	}
	value := action.Action.Value
	resourceType, _ := value["resource_type"].(string)
	resourceID, _ := value["resource_id"].(string)
	// 兼容旧版告警卡片；新卡片统一使用 resource_type/resource_id。
	if resourceID == "" {
		resourceID, _ = value["alert_id"].(string)
	}
	if resourceType == "" && resourceID != "" {
		resourceType = "alert"
	}
	actionName, _ := value["action"].(string)
	requestID, _ := value["request_id"].(string)
	if resourceType == "" || resourceID == "" || actionName == "" {
		return nil, fmt.Errorf("lark_callback: action value 缺少 resource_type/resource_id/action")
	}

	actor := action.OpenID
	if actor == "" {
		actor = action.UserID
	}
	if actor == "" {
		actor = "lark:unknown"
	}

	now := time.Now()
	err := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		actorID := ""
		actorDisplay := actor
		if s.larkUsers != nil {
			user, lookupErr := s.larkUsers.GetByLarkOpenID(ctx, tx, actor)
			if lookupErr != nil {
				return fmt.Errorf("lark_callback: 飞书用户未绑定 Firebreak 账号: %w", lookupErr)
			}
			if user == nil || !user.Enabled {
				return fmt.Errorf("lark_callback: 飞书用户未绑定或已禁用")
			}
			actorID = user.ID
			if user.DisplayName != "" {
				actorDisplay = user.DisplayName
			} else if user.Username != "" {
				actorDisplay = user.Username
			}
		}
		if s.actions != nil {
			if requestID == "" {
				if action.OpenMessageID == "" {
					return fmt.Errorf("lark_callback: 卡片动作缺少 request_id/open_message_id")
				}
				requestID = fmt.Sprintf("legacy:%s:%s:%s:%s", action.OpenMessageID, resourceType, resourceID, actionName)
			}
			created, insertErr := s.actions.Insert(ctx, tx, store.LarkCardAction{
				RequestID: requestID, ResourceType: resourceType, ResourceID: resourceID,
				Action: actionName, OperatorLarkID: actor, OperatorUserID: actorID,
				Payload: mustJSON(value),
			})
			if insertErr != nil {
				return insertErr
			}
			if !created {
				return nil
			}
		}
		var result map[string]any
		var actionErr error
		switch resourceType {
		case "alert":
			result, actionErr = s.handleAlertAction(ctx, tx, resourceID, actionName, actorID, actorDisplay, now)
		case "fault":
			result, actionErr = s.handleFaultAction(ctx, tx, resourceID, actionName, now)
		case "incident":
			result, actionErr = s.handleIncidentAction(ctx, tx, resourceID, actionName, actorID, actorDisplay)
		default:
			return fmt.Errorf("lark_callback: 未知 resource_type %q", resourceType)
		}
		if actionErr != nil {
			return actionErr
		}
		if s.actions != nil {
			if err := s.actions.UpdateResult(ctx, tx, requestID, mustJSON(result)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.logger.Error("lark_callback: 处理卡片操作失败", "resource_type", resourceType, "resource_id", resourceID, "action", actionName, "err", err)
		return nil, err
	}
	return &larkcard.CustomResp{Body: map[string]interface{}{
		"toast": map[string]interface{}{"type": "success", "content": "操作已生效"},
	}}, nil
}

func (s *Server) handleAlertAction(ctx context.Context, tx pgx.Tx, alertID, actionName, actorID, actorDisplay string, now time.Time) (map[string]any, error) {
	alert, err := s.alerts.GetByID(ctx, tx, alertID)
	if err != nil {
		return nil, err
	}
	var updated domain.Alert
	var entry domain.TimelineEntry
	switch actionName {
	case "ack":
		updated, entry, err = domain.Ack(alert, actorID, actorDisplay, now)
	case "resolve":
		updated, entry, err = domain.Resolve(alert, actorDisplay, domain.ResolveReasonManual, "", now)
	default:
		return nil, fmt.Errorf("lark_callback: Alert 未知 action %q", actionName)
	}
	if err != nil {
		return nil, err
	}
	if err := s.alerts.UpdateStatus(ctx, tx, updated); err != nil {
		return nil, err
	}
	if _, err := s.timeline.Append(ctx, tx, entry); err != nil {
		return nil, err
	}
	if s.escalation != nil {
		if err := s.escalation.Stop(ctx, tx, updated.ID); err != nil {
			return nil, err
		}
	}
	if actionName == "resolve" && s.lifecycle != nil {
		if err := s.lifecycle.CancelLifecycleTimers(ctx, tx, updated.ID); err != nil {
			return nil, err
		}
	}
	return map[string]any{"status": updated.Status, "action": actionName}, nil
}

func (s *Server) handleFaultAction(ctx context.Context, tx pgx.Tx, faultID, actionName string, now time.Time) (map[string]any, error) {
	if s.faults == nil {
		return nil, fmt.Errorf("lark_callback: 未配置 Fault 服务")
	}
	if actionName != "advance" {
		return nil, fmt.Errorf("lark_callback: Fault 未知 action %q", actionName)
	}
	fault, err := s.faults.GetByID(ctx, tx, faultID)
	if err != nil {
		return nil, err
	}
	var to domain.FaultStatus
	switch fault.Status {
	case domain.FaultStatusCandidate:
		to = domain.FaultStatusActive
	case domain.FaultStatusActive:
		to = domain.FaultStatusMonitoring
	case domain.FaultStatusMonitoring:
		to = domain.FaultStatusResolved
	default:
		return nil, fmt.Errorf("lark_callback: Fault 当前状态 %q 不支持推进", fault.Status)
	}
	_, err = s.faults.UpdateLifecycleStatus(ctx, tx, fault.ID, fault.Status, to, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status_from": fault.Status, "status_to": to, "action": actionName}, nil
}

func (s *Server) handleIncidentAction(ctx context.Context, tx pgx.Tx, incidentID, actionName, actorID, actorDisplay string) (map[string]any, error) {
	if s.incidents == nil {
		return nil, fmt.Errorf("lark_callback: 未配置 Incident 服务")
	}
	var to domain.IncidentStatus
	switch actionName {
	case "mitigate":
		to = domain.IncidentStatusMitigating
	default:
		return nil, fmt.Errorf("lark_callback: Incident 未知 action %q", actionName)
	}
	incident, err := s.incidents.TransitionInTx(ctx, tx, incidentID, to, incidentservice.Actor{UserID: actorID, Display: actorDisplay})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": incident.Status, "action": actionName}, nil
}

func mustJSON(value map[string]interface{}) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}
