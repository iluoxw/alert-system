// 轻量目录端点（directory.go，serviceability C4）+ 健康检查（C5）的 httptest 覆盖：
// 登录级鉴权（member 也能访问，非 admin only）、未登录 401、字段白名单回归锚
// （防未来 store.Team/store.User 加字段被顺手带出 email/username/enabled 等敏感
// 字段）、healthz 恒 200、readyz 两态。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"alert-system/internal/store"
)

// newDirectoryTestEnv 复用 auth_test.go 的 newTestRouter/addTestUser/doLogin 与
// config_test.go 的 cfgTeamStore/cfgUserStore（同包，不重复定义 fake）。
func newDirectoryTestEnv(t *testing.T) (router *Router, teams *cfgTeamStore, users *cfgUserStore, adminCookie, memberCookie *http.Cookie) {
	t.Helper()
	router, _, authUsers, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(authUsers, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	adminCookie = doLogin(t, router, "admin", "correct-password")

	addTestUser(authUsers, "member-1", "member", "correct-password", true)
	memberCookie = doLogin(t, router, "member", "correct-password")

	teams = newCfgTeamStore()
	users = newCfgUserStore()
	router.RegisterDirectoryRoutes(teams, users)
	return router, teams, users, adminCookie, memberCookie
}

// 非 admin（普通 member）会话也能访问两个轻量目录端点——R4 的关键点，与
// /admin/teams、/admin/users（admin only）形成对照。
func TestDirectory_Member_CanListTeamsAndUsers(t *testing.T) {
	router, teams, users, _, memberCookie := newDirectoryTestEnv(t)
	if _, err := teams.Create(context.Background(), nil, "sre"); err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	if _, err := users.Create(context.Background(), nil, store.User{Username: "alice", PasswordHash: "x", DisplayName: "Alice"}); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/teams", memberCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/teams status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/users", memberCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/users status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestDirectory_Unauthenticated_Returns401(t *testing.T) {
	router, _, _, _, _ := newDirectoryTestEnv(t)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/teams", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/teams status = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/users", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/users status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestDirectory_FieldWhitelist 是防回归锚：teams 只准出 id/name 两个 key，users
// 只准出 id/display_name 两个 key，绝不能带出 email/username/enabled 等字段。
func TestDirectory_FieldWhitelist(t *testing.T) {
	router, teams, users, _, memberCookie := newDirectoryTestEnv(t)
	if _, err := teams.Create(context.Background(), nil, "sre"); err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	if _, err := users.Create(context.Background(), nil, store.User{
		Username: "alice", PasswordHash: "secret-hash", DisplayName: "Alice",
		Email: "alice@example.com", Enabled: true,
	}); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/teams", memberCookie))
	var teamResp struct {
		Teams []map[string]any `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &teamResp); err != nil {
		t.Fatalf("解析 teams 响应失败: %v", err)
	}
	if len(teamResp.Teams) != 1 {
		t.Fatalf("teams 数量 = %d, want 1", len(teamResp.Teams))
	}
	if len(teamResp.Teams[0]) != 2 {
		t.Fatalf("team 字段数 = %d, want 2（只允许 id/name）: %+v", len(teamResp.Teams[0]), teamResp.Teams[0])
	}
	if _, ok := teamResp.Teams[0]["id"]; !ok {
		t.Fatalf("team 缺 id 字段: %+v", teamResp.Teams[0])
	}
	if _, ok := teamResp.Teams[0]["name"]; !ok {
		t.Fatalf("team 缺 name 字段: %+v", teamResp.Teams[0])
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/users", memberCookie))
	var userResp struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &userResp); err != nil {
		t.Fatalf("解析 users 响应失败: %v", err)
	}
	if len(userResp.Users) != 1 {
		t.Fatalf("users 数量 = %d, want 1", len(userResp.Users))
	}
	if len(userResp.Users[0]) != 2 {
		t.Fatalf("user 字段数 = %d, want 2（只允许 id/display_name）: %+v", len(userResp.Users[0]), userResp.Users[0])
	}
	if _, ok := userResp.Users[0]["id"]; !ok {
		t.Fatalf("user 缺 id 字段: %+v", userResp.Users[0])
	}
	if _, ok := userResp.Users[0]["display_name"]; !ok {
		t.Fatalf("user 缺 display_name 字段: %+v", userResp.Users[0])
	}
	for _, forbidden := range []string{"email", "username", "enabled", "password_hash"} {
		if _, ok := userResp.Users[0][forbidden]; ok {
			t.Fatalf("user 泄露了敏感字段 %q: %+v", forbidden, userResp.Users[0])
		}
	}
}

// ---- 健康检查 ----

func TestHealthz_Unauthenticated_Returns200(t *testing.T) {
	router, _, _, _, _ := newTestRouter(time.Hour, time.Hour)
	router.RegisterHealthRoutes(nil)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestReadyz_ReadyFuncSuccess_Returns200(t *testing.T) {
	router, _, _, _, _ := newTestRouter(time.Hour, time.Hour)
	router.RegisterHealthRoutes(func(ctx context.Context) error { return nil })

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestReadyz_ReadyFuncError_Returns503 同时断言错误响应不泄露 DSN/连接串——即使注入的
// ready 函数返回的原始 error 里含连接串，handler 也只应回一句概括文案。
func TestReadyz_ReadyFuncError_Returns503(t *testing.T) {
	router, _, _, _, _ := newTestRouter(time.Hour, time.Hour)
	router.RegisterHealthRoutes(func(ctx context.Context) error {
		return errors.New("dial postgres://user:secret-password@db:5432/alert 失败")
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "postgres://") || strings.Contains(w.Body.String(), "secret-password") {
		t.Fatalf("readyz 503 响应体泄露了 DSN/密码: %s", w.Body.String())
	}
}
