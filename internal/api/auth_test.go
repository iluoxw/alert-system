package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"alert-system/internal/config"
	"alert-system/internal/store"
)

// ---- 内存 fake：UserStore/SessionStore/MembershipStore/Clock ----
// fakeTxRunner 复用 lark_callback_test.go 里已有的定义（同包，不重复声明）。

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeUserStore 是 UserStore 的内存 fake。
type fakeUserStore struct {
	byID       map[string]store.User
	byUsername map[string]store.User
}

func (f *fakeUserStore) GetByUsername(ctx context.Context, tx pgx.Tx, username string) (*store.User, error) {
	u, ok := f.byUsername[username]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &u, nil
}

func (f *fakeUserStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.User, error) {
	u, ok := f.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &u, nil
}

// fakeSessionStore 是 SessionStore 的内存 fake；LastSeenAt 未指定时按 clock.Now()
// 填充，模拟 sessions 表 last_seen_at 列的 DEFAULT now()。
type fakeSessionStore struct {
	clock    *fakeClock
	sessions map[string]*store.Session
	byHash   map[string]string
	seq      int
}

func (f *fakeSessionStore) Create(ctx context.Context, tx pgx.Tx, s store.Session) (*store.Session, error) {
	f.seq++
	s.ID = fmt.Sprintf("sess-%d", f.seq)
	if s.LastSeenAt.IsZero() {
		s.LastSeenAt = f.clock.Now()
	}
	cp := s
	f.sessions[cp.ID] = &cp
	f.byHash[cp.TokenHash] = cp.ID
	out := cp
	return &out, nil
}

func (f *fakeSessionStore) GetByTokenHash(ctx context.Context, tx pgx.Tx, tokenHash string) (*store.Session, error) {
	id, ok := f.byHash[tokenHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	s, ok := f.sessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	out := *s
	return &out, nil
}

func (f *fakeSessionStore) Touch(ctx context.Context, tx pgx.Tx, id string, lastSeenAt time.Time) error {
	s, ok := f.sessions[id]
	if !ok {
		return store.ErrNotFound
	}
	s.LastSeenAt = lastSeenAt
	return nil
}

func (f *fakeSessionStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	delete(f.sessions, id)
	return nil
}

// fakeMembershipStore 是 MembershipStore 的内存 fake：userID -> 是否在任一团队持有 admin。
type fakeMembershipStore struct {
	admins map[string]bool
}

func (f *fakeMembershipStore) IsAdminAnyTeam(ctx context.Context, tx pgx.Tx, userID string) (bool, error) {
	return f.admins[userID], nil
}

// ---- 测试装配 helper ----

func newTestRouter(sessionTTL, idleTTL time.Duration) (*Router, *fakeClock, *fakeUserStore, *fakeSessionStore, *fakeMembershipStore) {
	clock := &fakeClock{now: time.Now()}
	users := &fakeUserStore{byID: map[string]store.User{}, byUsername: map[string]store.User{}}
	sessions := &fakeSessionStore{clock: clock, sessions: map[string]*store.Session{}, byHash: map[string]string{}}
	memberships := &fakeMembershipStore{admins: map[string]bool{}}
	auth := NewPasswordAuthenticator(users)
	cfg := config.Config{
		SessionTTL:          sessionTTL,
		SessionIdleTTL:      idleTTL,
		SessionCookieSecure: false, // 测试用明文 http，需要关闭 Secure 才能在 httptest 里收发 cookie
		Lark:                config.LarkConfig{VerificationToken: "test-verify-token"},
	}
	lark := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	router := NewRouter(cfg, users, sessions, memberships, fakeTxRunner{}, auth, lark, nil)
	router.clock = clock // 白盒覆盖，保证会话校验与测试推进的是同一个时钟
	return router, clock, users, sessions, memberships
}

func addTestUser(users *fakeUserStore, id, username, password string, enabled bool) store.User {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost) // MinCost 加速测试，不影响断言
	if err != nil {
		panic(err)
	}
	u := store.User{ID: id, Username: username, PasswordHash: string(hash), DisplayName: username, Enabled: enabled}
	users.byID[id] = u
	users.byUsername[username] = u
	return u
}

func loginRequest(username, password string) *http.Request {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// doLogin 执行登录并返回 session cookie（调用方断言登录本身应当成功）。
func doLogin(t *testing.T, router *Router, username, password string) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, loginRequest(username, password))
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败: status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("登录响应未设置 %s cookie", sessionCookieName)
	return nil
}

func authedRequest(method, path string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

// ---- 测试矩阵 ----

// 未登录访问受保护路由 → 401。
func TestSessionAuth_Unauthenticated_Returns401(t *testing.T) {
	router, _, _, _, _ := newTestRouter(time.Hour, time.Hour)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/me", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// 登录成功 → 后续携带 cookie 的请求可以访问受保护路由。
func TestLogin_Success_CookieAuthenticatedAccessSucceeds(t *testing.T) {
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "alice", "correct-password", true)

	cookie := doLogin(t, router, "alice", "correct-password")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/me", cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Username != "alice" {
		t.Errorf("username = %q, want alice", resp.Username)
	}
}

// member（未持有任何团队 admin 角色）访问 admin 专属路由 → 403。
func TestRequireAdmin_MemberAccessingAdminRoute_Returns403(t *testing.T) {
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "bob", "correct-password", true)
	// 不加入 memberships.admins，保持非 admin。

	cookie := doLogin(t, router, "bob", "correct-password")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/admin/ping", cookie))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// admin（至少一个团队持有 admin 角色）访问 admin 专属路由 → 200，与上一测试对照，
// 证明 requireAdmin() 确实按角色区分而不是恒定放行/拒绝。
func TestRequireAdmin_AdminAccessingAdminRoute_Succeeds(t *testing.T) {
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "carol", "correct-password", true)
	memberships.admins["u1"] = true

	cookie := doLogin(t, router, "carol", "correct-password")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/admin/ping", cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// 登出后 DB 侧会话已删除 → 用旧 cookie 访问返回 401（不是只清客户端 cookie）。
func TestLogout_ThenAccess_Returns401(t *testing.T) {
	router, _, users, sessions, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "dave", "correct-password", true)

	cookie := doLogin(t, router, "dave", "correct-password")
	if len(sessions.sessions) != 1 {
		t.Fatalf("登录后应存在 1 条会话记录，实际 %d", len(sessions.sessions))
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodPost, "/api/v1/auth/logout", cookie))
	if w.Code != http.StatusNoContent {
		t.Fatalf("登出 status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if len(sessions.sessions) != 0 {
		t.Errorf("登出后会话应从存储中删除，实际仍剩 %d 条", len(sessions.sessions))
	}

	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, authedRequest(http.MethodGet, "/api/v1/me", cookie))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("登出后用旧 cookie 访问 status = %d, want 401", w2.Code)
	}
}

// 固定过期：超过 SessionTTL（即使一直有请求持续续期滑动窗口）→ 401。
func TestSessionAuth_FixedExpiry_Returns401(t *testing.T) {
	router, clock, users, _, _ := newTestRouter(time.Minute, time.Hour) // TTL 短、idle 窗口长
	addTestUser(users, "u1", "erin", "correct-password", true)
	cookie := doLogin(t, router, "erin", "correct-password")

	clock.now = clock.now.Add(2 * time.Minute) // 超过固定过期，尚未超过 idle 窗口

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/me", cookie))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（固定过期）", w.Code)
	}
}

// 滑动续期：超过 SessionIdleTTL 无请求（即使固定过期还没到）→ 401。
func TestSessionAuth_IdleExpiry_Returns401(t *testing.T) {
	router, clock, users, _, _ := newTestRouter(24*time.Hour, time.Minute) // TTL 长、idle 窗口短
	addTestUser(users, "u1", "frank", "correct-password", true)
	cookie := doLogin(t, router, "frank", "correct-password")

	clock.now = clock.now.Add(2 * time.Minute) // 超过 idle 窗口，尚未超过固定过期

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/me", cookie))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（滑动续期窗口耗尽）", w.Code)
	}
}

// 滑动续期确实会推进水位线：idle 窗口内的持续访问应当不断续期，总耗时可以超过单个
// idle 窗口而不失效——与"固定过期"测试对照，证明两条规则是独立生效的。
func TestSessionAuth_SlidingRenewal_KeepsSessionAliveAcrossMultipleIdleWindows(t *testing.T) {
	router, clock, users, _, _ := newTestRouter(24*time.Hour, time.Minute)
	addTestUser(users, "u1", "grace", "correct-password", true)
	cookie := doLogin(t, router, "grace", "correct-password")

	for i := 0; i < 3; i++ {
		clock.now = clock.now.Add(30 * time.Second) // 每次都小于 idle 窗口
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/me", cookie))
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次续期访问 status = %d, want 200", i+1, w.Code)
		}
	}
}

// 防用户名枚举：用户不存在 与 用户存在但密码错误，返回完全相同的状态码与响应体。
func TestLogin_AntiEnumeration_IdenticalResponseForUnknownUserAndWrongPassword(t *testing.T) {
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "heidi", "correct-password", true)

	wUnknown := httptest.NewRecorder()
	router.ServeHTTP(wUnknown, loginRequest("no-such-user", "whatever"))

	wWrongPassword := httptest.NewRecorder()
	router.ServeHTTP(wWrongPassword, loginRequest("heidi", "wrong-password"))

	if wUnknown.Code != http.StatusUnauthorized || wWrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("两种失败都应是 401；unknown=%d, wrongPassword=%d", wUnknown.Code, wWrongPassword.Code)
	}
	if wUnknown.Body.String() != wWrongPassword.Body.String() {
		t.Errorf("两种失败的响应体应完全一致；unknown=%q, wrongPassword=%q",
			wUnknown.Body.String(), wWrongPassword.Body.String())
	}
	for k := range wUnknown.Result().Header {
		if wUnknown.Result().Header.Get(k) != wWrongPassword.Result().Header.Get(k) {
			t.Errorf("响应头 %s 不一致: unknown=%q, wrongPassword=%q",
				k, wUnknown.Result().Header.Get(k), wWrongPassword.Result().Header.Get(k))
		}
	}
}

// 已禁用账号：即使 session 本身仍在固定过期与 idle 窗口内，也应视为失效（每次请求
// 都会重新查询 users.enabled）。
func TestSessionAuth_DisabledUserMidSession_Returns401(t *testing.T) {
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "ivan", "correct-password", true)
	cookie := doLogin(t, router, "ivan", "correct-password")

	// 会话建立后账号被禁用（模拟管理员中途撤销）。
	u := users.byID["u1"]
	u.Enabled = false
	users.byID["u1"] = u
	users.byUsername["ivan"] = u

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/me", cookie))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（账号已禁用）", w.Code)
	}
}

// /callbacks/lark/card 豁免 session 中间件（R8.3）：不带任何 cookie 也应能命中该路由
// （具体的验签逻辑属于 lark_callback.go，这里只验证路由挂载没有被 session 中间件拦截）。
func TestLarkCallbackRoute_ExemptFromSessionMiddleware(t *testing.T) {
	router, _, _, _, _ := newTestRouter(time.Hour, time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/callbacks/lark/card", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（session 中间件不应拦截这条路由）", w.Code)
	}
}
