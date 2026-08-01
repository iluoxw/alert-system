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

// handleCardAction 是验签通过后的业务分发：从卡片按钮的 value 里取 alert_id/action，
// 调状态机（domain.Ack/Resolve），并在同一事务内落库状态与时间线（R4.6：append-only）。
// 返回值会被 SDK 序列化为回调响应体（非 nil 且非 string/*CustomResp 时走 json.Marshal），
// 这里返回固定的成功字符串，不回显任何内部错误细节。
func (s *Server) handleCardAction(ctx context.Context, action *larkcard.CardAction) (interface{}, error) {
	if action.Action == nil {
		return nil, fmt.Errorf("lark_callback: 回调缺少 action 字段")
	}
	alertID, _ := action.Action.Value["alert_id"].(string)
	actionName, _ := action.Action.Value["action"].(string)
	if alertID == "" || actionName == "" {
		return nil, fmt.Errorf("lark_callback: action value 缺少 alert_id/action")
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
		alert, err := s.alerts.GetByID(ctx, tx, alertID)
		if err != nil {
			return err
		}

		var updated domain.Alert
		var entry domain.TimelineEntry
		switch actionName {
		case "ack":
			// actorID 传空字符串：本包目前没有"飞书身份 -> 内部 users 记录"的映射能力，
			// alerts.acked_by 是 UUID 列，无法映射时只能落 NULL（domain.Ack 对空
			// actorID 的处理约定）；actor（OpenID/UserID）仍完整写入 timeline 展示。
			updated, entry, err = domain.Ack(alert, "", actor, now)
		case "resolve":
			updated, entry, err = domain.Resolve(alert, actor, domain.ResolveReasonManual, "", now)
		default:
			return fmt.Errorf("lark_callback: 未知 action %q", actionName)
		}
		if err != nil {
			return err
		}

		if err := s.alerts.UpdateStatus(ctx, tx, updated); err != nil {
			return err
		}
		if _, err := s.timeline.Append(ctx, tx, entry); err != nil {
			return err
		}

		// 与 alerts.go 的 REST ack/resolve 端点同一不变量：状态机成功转移后，无条件尝试
		// 停止升级链/生命周期定时器（两者均幂等），不依赖"本次是否真的发生了状态转移"
		// 的历史。actionName 走到这里必为 "ack"/"resolve" 之一（其余分支已在上面的
		// switch 里提前返回错误）。
		if s.escalation != nil {
			if err := s.escalation.Stop(ctx, tx, updated.ID); err != nil {
				return err
			}
		}
		if actionName == "resolve" && s.lifecycle != nil {
			if err := s.lifecycle.CancelLifecycleTimers(ctx, tx, updated.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.logger.Error("lark_callback: 处理卡片操作失败", "alert_id", alertID, "action", actionName, "err", err)
		return nil, err
	}
	return "success", nil
}
