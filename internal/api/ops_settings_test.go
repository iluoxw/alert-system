package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// ---- fakeOpsSettingsStore：内存版 OpsSettingsStore ----

type fakeOpsSettingsStore struct {
	byTeam map[string]store.OpsSettings
}

func newFakeOpsSettingsStore() *fakeOpsSettingsStore {
	return &fakeOpsSettingsStore{byTeam: map[string]store.OpsSettings{}}
}

func (f *fakeOpsSettingsStore) GetSettings(ctx context.Context, tx pgx.Tx, teamID string) (*store.OpsSettings, error) {
	s, ok := f.byTeam[teamID]
	if !ok {
		return nil, store.ErrNotFound
	}
	out := s
	return &out, nil
}

func (f *fakeOpsSettingsStore) UpsertSettings(ctx context.Context, tx pgx.Tx, s store.OpsSettings) error {
	f.byTeam[s.TeamID] = s
	return nil
}

// ---- fakeOpsScheduler：内存版 OpsSettingsScheduler，记录 pending 定时器供测试断言
// "开关翻转后 timers 状态变化"（任务 10 Success 里的第二类断言）----

type fakeScheduledTimer struct {
	FireAt  time.Time
	Payload []byte
}

type fakeOpsScheduler struct {
	pending map[string]fakeScheduledTimer // key = kind + "|" + refID
}

func newFakeOpsScheduler() *fakeOpsScheduler {
	return &fakeOpsScheduler{pending: map[string]fakeScheduledTimer{}}
}

func (f *fakeOpsScheduler) Schedule(ctx context.Context, tx pgx.Tx, kind, refID string, fireAt time.Time, payload []byte) error {
	f.pending[kind+"|"+refID] = fakeScheduledTimer{FireAt: fireAt, Payload: payload}
	return nil
}

func (f *fakeOpsScheduler) Cancel(ctx context.Context, tx pgx.Tx, kind, refID string) error {
	delete(f.pending, kind+"|"+refID)
	return nil
}

// ---- 测试装配 helper：GET 登录即可、PUT admin only，故同时准备 member/admin 两个
// cookie（复用 auth_test.go 的 newTestRouter/addTestUser/doLogin，cfgTeamStore 复用
// config_test.go 已有的 fake 团队仓储——同包直接复用，不重复定义）----

type opsTestEnv struct {
	router       *Router
	adminCookie  *http.Cookie
	memberCookie *http.Cookie
	teams        *cfgTeamStore
	settings     *fakeOpsSettingsStore
	sched        *fakeOpsScheduler
}

func newOpsTestEnv(t *testing.T) *opsTestEnv {
	t.Helper()
	router, _, authUsers, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(authUsers, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	adminCookie := doLogin(t, router, "admin", "correct-password")

	addTestUser(authUsers, "member-1", "member", "correct-password", true)
	memberCookie := doLogin(t, router, "member", "correct-password")

	env := &opsTestEnv{
		router:       router,
		adminCookie:  adminCookie,
		memberCookie: memberCookie,
		teams:        newCfgTeamStore(),
		settings:     newFakeOpsSettingsStore(),
		sched:        newFakeOpsScheduler(),
	}
	router.RegisterOpsSettingsRoutes(env.teams, env.settings, env.sched)
	return env
}

func (env *opsTestEnv) createTeam(t *testing.T, name string) string {
	t.Helper()
	team, err := env.teams.Create(context.Background(), nil, name)
	if err != nil {
		t.Fatalf("创建团队失败: %v", err)
	}
	return team.ID
}

func opsRequest(method, path string, body any, cookie *http.Cookie) *http.Request {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func (env *opsTestEnv) get(cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, opsRequest(http.MethodGet, path, nil, cookie))
	return w
}

func (env *opsTestEnv) put(cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, opsRequest(http.MethodPut, path, body, cookie))
	return w
}

// validOpsSettingsBody 返回一份三开关全开、channels 引用已注册渠道的合法 PUT 请求体，
// 各测试按需覆盖单个字段来触发某一类校验失败。
func validOpsSettingsBody() map[string]any {
	return map[string]any{
		"timezone":                     "Asia/Shanghai",
		"daily_report_enabled":         true,
		"daily_report_time":            "09:00",
		"stale_sweep_enabled":          true,
		"stale_sweep_interval_seconds": 1800,
		"oncall_broadcast_enabled":     true,
		"oncall_broadcast_time":        "09:30",
		"channels":                     []map[string]string{{"kind": cfgRegisteredChannelKind, "target": "group-1"}},
	}
}

// ---- GET ----

func TestOpsSettingsGet_Unconfigured_ReturnsDefaults(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")

	w := env.get(env.memberCookie, "/api/v1/teams/"+teamID+"/ops-settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	want := map[string]any{
		"team_id":                      teamID,
		"timezone":                     "Asia/Shanghai",
		"daily_report_enabled":         false,
		"daily_report_time":            "09:00",
		"stale_sweep_enabled":          false,
		"stale_sweep_interval_seconds": float64(1800),
		"oncall_broadcast_enabled":     false,
		"oncall_broadcast_time":        "09:30",
	}
	for k, v := range want {
		if resp[k] != v {
			t.Errorf("resp[%q] = %v, want %v", k, resp[k], v)
		}
	}
	if channels, ok := resp["channels"].([]any); !ok || len(channels) != 0 {
		t.Errorf("channels = %v, want 空数组", resp["channels"])
	}
}

func TestOpsSettingsGet_TeamNotFound_Returns404(t *testing.T) {
	env := newOpsTestEnv(t)
	w := env.get(env.memberCookie, "/api/v1/teams/no-such-team/ops-settings")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestOpsSettingsGet_NonAdminAllowed(t *testing.T) {
	// 证明 GET 是登录即可、不要求 admin：member 也能查看（R2 前端渲染表单需要）。
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")
	w := env.get(env.memberCookie, "/api/v1/teams/"+teamID+"/ops-settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ---- PUT 鉴权 ----

func TestOpsSettingsPut_NonAdmin_Returns403(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")
	w := env.put(env.memberCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", validOpsSettingsBody())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestOpsSettingsPut_TeamNotFound_Returns404(t *testing.T) {
	env := newOpsTestEnv(t)
	w := env.put(env.adminCookie, "/api/v1/admin/teams/no-such-team/ops-settings", validOpsSettingsBody())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ---- PUT 校验失败：四类 422 ----

func TestOpsSettingsPut_UnregisteredChannel_Returns422(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")
	body := validOpsSettingsBody()
	body["channels"] = []map[string]string{{"kind": "not-registered", "target": "x"}}

	w := env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestOpsSettingsPut_BadHHMM_Returns422(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")
	body := validOpsSettingsBody()
	body["daily_report_time"] = "9:00" // 非零填充，不满足严格 HH:MM 格式

	w := env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestOpsSettingsPut_IntervalTooSmall_Returns422(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")
	body := validOpsSettingsBody()
	body["stale_sweep_interval_seconds"] = 59

	w := env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestOpsSettingsPut_BadTimezone_Returns422(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")
	body := validOpsSettingsBody()
	body["timezone"] = "Not/A_Real_Zone"

	w := env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// ---- PUT 成功路径：事务内 Schedule/Cancel 编排 ----

func TestOpsSettingsPut_Success_SchedulesEnabledTimers(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")

	w := env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", validOpsSettingsBody())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["daily_report_enabled"] != true {
		t.Errorf("daily_report_enabled = %v, want true", resp["daily_report_enabled"])
	}

	for _, kind := range []string{"ops_daily_report", "ops_stale_sweep", "ops_oncall_broadcast"} {
		if _, ok := env.sched.pending[kind+"|"+teamID]; !ok {
			t.Errorf("kind=%s 未被 Schedule（refID=%s）", kind, teamID)
		}
	}
}

func TestOpsSettingsPut_ToggleOff_CancelsTimer(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")

	// 第一次保存：三个任务全开，全部应有 pending 定时器。
	w := env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", validOpsSettingsBody())
	if w.Code != http.StatusOK {
		t.Fatalf("首次保存 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(env.sched.pending) != 3 {
		t.Fatalf("首次保存后 pending 定时器数 = %d, want 3", len(env.sched.pending))
	}

	// 第二次保存：三个任务全关，对应 pending 定时器应被 Cancel 清除。
	body := validOpsSettingsBody()
	body["daily_report_enabled"] = false
	body["stale_sweep_enabled"] = false
	body["oncall_broadcast_enabled"] = false
	w = env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", body)
	if w.Code != http.StatusOK {
		t.Fatalf("第二次保存 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(env.sched.pending) != 0 {
		t.Errorf("关闭全部开关后 pending 定时器数 = %d, want 0（%v）", len(env.sched.pending), env.sched.pending)
	}
}

func TestOpsSettingsPut_ThenGet_RoundTrip(t *testing.T) {
	env := newOpsTestEnv(t)
	teamID := env.createTeam(t, "sre")

	env.put(env.adminCookie, "/api/v1/admin/teams/"+teamID+"/ops-settings", validOpsSettingsBody())

	w := env.get(env.memberCookie, "/api/v1/teams/"+teamID+"/ops-settings")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["stale_sweep_interval_seconds"] != float64(1800) {
		t.Errorf("stale_sweep_interval_seconds = %v, want 1800", resp["stale_sweep_interval_seconds"])
	}
	channels, ok := resp["channels"].([]any)
	if !ok || len(channels) != 1 {
		t.Fatalf("channels = %v, want 1 个元素", resp["channels"])
	}
}
