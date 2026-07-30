package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	larkcard "github.com/larksuite/oapi-sdk-go/v3/card"

	"alert-system/internal/domain"
)

// fakeAlertStore 是 AlertStore 的内存 fake，用 map 模拟告警仓储，不依赖真实 Postgres。
type fakeAlertStore struct {
	alerts map[string]domain.Alert
}

func (f *fakeAlertStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error) {
	a, ok := f.alerts[id]
	if !ok {
		return domain.Alert{}, errAlertNotFoundForTest
	}
	return a, nil
}

func (f *fakeAlertStore) UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	f.alerts[alert.ID] = alert
	return nil
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errAlertNotFoundForTest = fakeErr("alert not found")

// fakeTimelineStore 是 TimelineStore 的内存 fake，记录每次 Append 调用供断言。
type fakeTimelineStore struct {
	entries []domain.TimelineEntry
}

func (f *fakeTimelineStore) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	f.entries = append(f.entries, entry)
	return "tl-1", nil
}

// fakeTxRunner 直接执行 fn，不做真实事务——单测只关心业务编排是否按序调用，不验证
// 事务边界本身（事务边界由 store.Store.WithTx 自己的测试覆盖）。
type fakeTxRunner struct{}

func (fakeTxRunner) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return fn(nil)
}

func challengeBody(token, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":      "url_verification",
		"token":     token,
		"challenge": challenge,
	})
	return b
}

func cardActionBody(openID, alertID, actionName string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "card_action_trigger",
		"open_id": openID,
		"action": map[string]any{
			"value": map[string]any{
				"alert_id": alertID,
				"action":   actionName,
			},
		},
	})
	return b
}

// signedRequest 构造带正确 HMAC-SHA1 签名头的回调请求（复用 SDK 导出的 Signature 函数，
// 保证测试算的签名和 Server.handleCardCallback 内部调用的 CardActionHandler.VerifySign
// 用的是同一算法）。
func signedRequest(token string, body []byte) *http.Request {
	const ts, nonce = "1700000000", "test-nonce"
	sig := larkcard.Signature(ts, nonce, token, string(body))
	req := httptest.NewRequest(http.MethodPost, "/callbacks/lark/card", bytes.NewReader(body))
	req.Header.Set("X-Lark-Request-Timestamp", ts)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", sig)
	return req
}

func unsignedRequest(body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/callbacks/lark/card", bytes.NewReader(body))
}

func newTestAlert(id string, status domain.Status) domain.Alert {
	return domain.Alert{ID: id, Status: status, Title: "test alert"}
}

// 场景：url_verification 握手——不带签名头，靠 body 里的 token 与配置核对，匹配则原样
// 回显 challenge，状态码 200。
func TestHandleCardCallback_URLVerification_Success(t *testing.T) {
	srv := NewServer(&fakeAlertStore{alerts: map[string]domain.Alert{}}, &fakeTimelineStore{}, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	req := unsignedRequest(challengeBody("verify-tok", "chal-123"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, w.Body.String())
	}
	if resp.Challenge != "chal-123" {
		t.Errorf("challenge = %q, want chal-123", resp.Challenge)
	}
}

// 场景：url_verification 请求携带的 token 与配置不匹配——按验签失败处理，返回 401
// （不使用 SDK 默认的 500）。
func TestHandleCardCallback_URLVerification_TokenMismatch_Returns401(t *testing.T) {
	srv := NewServer(&fakeAlertStore{alerts: map[string]domain.Alert{}}, &fakeTimelineStore{}, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	req := unsignedRequest(challengeBody("wrong-tok", "chal-123"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// 场景：普通卡片回调缺少/带错签名——401，且不触发任何状态机调用。
func TestHandleCardCallback_InvalidSignature_Returns401(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-1": newTestAlert("alert-1", domain.StatusFiring)}}
	timeline := &fakeTimelineStore{}
	srv := NewServer(alerts, timeline, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	body := cardActionBody("ou_test_user", "alert-1", "ack")
	req := unsignedRequest(body) // 不带任何签名头
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if alerts.alerts["alert-1"].Status != domain.StatusFiring {
		t.Errorf("验签失败不应触发状态机调用，实际状态变为 %v", alerts.alerts["alert-1"].Status)
	}
	if len(timeline.entries) != 0 {
		t.Errorf("验签失败不应写入时间线，实际写入 %d 条", len(timeline.entries))
	}
}

// 场景：验签通过的 Ack 回调 → 状态机调用（firing -> acknowledged）→ 状态更新 + 时间线落账。
func TestHandleCardCallback_ValidSignature_AckCallback(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-1": newTestAlert("alert-1", domain.StatusFiring)}}
	timeline := &fakeTimelineStore{}
	srv := NewServer(alerts, timeline, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	body := cardActionBody("ou_test_user", "alert-1", "ack")
	req := signedRequest("verify-tok", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	updated := alerts.alerts["alert-1"]
	if updated.Status != domain.StatusAcknowledged {
		t.Errorf("Status = %v, want acknowledged", updated.Status)
	}
	// 卡片回调没有"飞书身份 -> 内部 users 记录"的映射能力，AckedBy（UUID 列）留空，
	// 操作者标识只进 timeline 的 Actor（TEXT 列）。
	if updated.AckedBy != "" {
		t.Errorf("AckedBy = %q, want 空字符串（无法映射到内部用户）", updated.AckedBy)
	}
	if len(timeline.entries) != 1 || timeline.entries[0].Action != domain.TimelineActionAck {
		t.Fatalf("期望落 1 条 ack 时间线，实际: %+v", timeline.entries)
	}
	if timeline.entries[0].Actor != "ou_test_user" {
		t.Errorf("timeline actor = %q, want ou_test_user", timeline.entries[0].Actor)
	}
}

// 场景：验签通过的 Resolve 回调 → 状态机调用（firing -> resolved）→ 状态更新 + 时间线落账。
func TestHandleCardCallback_ValidSignature_ResolveCallback(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-2": newTestAlert("alert-2", domain.StatusFiring)}}
	timeline := &fakeTimelineStore{}
	srv := NewServer(alerts, timeline, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	body := cardActionBody("ou_test_user", "alert-2", "resolve")
	req := signedRequest("verify-tok", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	updated := alerts.alerts["alert-2"]
	if updated.Status != domain.StatusResolved {
		t.Errorf("Status = %v, want resolved", updated.Status)
	}
	if len(timeline.entries) != 1 || timeline.entries[0].Action != domain.TimelineActionResolve {
		t.Fatalf("期望落 1 条 resolve 时间线，实际: %+v", timeline.entries)
	}
	if timeline.entries[0].Detail["reason"] != string(domain.ResolveReasonManual) {
		t.Errorf("reason = %v, want manual（卡片按钮触发视为人工解决）", timeline.entries[0].Detail["reason"])
	}
}

// 场景：验签通过但告警不存在——业务处理失败，不使用 401（那是验签专用状态码），
// 落到 SDK 默认的 500。
func TestHandleCardCallback_ValidSignature_AlertNotFound_Returns500(t *testing.T) {
	srv := NewServer(&fakeAlertStore{alerts: map[string]domain.Alert{}}, &fakeTimelineStore{}, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	body := cardActionBody("ou_test_user", "does-not-exist", "ack")
	req := signedRequest("verify-tok", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// 场景：verificationToken 为空（未配置）——不启用验签，任何 body 都直接放行到业务处理
// （与 SDK VerifySign 自身的 no-op-when-empty 约定一致）。
func TestHandleCardCallback_NoVerificationTokenConfigured_SkipsSignatureCheck(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-3": newTestAlert("alert-3", domain.StatusFiring)}}
	srv := NewServer(alerts, &fakeTimelineStore{}, fakeTxRunner{}, "", "", nil, nil, nil)

	body := cardActionBody("ou_test_user", "alert-3", "ack")
	req := unsignedRequest(body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if alerts.alerts["alert-3"].Status != domain.StatusAcknowledged {
		t.Errorf("Status = %v, want acknowledged", alerts.alerts["alert-3"].Status)
	}
}

// fakeEscalationStopper 是 EscalationStopper 的测试替身，记录每次 Stop 调用的 alertID。
type fakeEscalationStopper struct {
	calls []string
}

func (f *fakeEscalationStopper) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	f.calls = append(f.calls, alertID)
	return nil
}

// fakeLifecycleTimerCanceler 是 LifecycleTimerCanceler 的测试替身，记录每次
// CancelLifecycleTimers 调用的 alertID。
type fakeLifecycleTimerCanceler struct {
	calls []string
}

func (f *fakeLifecycleTimerCanceler) CancelLifecycleTimers(ctx context.Context, tx pgx.Tx, alertID string) error {
	f.calls = append(f.calls, alertID)
	return nil
}

// 场景：卡片 Ack 回调成功转移状态后，应无条件调用 EscalationStopper.Stop（不涉及
// LifecycleTimerCanceler——TTL/repeat 等定时器只在告警进入终态 resolved 时才需要取消）。
func TestHandleCardCallback_AckCallback_StopsEscalation(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-4": newTestAlert("alert-4", domain.StatusFiring)}}
	escalation := &fakeEscalationStopper{}
	lifecycle := &fakeLifecycleTimerCanceler{}
	srv := NewServer(alerts, &fakeTimelineStore{}, fakeTxRunner{}, "verify-tok", "", nil, escalation, lifecycle)

	body := cardActionBody("ou_test_user", "alert-4", "ack")
	req := signedRequest("verify-tok", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(escalation.calls) != 1 || escalation.calls[0] != "alert-4" {
		t.Fatalf("EscalationStopper.Stop 调用 = %+v, want [alert-4]", escalation.calls)
	}
	if len(lifecycle.calls) != 0 {
		t.Fatalf("Ack 不应调用 LifecycleTimerCanceler，实际调用 %+v", lifecycle.calls)
	}
}

// 场景：卡片 Resolve 回调成功转移状态后，应无条件调用 EscalationStopper.Stop 与
// LifecycleTimerCanceler.CancelLifecycleTimers（与 alerts.go 的 REST resolve 端点同一
// 不变量）。
func TestHandleCardCallback_ResolveCallback_StopsEscalationAndCancelsLifecycle(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-5": newTestAlert("alert-5", domain.StatusFiring)}}
	escalation := &fakeEscalationStopper{}
	lifecycle := &fakeLifecycleTimerCanceler{}
	srv := NewServer(alerts, &fakeTimelineStore{}, fakeTxRunner{}, "verify-tok", "", nil, escalation, lifecycle)

	body := cardActionBody("ou_test_user", "alert-5", "resolve")
	req := signedRequest("verify-tok", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(escalation.calls) != 1 || escalation.calls[0] != "alert-5" {
		t.Fatalf("EscalationStopper.Stop 调用 = %+v, want [alert-5]", escalation.calls)
	}
	if len(lifecycle.calls) != 1 || lifecycle.calls[0] != "alert-5" {
		t.Fatalf("LifecycleTimerCanceler.CancelLifecycleTimers 调用 = %+v, want [alert-5]", lifecycle.calls)
	}
}

// 场景：escalation/lifecycle 均为 nil（未接线的调用方）——原有行为不变，不 panic。
func TestHandleCardCallback_NilEscalationAndLifecycle_NoPanic(t *testing.T) {
	alerts := &fakeAlertStore{alerts: map[string]domain.Alert{"alert-6": newTestAlert("alert-6", domain.StatusFiring)}}
	srv := NewServer(alerts, &fakeTimelineStore{}, fakeTxRunner{}, "verify-tok", "", nil, nil, nil)

	body := cardActionBody("ou_test_user", "alert-6", "resolve")
	req := signedRequest("verify-tok", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if alerts.alerts["alert-6"].Status != domain.StatusResolved {
		t.Errorf("Status = %v, want resolved", alerts.alerts["alert-6"].Status)
	}
}
