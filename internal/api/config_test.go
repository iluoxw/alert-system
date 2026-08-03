// 配置管理 API（任务 27）的 httptest 覆盖：每类资源至少覆盖一条校验失败路径与一条
// 成功路径，重点覆盖三类语义校验（route priority 冲突、R13.6 渠道未注册、静默时间窗/
// 非空 matchers）与 R1.3/R10.5 的安全/原子性约束。
//
// 全部内存 fake 按团队约定统一加 cfg/silenceAPI 前缀，避免与 auth_test.go、
// lark_callback_test.go 已有 fake 及并行任务新增的 fake 撞名。
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
	"github.com/jackc/pgx/v5/pgconn"

	"alert-system/internal/notify"
	"alert-system/internal/store"
)

// cfgUniqueViolationErr 模拟 teams.name/users.username 唯一约束冲突时 pgx 返回的错误
// 形态，供 isUniqueViolation（config.go）识别——真实仓储由 Postgres driver 产生同类型
// 错误，这里的 fake 不经过真实 DB，手工构造一个等价的 *pgconn.PgError。
var cfgUniqueViolationErr = &pgconn.PgError{Code: "23505", ConstraintName: "fake_unique_violation"}

// ---- 内存 fake：ConfigTeamStore ----

type cfgTeamStore struct {
	byID   map[string]store.Team
	byName map[string]string
	seq    int
}

func newCfgTeamStore() *cfgTeamStore {
	return &cfgTeamStore{byID: map[string]store.Team{}, byName: map[string]string{}}
}

func (s *cfgTeamStore) Create(ctx context.Context, tx pgx.Tx, name string) (*store.Team, error) {
	if _, exists := s.byName[name]; exists {
		return nil, cfgUniqueViolationErr
	}
	s.seq++
	now := time.Now()
	t := store.Team{ID: fmt.Sprintf("team-%d", s.seq), Name: name, CreatedAt: now, UpdatedAt: now}
	s.byID[t.ID] = t
	s.byName[name] = t.ID
	out := t
	return &out, nil
}

func (s *cfgTeamStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Team, error) {
	t, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &t, nil
}

func (s *cfgTeamStore) List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Team, error) {
	out := make([]store.Team, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, t)
	}
	return out, nil
}

func (s *cfgTeamStore) Update(ctx context.Context, tx pgx.Tx, id, name string) error {
	t, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	t.Name = name
	s.byID[id] = t
	return nil
}

func (s *cfgTeamStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigUserStore ----

type cfgUserStore struct {
	byID       map[string]store.User
	byUsername map[string]string
	seq        int
}

func newCfgUserStore() *cfgUserStore {
	return &cfgUserStore{byID: map[string]store.User{}, byUsername: map[string]string{}}
}

func (s *cfgUserStore) Create(ctx context.Context, tx pgx.Tx, u store.User) (*store.User, error) {
	if _, exists := s.byUsername[u.Username]; exists {
		return nil, cfgUniqueViolationErr
	}
	s.seq++
	now := time.Now()
	u.ID = fmt.Sprintf("user-%d", s.seq)
	u.CreatedAt, u.UpdatedAt = now, now
	s.byID[u.ID] = u
	s.byUsername[u.Username] = u.ID
	out := u
	return &out, nil
}

func (s *cfgUserStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.User, error) {
	u, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &u, nil
}

func (s *cfgUserStore) List(ctx context.Context, tx pgx.Tx, limit int) ([]store.User, error) {
	out := make([]store.User, 0, len(s.byID))
	for _, u := range s.byID {
		out = append(out, u)
	}
	return out, nil
}

func (s *cfgUserStore) Update(ctx context.Context, tx pgx.Tx, id, displayName, email string, enabled bool) error {
	u, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	u.DisplayName, u.Email, u.Enabled = displayName, email, enabled
	s.byID[id] = u
	return nil
}

func (s *cfgUserStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigRouteStore ----

type cfgRouteStore struct {
	byID       map[string]store.Route
	byPriority map[int]string
	seq        int
}

func newCfgRouteStore() *cfgRouteStore {
	return &cfgRouteStore{byID: map[string]store.Route{}, byPriority: map[int]string{}}
}

func (s *cfgRouteStore) Create(ctx context.Context, tx pgx.Tx, route store.Route) (*store.Route, error) {
	if _, exists := s.byPriority[route.Priority]; exists {
		return nil, store.ErrRoutePriorityConflict
	}
	s.seq++
	now := time.Now()
	route.ID = fmt.Sprintf("route-%d", s.seq)
	route.CreatedAt, route.UpdatedAt = now, now
	s.byID[route.ID] = route
	s.byPriority[route.Priority] = route.ID
	out := route
	return &out, nil
}

func (s *cfgRouteStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Route, error) {
	r, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &r, nil
}

func (s *cfgRouteStore) ListOrdered(ctx context.Context, tx pgx.Tx, limit int) ([]store.Route, error) {
	out := make([]store.Route, 0, len(s.byID))
	for _, r := range s.byID {
		out = append(out, r)
	}
	return out, nil
}

func (s *cfgRouteStore) Update(ctx context.Context, tx pgx.Tx, route store.Route) error {
	existing, ok := s.byID[route.ID]
	if !ok {
		return store.ErrNotFound
	}
	if ownerID, exists := s.byPriority[route.Priority]; exists && ownerID != route.ID {
		return store.ErrRoutePriorityConflict
	}
	delete(s.byPriority, existing.Priority)
	route.CreatedAt = existing.CreatedAt
	s.byID[route.ID] = route
	s.byPriority[route.Priority] = route.ID
	return nil
}

func (s *cfgRouteStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	r, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	delete(s.byPriority, r.Priority)
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigScheduleStore ----

type cfgScheduleStore struct {
	byID map[string]store.Schedule
	seq  int
}

func newCfgScheduleStore() *cfgScheduleStore {
	return &cfgScheduleStore{byID: map[string]store.Schedule{}}
}

func (s *cfgScheduleStore) Create(ctx context.Context, tx pgx.Tx, sch store.Schedule) (*store.Schedule, error) {
	s.seq++
	now := time.Now()
	sch.ID = fmt.Sprintf("schedule-%d", s.seq)
	sch.CreatedAt, sch.UpdatedAt = now, now
	s.byID[sch.ID] = sch
	out := sch
	return &out, nil
}

func (s *cfgScheduleStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Schedule, error) {
	sch, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &sch, nil
}

func (s *cfgScheduleStore) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.Schedule, error) {
	var out []store.Schedule
	for _, sch := range s.byID {
		if sch.TeamID == teamID {
			out = append(out, sch)
		}
	}
	return out, nil
}

func (s *cfgScheduleStore) ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]store.Schedule, error) {
	out := make([]store.Schedule, 0, len(s.byID))
	for _, sch := range s.byID {
		out = append(out, sch)
	}
	return out, nil
}

func (s *cfgScheduleStore) Update(ctx context.Context, tx pgx.Tx, id, name, timezone string) error {
	sch, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	sch.Name, sch.Timezone = name, timezone
	s.byID[id] = sch
	return nil
}

func (s *cfgScheduleStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigEscalationPolicyStore ----

type cfgEscalationPolicyStore struct {
	byID map[string]store.EscalationPolicy
	seq  int
}

func newCfgEscalationPolicyStore() *cfgEscalationPolicyStore {
	return &cfgEscalationPolicyStore{byID: map[string]store.EscalationPolicy{}}
}

func (s *cfgEscalationPolicyStore) Create(ctx context.Context, tx pgx.Tx, p store.EscalationPolicy) (*store.EscalationPolicy, error) {
	s.seq++
	now := time.Now()
	p.ID = fmt.Sprintf("esc-%d", s.seq)
	p.CreatedAt, p.UpdatedAt = now, now
	s.byID[p.ID] = p
	out := p
	return &out, nil
}

func (s *cfgEscalationPolicyStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.EscalationPolicy, error) {
	p, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &p, nil
}

func (s *cfgEscalationPolicyStore) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.EscalationPolicy, error) {
	var out []store.EscalationPolicy
	for _, p := range s.byID {
		if p.TeamID == teamID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *cfgEscalationPolicyStore) ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]store.EscalationPolicy, error) {
	out := make([]store.EscalationPolicy, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	return out, nil
}

func (s *cfgEscalationPolicyStore) Update(ctx context.Context, tx pgx.Tx, p store.EscalationPolicy) error {
	existing, ok := s.byID[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	p.CreatedAt = existing.CreatedAt
	s.byID[p.ID] = p
	return nil
}

func (s *cfgEscalationPolicyStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigNotificationPolicyStore ----

type cfgNotificationPolicyStore struct {
	byID map[string]store.NotificationPolicy
	seq  int
}

func newCfgNotificationPolicyStore() *cfgNotificationPolicyStore {
	return &cfgNotificationPolicyStore{byID: map[string]store.NotificationPolicy{}}
}

func (s *cfgNotificationPolicyStore) Create(ctx context.Context, tx pgx.Tx, p store.NotificationPolicy) (*store.NotificationPolicy, error) {
	s.seq++
	now := time.Now()
	p.ID = fmt.Sprintf("notif-%d", s.seq)
	p.CreatedAt, p.UpdatedAt = now, now
	s.byID[p.ID] = p
	out := p
	return &out, nil
}

func (s *cfgNotificationPolicyStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.NotificationPolicy, error) {
	p, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &p, nil
}

func (s *cfgNotificationPolicyStore) ListByTeam(ctx context.Context, tx pgx.Tx, teamID string, limit int) ([]store.NotificationPolicy, error) {
	var out []store.NotificationPolicy
	for _, p := range s.byID {
		if p.TeamID == teamID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *cfgNotificationPolicyStore) ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]store.NotificationPolicy, error) {
	out := make([]store.NotificationPolicy, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	return out, nil
}

func (s *cfgNotificationPolicyStore) Update(ctx context.Context, tx pgx.Tx, p store.NotificationPolicy) error {
	existing, ok := s.byID[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	p.CreatedAt = existing.CreatedAt
	s.byID[p.ID] = p
	return nil
}

func (s *cfgNotificationPolicyStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigIntegrationStore ----

type cfgIntegrationStore struct {
	byID   map[string]store.Integration
	byName map[string]string
	seq    int
}

func newCfgIntegrationStore() *cfgIntegrationStore {
	return &cfgIntegrationStore{byID: map[string]store.Integration{}, byName: map[string]string{}}
}

func (s *cfgIntegrationStore) Create(ctx context.Context, tx pgx.Tx, in store.Integration) (*store.Integration, error) {
	if _, exists := s.byName[in.Name]; exists {
		return nil, store.ErrIntegrationNameConflict
	}
	s.seq++
	now := time.Now()
	in.ID = fmt.Sprintf("integration-%d", s.seq)
	in.CreatedAt, in.UpdatedAt = now, now
	s.byID[in.ID] = in
	s.byName[in.Name] = in.ID
	out := in
	return &out, nil
}

func (s *cfgIntegrationStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Integration, error) {
	in, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &in, nil
}

func (s *cfgIntegrationStore) List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Integration, error) {
	out := make([]store.Integration, 0, len(s.byID))
	for _, in := range s.byID {
		out = append(out, in)
	}
	return out, nil
}

func (s *cfgIntegrationStore) Update(ctx context.Context, tx pgx.Tx, in store.Integration) error {
	existing, ok := s.byID[in.ID]
	if !ok {
		return store.ErrNotFound
	}
	in.CreatedAt, in.HookIDHash = existing.CreatedAt, existing.HookIDHash
	s.byID[in.ID] = in
	return nil
}

func (s *cfgIntegrationStore) RotateHookIDHash(ctx context.Context, tx pgx.Tx, id, hookIDHash string) error {
	in, ok := s.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	in.HookIDHash = hookIDHash
	in.UpdatedAt = time.Now()
	s.byID[id] = in
	return nil
}

func (s *cfgIntegrationStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// ---- 内存 fake：ConfigSilenceStore / ConfigAlertSilenceStore ----

type silenceAPISilenceStore struct {
	byID map[string]store.Silence
	seq  int
}

func newSilenceAPISilenceStore() *silenceAPISilenceStore {
	return &silenceAPISilenceStore{byID: map[string]store.Silence{}}
}

func (s *silenceAPISilenceStore) Create(ctx context.Context, tx pgx.Tx, sil store.Silence) (*store.Silence, error) {
	s.seq++
	now := time.Now()
	sil.ID = fmt.Sprintf("silence-%d", s.seq)
	sil.CreatedAt, sil.UpdatedAt = now, now
	s.byID[sil.ID] = sil
	out := sil
	return &out, nil
}

func (s *silenceAPISilenceStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Silence, error) {
	sil, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &sil, nil
}

func (s *silenceAPISilenceStore) List(ctx context.Context, tx pgx.Tx, limit int) ([]store.Silence, error) {
	out := make([]store.Silence, 0, len(s.byID))
	for _, sil := range s.byID {
		out = append(out, sil)
	}
	return out, nil
}

func (s *silenceAPISilenceStore) Update(ctx context.Context, tx pgx.Tx, sil store.Silence) error {
	existing, ok := s.byID[sil.ID]
	if !ok {
		return store.ErrNotFound
	}
	sil.CreatedAt, sil.CreatedBy = existing.CreatedAt, existing.CreatedBy
	s.byID[sil.ID] = sil
	return nil
}

func (s *silenceAPISilenceStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := s.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// silenceAPIAlertSilenceStore 模拟 alerts.silenced_by：alertID -> 引用它的静默 ID 集合，
// 供 R10.5 测试断言"删除静默后已静默告警的引用被原子清理"。
type silenceAPIAlertSilenceStore struct {
	silencedBy map[string]map[string]bool
}

func newSilenceAPIAlertSilenceStore() *silenceAPIAlertSilenceStore {
	return &silenceAPIAlertSilenceStore{silencedBy: map[string]map[string]bool{}}
}

func (s *silenceAPIAlertSilenceStore) DetachSilence(ctx context.Context, tx pgx.Tx, silenceID string) error {
	for _, set := range s.silencedBy {
		delete(set, silenceID)
	}
	return nil
}

// ---- cfgFakeChannel：供 R13.6 测试模拟"已注册渠道" ----

type cfgFakeChannel struct{ kind string }

func (c cfgFakeChannel) Kind() string { return c.kind }
func (c cfgFakeChannel) Send(ctx context.Context, target string, msg notify.Message) error {
	return nil
}

// cfgRegisteredChannelKind 是本测试文件专用的渠道 kind，init() 里注册进
// notify 包的全局 registry；用一个不太可能与其他任务/真实渠道撞名的字符串，
// 注册只增不减，不影响其他测试。
const cfgRegisteredChannelKind = "cfg-test-registered-channel"

func init() {
	notify.Register(cfgFakeChannel{kind: cfgRegisteredChannelKind})
}

// ---- 测试装配 helper ----

type cfgTestEnv struct {
	router        *Router
	adminCookie   *http.Cookie
	teams         *cfgTeamStore
	users         *cfgUserStore
	routes        *cfgRouteStore
	schedules     *cfgScheduleStore
	escalations   *cfgEscalationPolicyStore
	notifications *cfgNotificationPolicyStore
	integrations  *cfgIntegrationStore
	silences      *silenceAPISilenceStore
	alertSilences *silenceAPIAlertSilenceStore
}

func newCfgTestEnv(t *testing.T) *cfgTestEnv {
	t.Helper()
	router, _, authUsers, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(authUsers, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	cookie := doLogin(t, router, "admin", "correct-password")

	env := &cfgTestEnv{
		router:        router,
		adminCookie:   cookie,
		teams:         newCfgTeamStore(),
		users:         newCfgUserStore(),
		routes:        newCfgRouteStore(),
		schedules:     newCfgScheduleStore(),
		escalations:   newCfgEscalationPolicyStore(),
		notifications: newCfgNotificationPolicyStore(),
		integrations:  newCfgIntegrationStore(),
		silences:      newSilenceAPISilenceStore(),
		alertSilences: newSilenceAPIAlertSilenceStore(),
	}
	router.RegisterConfigRoutes(ConfigDeps{
		Teams:                env.teams,
		Users:                env.users,
		Routes:               env.routes,
		Schedules:            env.schedules,
		EscalationPolicies:   env.escalations,
		NotificationPolicies: env.notifications,
		Integrations:         env.integrations,
		Silences:             env.silences,
		AlertSilences:        env.alertSilences,
	})
	return env
}

func cfgJSONRequest(method, path string, body any, cookie *http.Cookie) *http.Request {
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

func (env *cfgTestEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, cfgJSONRequest(method, path, body, env.adminCookie))
	return w
}

// ---- 鉴权：非 admin 访问配置管理路由 → 403（与 auth_test.go 的 /admin/ping 用例对照，
// 证明这一整批新端点确实挂在同一条 requireAdmin 中间件链上）----

func TestConfigRoutes_NonAdminAccess_Returns403(t *testing.T) {
	env := newCfgTestEnv(t)
	router, _, authUsers, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(authUsers, "member-1", "member", "correct-password", true)
	memberCookie := doLogin(t, router, "member", "correct-password")
	router.RegisterConfigRoutes(ConfigDeps{
		Teams: env.teams, Users: env.users, Routes: env.routes, Schedules: env.schedules,
		EscalationPolicies: env.escalations, NotificationPolicies: env.notifications,
		Integrations: env.integrations, Silences: env.silences, AlertSilences: env.alertSilences,
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, cfgJSONRequest(http.MethodPost, "/api/v1/admin/teams", map[string]string{"name": "sre"}, memberCookie))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// ---- teams ----

func TestCreateTeam_Success(t *testing.T) {
	env := newCfgTestEnv(t)
	w := env.do(t, http.MethodPost, "/api/v1/admin/teams", map[string]string{"name": "sre"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["name"] != "sre" {
		t.Errorf("name = %v, want sre", resp["name"])
	}
}

func TestCreateTeam_DuplicateName_Returns409(t *testing.T) {
	env := newCfgTestEnv(t)
	env.do(t, http.MethodPost, "/api/v1/admin/teams", map[string]string{"name": "sre"})
	w := env.do(t, http.MethodPost, "/api/v1/admin/teams", map[string]string{"name": "sre"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

// ---- users ----

func TestCreateUser_Success_PasswordNeverInResponse(t *testing.T) {
	env := newCfgTestEnv(t)
	w := env.do(t, http.MethodPost, "/api/v1/admin/users", map[string]string{
		"username": "carol", "password": "s3cret!", "display_name": "Carol",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("s3cret")) {
		t.Errorf("响应体不应包含明文密码: %s", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("password")) {
		t.Errorf("响应体不应包含 password/password_hash 字段: %s", w.Body.String())
	}
}

// ---- integrations（managed hook UUID 只在创建/轮换响应出现）----

func TestCreateIntegration_TokenRevealedOnceThenNeverAgain(t *testing.T) {
	env := newCfgTestEnv(t)
	w := env.do(t, http.MethodPost, "/api/v1/admin/integrations", map[string]any{
		"kind": "webhook", "name": "prod-webhook",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	hookID, _ := created["hook_id"].(string)
	webhookPath, _ := created["webhook_path"].(string)
	if hookID == "" {
		t.Fatalf("创建响应应包含 hook_id，实际: %s", w.Body.String())
	}
	if len(hookID) != 36 || webhookPath != "/ingest/v1/hook/"+hookID {
		t.Fatalf("hook UUID/path 不符合契约: hook_id=%q path=%q", hookID, webhookPath)
	}
	if _, exists := created["token"]; exists {
		t.Fatalf("创建响应不应包含兼容 token 字段: %s", w.Body.String())
	}
	id, _ := created["id"].(string)

	w2 := env.do(t, http.MethodGet, "/api/v1/admin/integrations/"+id, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("查询 status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	if bytes.Contains(w2.Body.Bytes(), []byte(hookID)) {
		t.Errorf("查询响应不应再包含明文 hook UUID: %s", w2.Body.String())
	}
	for _, forbidden := range [][]byte{[]byte("token_hash"), []byte(`"token"`), []byte("hook_id"), []byte("webhook_path")} {
		if bytes.Contains(w2.Body.Bytes(), forbidden) {
			t.Errorf("查询响应泄露接入凭据字段 %q: %s", forbidden, w2.Body.String())
		}
	}
}

func TestRotateAlertSourceHook_InvalidatesStoredCredentialWithoutChangingSourceID(t *testing.T) {
	env := newCfgTestEnv(t)
	createdResp := env.do(t, http.MethodPost, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "prod-alertmanager", "default_severity": "warning",
	})
	if createdResp.Code != http.StatusCreated {
		t.Fatalf("创建 status=%d body=%s", createdResp.Code, createdResp.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createdResp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	oldHook, _ := created["hook_id"].(string)

	rotatedResp := env.do(t, http.MethodPost, "/api/v1/admin/alert-sources/"+id+"/rotate-hook", nil)
	if rotatedResp.Code != http.StatusOK {
		t.Fatalf("轮换 status=%d body=%s", rotatedResp.Code, rotatedResp.Body.String())
	}
	var rotated map[string]any
	if err := json.Unmarshal(rotatedResp.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	newHook, _ := rotated["hook_id"].(string)
	if newHook == "" || newHook == oldHook {
		t.Fatalf("轮换未生成新 hook: old=%q new=%q", oldHook, newHook)
	}
	if rotated["id"] != id || rotated["webhook_path"] != "/ingest/v1/hook/"+newHook {
		t.Fatalf("轮换响应未保持 source 身份或 path 异常: %+v", rotated)
	}
	if _, exists := rotated["token"]; exists {
		t.Fatalf("轮换响应不应包含兼容 token 字段: %+v", rotated)
	}
	stored := env.integrations.byID[id]
	if stored.HookIDHash != hashIntegrationHookID(newHook) || stored.HookIDHash == hashIntegrationHookID(oldHook) {
		t.Fatalf("数据库凭据哈希未原子替换")
	}
}

// ---- routes（priority 全局唯一冲突 → 409；引用的 team 不存在 → 422）----

func TestCreateRoute_PriorityConflict_Returns409(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")

	first := map[string]any{
		"priority": 10, "target_team_id": team.ID,
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", first)
	if w.Code != http.StatusCreated {
		t.Fatalf("第一次创建 status = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	second := map[string]any{
		"priority": 10, "target_team_id": team.ID,
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "staging"}},
	}
	w2 := env.do(t, http.MethodPost, "/api/v1/admin/routes", second)
	if w2.Code != http.StatusConflict {
		t.Fatalf("第二次创建 status = %d, want 409（priority 冲突）; body=%s", w2.Code, w2.Body.String())
	}
}

func TestCreateRoute_TargetTeamNotFound_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	body := map[string]any{
		"priority": 5, "target_team_id": "no-such-team",
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// ---- routes：group_mode/storm_threshold_per_min（R1/R3，C1/C3）----

// 不带 group_mode/storm_threshold_per_min 字段的既有请求：落库结果与引入这两个
// 字段之前逐字节一致——group_mode 缺省 "label"，storm_threshold_per_min 缺省 0。
func TestCreateRoute_DefaultGroupModeAndStormThreshold_BackwardCompatible(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"priority": 1, "target_team_id": team.ID,
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["group_mode"] != "label" {
		t.Errorf("group_mode = %v, want label", resp["group_mode"])
	}
	if resp["storm_threshold_per_min"] != float64(0) {
		t.Errorf("storm_threshold_per_min = %v, want 0", resp["storm_threshold_per_min"])
	}
}

// group_mode=window 且 group_by 为空 → 合法，响应回显 group_mode。
func TestCreateRoute_GroupModeWindow_Success(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"priority": 2, "target_team_id": team.ID, "group_mode": "window",
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["group_mode"] != "window" {
		t.Errorf("group_mode = %v, want window", resp["group_mode"])
	}
}

// group_mode 非法枚举值 → 422。
func TestCreateRoute_GroupModeInvalid_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"priority": 3, "target_team_id": team.ID, "group_mode": "bogus",
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// group_mode=window 与 group_by 非空互斥 → 422。
func TestCreateRoute_GroupModeWindowWithGroupBy_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"priority": 4, "target_team_id": team.ID, "group_mode": "window",
		"group_by": []string{"env"},
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422（group_mode=window 与 group_by 互斥）; body=%s", w.Code, w.Body.String())
	}
}

// storm_threshold_per_min 为负数 → 422。
func TestCreateRoute_StormThresholdNegative_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"priority": 6, "target_team_id": team.ID, "storm_threshold_per_min": -1,
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// group_by 保留键 __template__（R2/C2）：API 层不做 group_by 元素格式校验，
// 与普通 label 键混用同样应当放行（201）。
func TestCreateRoute_GroupByTemplateReservedKey_Success(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"priority": 7, "target_team_id": team.ID,
		"group_by": []string{"__template__", "cluster"},
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/routes", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// ---- escalation policies（Targets 引用的团队必须存在）----

func TestCreateEscalationPolicy_TargetTeamNotFound_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{"no-such-team"}},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateEscalationPolicy_Success(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 0, "targets": []string{team.ID}},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// ---- escalation policies severity_config（任务 4：透传与校验）----

func TestCreateEscalationPolicy_SeverityConfigUnknownOverrideSeverity_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{team.ID}},
		},
		"severity_config": map[string]any{
			"overrides": map[string]any{"bogus": []int{10}},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if len(env.escalations.byID) != 0 {
		t.Fatalf("校验失败不应落库，got %d 条", len(env.escalations.byID))
	}
}

func TestCreateEscalationPolicy_SeverityConfigUnknownSkipSeverity_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{team.ID}},
		},
		"severity_config": map[string]any{
			"skip": []string{"bogus"},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if len(env.escalations.byID) != 0 {
		t.Fatalf("校验失败不应落库，got %d 条", len(env.escalations.byID))
	}
}

func TestCreateEscalationPolicy_SeverityConfigNegativeWaitSeconds_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{team.ID}},
		},
		"severity_config": map[string]any{
			"overrides": map[string]any{"critical": []int{-1}},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if len(env.escalations.byID) != 0 {
		t.Fatalf("校验失败不应落库，got %d 条", len(env.escalations.byID))
	}
}

func TestCreateEscalationPolicy_SeverityConfigOverridesExceedLevels_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{team.ID}}, // 只有 1 级
		},
		"severity_config": map[string]any{
			"overrides": map[string]any{"critical": []int{60, 120}}, // 长度 2 > levels 长度 1
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if len(env.escalations.byID) != 0 {
		t.Fatalf("校验失败不应落库，got %d 条", len(env.escalations.byID))
	}
}

func TestCreateEscalationPolicy_SeverityConfigFullRoundTrip_Returns201(t *testing.T) {
	env := newCfgTestEnv(t)
	team, _ := env.teams.Create(context.Background(), nil, "sre")
	body := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{team.ID}},
			{"wait_seconds": 300, "targets": []string{team.ID}},
		},
		"severity_config": map[string]any{
			"overrides": map[string]any{"critical": []int{180, 600}},
			"skip":      []string{"info"},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/escalation-policies", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created struct {
		ID             string               `json:"id"`
		SeverityConfig store.SeverityConfig `json:"severity_config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析响应体失败: %v; body=%s", err, w.Body.String())
	}
	if len(created.SeverityConfig.Overrides["critical"]) != 2 || created.SeverityConfig.Overrides["critical"][0] != 180 {
		t.Fatalf("响应体 severity_config.overrides 未正确透传: %+v", created.SeverityConfig)
	}
	if len(created.SeverityConfig.Skip) != 1 || created.SeverityConfig.Skip[0] != "info" {
		t.Fatalf("响应体 severity_config.skip 未正确透传: %+v", created.SeverityConfig)
	}

	// 更新端点同样要透传：换一套合法配置，PATCH 后 GET 验证生效（覆盖 update 代码路径）。
	updateBody := map[string]any{
		"team_id": team.ID,
		"levels": []map[string]any{
			{"wait_seconds": 60, "targets": []string{team.ID}},
		},
		"severity_config": map[string]any{
			"overrides": map[string]any{"warning": []int{30}},
		},
	}
	w = env.do(t, http.MethodPatch, "/api/v1/admin/escalation-policies/"+created.ID, updateBody)
	if w.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	w = env.do(t, http.MethodGet, "/api/v1/admin/escalation-policies/"+created.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		SeverityConfig store.SeverityConfig `json:"severity_config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应体失败: %v; body=%s", err, w.Body.String())
	}
	if len(got.SeverityConfig.Overrides["warning"]) != 1 || got.SeverityConfig.Overrides["warning"][0] != 30 {
		t.Fatalf("更新后的 severity_config 未生效: %+v", got.SeverityConfig)
	}
}

// ---- notification policies（R13.6：引用未注册渠道 → 422）----

func TestCreateNotificationPolicy_UnregisteredChannel_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	body := map[string]any{
		"name":     "default-warning",
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
		"steps": map[string]any{
			"warning": []map[string]any{
				{"wait_seconds": 0, "channels": []string{"definitely-not-registered-kind"}, "mode": "replace"},
			},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/notification-policies", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422（R13.6 未注册渠道）; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateNotificationPolicy_RegisteredChannel_Success(t *testing.T) {
	env := newCfgTestEnv(t)
	body := map[string]any{
		"name":     "default-warning",
		"matchers": []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
		"steps": map[string]any{
			"warning": []map[string]any{
				{"wait_seconds": 0, "channels": []string{cfgRegisteredChannelKind}, "mode": "replace"},
			},
		},
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/notification-policies", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// ---- silences（时间窗合法性、matchers 非空；R10.5 提前删除立即恢复通知）----

func TestCreateSilence_EmptyMatchers_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	body := map[string]any{
		"matchers":  []map[string]string{},
		"starts_at": time.Now().Format(time.RFC3339),
		"ends_at":   time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/silences", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422（matchers 不能为空）; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateSilence_InvalidTimeWindow_Returns422(t *testing.T) {
	env := newCfgTestEnv(t)
	now := time.Now()
	body := map[string]any{
		"matchers":  []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
		"starts_at": now.Format(time.RFC3339),
		"ends_at":   now.Add(-time.Hour).Format(time.RFC3339), // ends_at 早于 starts_at
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/silences", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422（时间窗非法）; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateSilence_Success(t *testing.T) {
	env := newCfgTestEnv(t)
	now := time.Now()
	body := map[string]any{
		"matchers":  []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
		"starts_at": now.Format(time.RFC3339),
		"ends_at":   now.Add(time.Hour).Format(time.RFC3339),
		"comment":   "计划内维护",
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/silences", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

func TestDeleteSilence_RestoresAlertNotification_R10_5(t *testing.T) {
	env := newCfgTestEnv(t)
	now := time.Now()
	body := map[string]any{
		"matchers":  []map[string]string{{"name": "env", "op": "=", "value": "prod"}},
		"starts_at": now.Format(time.RFC3339),
		"ends_at":   now.Add(time.Hour).Format(time.RFC3339),
	}
	w := env.do(t, http.MethodPost, "/api/v1/admin/silences", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建静默 status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	silenceID, _ := created["id"].(string)

	// 模拟一条已经被这条静默标记的告警（alerts.silenced_by 里含 silenceID）。
	env.alertSilences.silencedBy["alert-1"] = map[string]bool{silenceID: true}

	wDel := env.do(t, http.MethodDelete, "/api/v1/admin/silences/"+silenceID, nil)
	if wDel.Code != http.StatusNoContent {
		t.Fatalf("删除静默 status = %d, want 204; body=%s", wDel.Code, wDel.Body.String())
	}

	if env.alertSilences.silencedBy["alert-1"][silenceID] {
		t.Errorf("R10.5: 删除静默后，alert-1 的 silenced_by 应已原子移除该静默 ID，实际仍然存在")
	}
	if _, ok := env.silences.byID[silenceID]; ok {
		t.Errorf("删除静默后仓储里不应再查到该静默")
	}
}
