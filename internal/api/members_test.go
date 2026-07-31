// 团队成员管理端点（members.go，serviceability C1）的 httptest 覆盖：鉴权、404/409/422
// 三类错误路径、最后一名 admin 护栏两个分支、正常 CRUD 全流程。
//
// fake 命名统一加 member 前缀，避免与 auth_test.go/config_test.go/ops_settings_test.go
// 已有 fake 撞名；team/user 的存在性校验直接复用 config_test.go 的 cfgTeamStore/
// cfgUserStore（同包，不重复定义）。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// ---- 内存 fake：MemberStore ----
//
// key 是 teamID+"|"+userID；ListByTeam 借助 users（cfgUserStore）回填
// username/display_name，模拟真实仓储的 join users 行为。

type memberFakeRow struct {
	userID string
	role   string
}

type memberFakeStore struct {
	users *cfgUserStore
	byKey map[string]memberFakeRow
}

func newMemberFakeStore(users *cfgUserStore) *memberFakeStore {
	return &memberFakeStore{users: users, byKey: map[string]memberFakeRow{}}
}

func memberFakeKey(teamID, userID string) string { return teamID + "|" + userID }

func (s *memberFakeStore) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string) ([]store.Member, error) {
	prefix := teamID + "|"
	var out []store.Member
	for key, row := range s.byKey {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		m := store.Member{UserID: row.userID, Role: row.role}
		if u, err := s.users.GetByID(ctx, tx, row.userID); err == nil {
			m.Username = u.Username
			m.DisplayName = u.DisplayName
		}
		out = append(out, m)
	}
	// 按 username 排序，与 store.MembershipRepo.ListByTeam 的 ORDER BY u.username 对齐。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Username > out[j].Username; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out, nil
}

func (s *memberFakeStore) Add(ctx context.Context, tx pgx.Tx, teamID, userID, role string) error {
	key := memberFakeKey(teamID, userID)
	if _, exists := s.byKey[key]; exists {
		return store.ErrMembershipConflict
	}
	s.byKey[key] = memberFakeRow{userID: userID, role: role}
	return nil
}

func (s *memberFakeStore) UpdateRole(ctx context.Context, tx pgx.Tx, teamID, userID, role string) error {
	key := memberFakeKey(teamID, userID)
	row, ok := s.byKey[key]
	if !ok {
		return store.ErrNotFound
	}
	row.role = role
	s.byKey[key] = row
	return nil
}

func (s *memberFakeStore) Remove(ctx context.Context, tx pgx.Tx, teamID, userID string) error {
	key := memberFakeKey(teamID, userID)
	if _, ok := s.byKey[key]; !ok {
		return store.ErrNotFound
	}
	delete(s.byKey, key)
	return nil
}

func (s *memberFakeStore) CountAdmins(ctx context.Context, tx pgx.Tx, teamID string) (int, error) {
	prefix := teamID + "|"
	count := 0
	for key, row := range s.byKey {
		if strings.HasPrefix(key, prefix) && row.role == "admin" {
			count++
		}
	}
	return count, nil
}

// ---- 测试装配 helper ----

type memberTestEnv struct {
	router       *Router
	adminCookie  *http.Cookie
	memberCookie *http.Cookie
	teams        *cfgTeamStore
	users        *cfgUserStore
	members      *memberFakeStore
}

func newMemberTestEnv(t *testing.T) *memberTestEnv {
	t.Helper()
	router, _, authUsers, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(authUsers, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	adminCookie := doLogin(t, router, "admin", "correct-password")

	addTestUser(authUsers, "member-1", "member", "correct-password", true)
	memberCookie := doLogin(t, router, "member", "correct-password")

	env := &memberTestEnv{
		router:       router,
		adminCookie:  adminCookie,
		memberCookie: memberCookie,
		teams:        newCfgTeamStore(),
		users:        newCfgUserStore(),
	}
	env.members = newMemberFakeStore(env.users)
	router.RegisterMemberRoutes(env.teams, env.users, env.members)
	return env
}

func (env *memberTestEnv) createTeam(t *testing.T, name string) string {
	t.Helper()
	team, err := env.teams.Create(context.Background(), nil, name)
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	return team.ID
}

func (env *memberTestEnv) createUser(t *testing.T, username string) *store.User {
	t.Helper()
	u, err := env.users.Create(context.Background(), nil, store.User{Username: username, PasswordHash: "x", DisplayName: username})
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", username, err)
	}
	return u
}

// do 发起一次请求；body 复用 opsRequest（ops_settings_test.go 已定义，同包直接用）的
// JSON 编码逻辑。
func (env *memberTestEnv) do(method string, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, opsRequest(method, path, body, cookie))
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, w.Body.String())
	}
	return m
}

// ---- 鉴权 ----

func TestMembers_NonAdmin_Returns403(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	w := env.do(http.MethodGet, env.memberCookie, "/api/v1/admin/teams/"+teamID+"/members", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// ---- Add：404/409/422 ----

func TestMembersAdd_TeamNotFound_Returns404(t *testing.T) {
	env := newMemberTestEnv(t)
	u := env.createUser(t, "alice")
	body := map[string]string{"user_id": u.ID, "role": "member"}
	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/no-such-team/members", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestMembersAdd_UserNotFound_Returns404(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	body := map[string]string{"user_id": "no-such-user", "role": "member"}
	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestMembersAdd_Duplicate_Returns409(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	u := env.createUser(t, "alice")
	body := map[string]string{"user_id": u.ID, "role": "member"}

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("首次添加 status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	w = env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复添加 status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

func TestMembersAdd_InvalidRole_Returns422(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	u := env.createUser(t, "alice")
	body := map[string]string{"user_id": u.ID, "role": "owner"}
	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// ---- UpdateRole：404/422 ----

func TestMembersUpdateRole_NotFound_Returns404(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/no-such-user", map[string]string{"role": "admin"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestMembersUpdateRole_InvalidRole_Returns422(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	u := env.createUser(t, "alice")
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": u.ID, "role": "member"})

	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+u.ID, map[string]string{"role": "owner"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// ---- 最后一名 admin 护栏 ----

func TestMembersRemove_LastAdmin_Returns422(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	admin := env.createUser(t, "alice")
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": admin.ID, "role": "admin"})

	w := env.do(http.MethodDelete, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+admin.ID, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("移除最后一名 admin status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestMembersUpdateRole_DowngradeLastAdmin_Returns422(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	admin := env.createUser(t, "alice")
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": admin.ID, "role": "admin"})

	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+admin.ID, map[string]string{"role": "member"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("降级最后一名 admin status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestMembersRemove_NonLastAdmin_Returns204(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	admin1 := env.createUser(t, "alice")
	admin2 := env.createUser(t, "bob")
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": admin1.ID, "role": "admin"})
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": admin2.ID, "role": "admin"})

	w := env.do(http.MethodDelete, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+admin1.ID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// TestMembersUpdateRole_SelfDowngrade_Allowed 验证「自降级合法」（design.md C1：服务端
// 不特判操作者身份）——只要不是团队最后一名 admin，降级请求本身（无论目标是谁）都放行。
func TestMembersUpdateRole_SelfDowngrade_Allowed(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	target := env.createUser(t, "alice")
	other := env.createUser(t, "bob")
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": target.ID, "role": "admin"})
	env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": other.ID, "role": "admin"})

	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+target.ID, map[string]string{"role": "member"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ---- 正常路径：Add -> List -> UpdateRole -> Remove -> List ----

func TestMembers_FullCRUD_Success(t *testing.T) {
	env := newMemberTestEnv(t)
	teamID := env.createTeam(t, "sre")
	alice := env.createUser(t, "alice")
	bob := env.createUser(t, "bob")

	// 故意先加 bob 再加 alice，验证响应体字段与 List 排序。
	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": bob.ID, "role": "member"})
	if w.Code != http.StatusCreated {
		t.Fatalf("添加 bob status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	w = env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", map[string]string{"user_id": alice.ID, "role": "admin"})
	if w.Code != http.StatusCreated {
		t.Fatalf("添加 alice status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	addResp := decodeJSON(t, w)
	if addResp["username"] != "alice" || addResp["display_name"] != "alice" || addResp["role"] != "admin" || addResp["user_id"] != alice.ID {
		t.Fatalf("添加 alice 响应体 = %+v", addResp)
	}

	w = env.do(http.MethodGet, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("List status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Members []map[string]any `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("解析 List 响应失败: %v", err)
	}
	if len(listResp.Members) != 2 {
		t.Fatalf("成员数 = %d, want 2: %+v", len(listResp.Members), listResp.Members)
	}
	if listResp.Members[0]["username"] != "alice" || listResp.Members[1]["username"] != "bob" {
		t.Fatalf("List 排序不对: %+v", listResp.Members)
	}

	// UpdateRole：bob 升为 admin，之后移除 alice 不应再触发最后 admin 护栏。
	w = env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+bob.ID, map[string]string{"role": "admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateRole status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	updateResp := decodeJSON(t, w)
	if updateResp["role"] != "admin" || updateResp["username"] != "bob" {
		t.Fatalf("UpdateRole 响应体 = %+v", updateResp)
	}

	w = env.do(http.MethodDelete, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members/"+alice.ID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("Remove alice status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	w = env.do(http.MethodGet, env.adminCookie, "/api/v1/admin/teams/"+teamID+"/members", nil)
	var listResp2 struct {
		Members []map[string]any `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp2); err != nil {
		t.Fatalf("解析第二次 List 响应失败: %v", err)
	}
	if len(listResp2.Members) != 1 || listResp2.Members[0]["username"] != "bob" {
		t.Fatalf("移除后成员列表 = %+v, want 仅 bob", listResp2.Members)
	}
}
