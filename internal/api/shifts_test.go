// 班次管理 + 轮换生成五个端点（serviceability C2，R2）的 httptest 覆盖：CRUD 各状态码、
// 默认时间窗、重叠校验（含 excludeID 自身排除）、dry_run 两态、生成上限透传、非 admin 403。
//
// fakeShiftStore 是 ShiftStore 的内存 fake，重叠判定用与 ShiftRepo.ListOverlapping 同样的
// 半开区间语义（starts_at < to AND ends_at > from）手写实现，而不是直接调用 store 包——
// internal/api 测试历来不依赖真实 DB/testcontainers（见 auth_test.go/ops_settings_test.go
// 的既有惯例），只有 internal/store 的集成测试才连真实 Postgres。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

// ---- 内存 fake：ShiftStore ----

type fakeShiftStore struct {
	byID map[string]store.Shift
	seq  int
}

func newFakeShiftStore() *fakeShiftStore {
	return &fakeShiftStore{byID: map[string]store.Shift{}}
}

func (f *fakeShiftStore) Create(ctx context.Context, tx pgx.Tx, s store.Shift) (*store.Shift, error) {
	f.seq++
	now := time.Now()
	s.ID = fmt.Sprintf("shift-%d", f.seq)
	s.CreatedAt, s.UpdatedAt = now, now
	f.byID[s.ID] = s
	out := s
	return &out, nil
}

func (f *fakeShiftStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (*store.Shift, error) {
	s, ok := f.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &s, nil
}

func (f *fakeShiftStore) ListInWindow(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, limit int) ([]store.Shift, error) {
	var out []store.Shift
	for _, s := range f.byID {
		if s.ScheduleID == scheduleID && s.StartsAt.Before(to) && s.EndsAt.After(from) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out, nil
}

func (f *fakeShiftStore) Update(ctx context.Context, tx pgx.Tx, s store.Shift) error {
	existing, ok := f.byID[s.ID]
	if !ok {
		return store.ErrNotFound
	}
	s.CreatedAt = existing.CreatedAt
	s.UpdatedAt = time.Now()
	f.byID[s.ID] = s
	return nil
}

func (f *fakeShiftStore) Delete(ctx context.Context, tx pgx.Tx, id string) error {
	if _, ok := f.byID[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

// ListOverlapping 语义同 store.ShiftRepo.ListOverlapping：半开区间 [from, to)，严格相邻
// 不算重叠；excludeID 非空时排除自身（编辑现有班次场景）。
func (f *fakeShiftStore) ListOverlapping(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time, excludeID string) ([]store.Shift, error) {
	var out []store.Shift
	for _, s := range f.byID {
		if s.ScheduleID != scheduleID {
			continue
		}
		if excludeID != "" && s.ID == excludeID {
			continue
		}
		if s.StartsAt.Before(to) && s.EndsAt.After(from) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out, nil
}

// ---- 测试装配 helper：admin only，故只需 admin cookie；额外准备 member cookie 覆盖
// 403 场景。schedules/users 复用 config_test.go 已有的 cfgScheduleStore/cfgUserStore
// （同包，结构上天然满足 ShiftScheduleStore/ShiftUserStore），不重复定义。----

type shiftTestEnv struct {
	router       *Router
	adminCookie  *http.Cookie
	memberCookie *http.Cookie
	schedules    *cfgScheduleStore
	users        *cfgUserStore
	shifts       *fakeShiftStore
}

func newShiftTestEnv(t *testing.T) *shiftTestEnv {
	t.Helper()
	router, _, authUsers, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(authUsers, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	adminCookie := doLogin(t, router, "admin", "correct-password")

	addTestUser(authUsers, "member-1", "member", "correct-password", true)
	memberCookie := doLogin(t, router, "member", "correct-password")

	env := &shiftTestEnv{
		router:       router,
		adminCookie:  adminCookie,
		memberCookie: memberCookie,
		schedules:    newCfgScheduleStore(),
		users:        newCfgUserStore(),
		shifts:       newFakeShiftStore(),
	}
	router.RegisterShiftRoutes(env.schedules, env.users, env.shifts)
	return env
}

func (env *shiftTestEnv) createSchedule(t *testing.T) string {
	t.Helper()
	sch, err := env.schedules.Create(context.Background(), nil, store.Schedule{TeamID: "team-1", Name: "primary", Timezone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("创建值班表失败: %v", err)
	}
	return sch.ID
}

func (env *shiftTestEnv) createUser(t *testing.T, username string) string {
	t.Helper()
	u, err := env.users.Create(context.Background(), nil, store.User{Username: username, DisplayName: username, PasswordHash: "x"})
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return u.ID
}

func (env *shiftTestEnv) do(method string, cookie *http.Cookie, path string, body any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, opsRequest(method, path, body, cookie))
	return w
}

func shiftBody(userID string, startsAt, endsAt time.Time) map[string]any {
	return map[string]any{
		"user_id":   userID,
		"starts_at": startsAt.Format(time.RFC3339),
		"ends_at":   endsAt.Format(time.RFC3339),
	}
}

// ---- 非 admin 403 ----

func TestShiftRoutes_NonAdmin_Returns403(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	w := env.do(http.MethodGet, env.memberCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// ---- GET 列表 ----

func TestShiftList_DefaultWindow_FiltersOutsideRange(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")

	now := time.Now()
	inWindow, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("准备窗口内班次失败: %v", err)
	}
	if _, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now.Add(200 * 24 * time.Hour), EndsAt: now.Add(201 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("准备窗口外班次失败: %v", err)
	}

	w := env.do(http.MethodGet, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShiftsResp(t, w)
	if len(resp) != 1 {
		t.Fatalf("默认窗口内班次数 = %d, want 1（body=%s）", len(resp), w.Body.String())
	}
	if resp[0]["id"] != inWindow.ID {
		t.Errorf("返回的班次 id = %v, want %s", resp[0]["id"], inWindow.ID)
	}
	if resp[0]["user_display_name"] != "alice" {
		t.Errorf("user_display_name = %v, want alice", resp[0]["user_display_name"])
	}
}

func TestShiftList_MalformedFrom_Returns400(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	w := env.do(http.MethodGet, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts?from=not-a-date", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestShiftList_ScheduleNotFound_Returns404(t *testing.T) {
	env := newShiftTestEnv(t)
	w := env.do(http.MethodGet, env.adminCookie, "/api/v1/admin/schedules/no-such-schedule/shifts", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ---- POST 创建 ----

func TestShiftCreate_Success_Returns201(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts",
		shiftBody(userID, now, now.Add(time.Hour)))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShiftResp(t, w)
	if resp["id"] == nil || resp["id"] == "" {
		t.Errorf("响应缺少 id: %v", resp)
	}
	if resp["user_display_name"] != "alice" {
		t.Errorf("user_display_name = %v, want alice", resp["user_display_name"])
	}
}

func TestShiftCreate_EndBeforeStart_Returns422(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts",
		shiftBody(userID, now, now.Add(-time.Hour)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestShiftCreate_UserNotFound_Returns404(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts",
		shiftBody("no-such-user", now, now.Add(time.Hour)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestShiftCreate_ScheduleNotFound_Returns404(t *testing.T) {
	env := newShiftTestEnv(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/no-such-schedule/shifts",
		shiftBody(userID, now, now.Add(time.Hour)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestShiftCreate_Conflict_Returns422WithDetails(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	existing, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now, EndsAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("准备既有班次失败: %v", err)
	}

	// 与既有班次重叠（既有 [now, now+2h)，新请求 [now+1h, now+3h) 有交集）。
	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts",
		shiftBody(userID, now.Add(time.Hour), now.Add(3*time.Hour)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	resp := decodeAny(t, w)
	details, ok := resp["details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("details = %v, want 1 条冲突", resp["details"])
	}
	d := details[0].(map[string]any)
	if d["shift_id"] != existing.ID {
		t.Errorf("details[0].shift_id = %v, want %s", d["shift_id"], existing.ID)
	}
	if d["user_display_name"] != "alice" {
		t.Errorf("details[0].user_display_name = %v, want alice", d["user_display_name"])
	}
}

// ---- PATCH 编辑 ----

func TestShiftUpdate_Success_Returns200(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	other := env.createUser(t, "bob")
	now := time.Now()

	existing, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now, EndsAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("准备班次失败: %v", err)
	}

	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/"+existing.ID,
		shiftBody(other, now.Add(5*time.Hour), now.Add(6*time.Hour)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShiftResp(t, w)
	if resp["user_display_name"] != "bob" {
		t.Errorf("user_display_name = %v, want bob", resp["user_display_name"])
	}
}

// TestShiftUpdate_SameWindow_NoFalseConflict 验证编辑班次自身时段（原样或轻微调整但仍与
// 自己重叠）不会被误判为冲突——excludeID 生效的核心断言。
func TestShiftUpdate_SameWindow_NoFalseConflict(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	existing, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now, EndsAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("准备班次失败: %v", err)
	}

	// 把结束时间往后挪半小时：新区间 [now, now+2.5h) 与旧区间自身高度重叠，若 excludeID
	// 不生效就会命中自己触发假阳性 422。
	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/"+existing.ID,
		shiftBody(userID, now, now.Add(150*time.Minute)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（excludeID 应排除自身）; body=%s", w.Code, w.Body.String())
	}
}

func TestShiftUpdate_ConflictWithOther_Returns422(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	if _, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now, EndsAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("准备班次 A 失败: %v", err)
	}
	shiftB, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now.Add(5 * time.Hour), EndsAt: now.Add(6 * time.Hour),
	})
	if err != nil {
		t.Fatalf("准备班次 B 失败: %v", err)
	}

	// 把 B 的时间挪去和 A 重叠。
	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/"+shiftB.ID,
		shiftBody(userID, now.Add(30*time.Minute), now.Add(90*time.Minute)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestShiftUpdate_WrongSchedule_Returns404(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleA := env.createSchedule(t)
	scheduleB := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	existing, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleA, UserID: userID, StartsAt: now, EndsAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("准备班次失败: %v", err)
	}

	w := env.do(http.MethodPatch, env.adminCookie, "/api/v1/admin/schedules/"+scheduleB+"/shifts/"+existing.ID,
		shiftBody(userID, now, now.Add(time.Hour)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（班次不属于该 schedule）; body=%s", w.Code, w.Body.String())
	}
}

// ---- DELETE ----

func TestShiftDelete_Success_Returns204(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	userID := env.createUser(t, "alice")
	now := time.Now()

	existing, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: userID, StartsAt: now, EndsAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("准备班次失败: %v", err)
	}

	w := env.do(http.MethodDelete, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/"+existing.ID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if _, ok := env.shifts.byID[existing.ID]; ok {
		t.Errorf("班次删除后仍存在于仓储")
	}
}

func TestShiftDelete_NotFound_Returns404(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)

	w := env.do(http.MethodDelete, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/no-such-shift", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ---- POST 生成轮换 ----

func generateBody(userIDs []string, shiftDurationSeconds int, startsAt time.Time, rounds int, dryRun bool) map[string]any {
	return map[string]any{
		"user_ids":               userIDs,
		"shift_duration_seconds": shiftDurationSeconds,
		"starts_at":              startsAt.Format(time.RFC3339),
		"rounds":                 rounds,
		"dry_run":                dryRun,
	}
}

func TestShiftGenerate_DryRun_DoesNotPersist(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	alice := env.createUser(t, "alice")
	bob := env.createUser(t, "bob")
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate",
		generateBody([]string{alice, bob}, 3600, now, 2, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShiftsResp(t, w)
	if len(resp) != 4 {
		t.Fatalf("预览班次数 = %d, want 4（2 轮 x 2 人）", len(resp))
	}
	for _, item := range resp {
		if _, hasID := item["id"]; hasID {
			t.Errorf("dry_run 预览响应不应包含 id 字段: %v", item)
		}
	}
	if len(env.shifts.byID) != 0 {
		t.Errorf("dry_run=true 不应落库，实际已落库 %d 条", len(env.shifts.byID))
	}

	// 复查：随后再次以相同参数 dry_run=true 请求应得到完全一样的预览（证明确未落库、
	// 不会因为第一次请求副作用改变第二次结果）。
	w2 := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate",
		generateBody([]string{alice, bob}, 3600, now, 2, true))
	if w2.Code != http.StatusOK {
		t.Fatalf("复查请求 status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	resp2 := decodeShiftsResp(t, w2)
	if len(resp2) != 4 {
		t.Fatalf("复查预览班次数 = %d, want 4", len(resp2))
	}
}

func TestShiftGenerate_Persist_CreatesRows(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	alice := env.createUser(t, "alice")
	bob := env.createUser(t, "bob")
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate",
		generateBody([]string{alice, bob}, 3600, now, 2, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShiftsResp(t, w)
	if len(resp) != 4 {
		t.Fatalf("落库响应班次数 = %d, want 4", len(resp))
	}
	for _, item := range resp {
		if item["id"] == nil || item["id"] == "" {
			t.Errorf("dry_run=false 响应应包含 id 字段: %v", item)
		}
	}
	if len(env.shifts.byID) != 4 {
		t.Fatalf("落库后仓储行数 = %d, want 4", len(env.shifts.byID))
	}
}

func TestShiftGenerate_Conflict_Returns422Details(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	alice := env.createUser(t, "alice")
	now := time.Now()

	// 生成集覆盖 [now, now+2h)（1 轮 x 1 人 x 1h），预置一条与之重叠的既有班次。
	existing, err := env.shifts.Create(context.Background(), nil, store.Shift{
		ScheduleID: scheduleID, UserID: alice, StartsAt: now.Add(30 * time.Minute), EndsAt: now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("准备既有班次失败: %v", err)
	}

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate",
		generateBody([]string{alice}, 3600, now, 2, false))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	resp := decodeAny(t, w)
	details, ok := resp["details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("details = %v, want 1 条冲突", resp["details"])
	}
	d := details[0].(map[string]any)
	if d["shift_id"] != existing.ID {
		t.Errorf("details[0].shift_id = %v, want %s", d["shift_id"], existing.ID)
	}
	// 冲突时整体不落库，不做部分落库。
	if len(env.shifts.byID) != 1 {
		t.Errorf("冲突后仓储行数 = %d, want 1（只有预置的那条，未追加任何新生成班次）", len(env.shifts.byID))
	}
}

func TestShiftGenerate_ExceedsCap_Returns422Passthrough(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	alice := env.createUser(t, "alice")
	now := time.Now()

	// rounds x len(userIDs) = 1001 > oncall.maxRotationShifts(1000)，GenerateRotation
	// 应在事务外就直接拒绝（未调用任何 store 方法），错误文案透传。
	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate",
		generateBody([]string{alice}, 3600, now, 1001, false))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	resp := decodeAny(t, w)
	msg, _ := resp["error"].(string)
	if msg == "" {
		t.Fatalf("响应缺少 error 文案: %v", resp)
	}
}

func TestShiftGenerate_EmptyUserIDs_Returns422Passthrough(t *testing.T) {
	env := newShiftTestEnv(t)
	scheduleID := env.createSchedule(t)
	now := time.Now()

	w := env.do(http.MethodPost, env.adminCookie, "/api/v1/admin/schedules/"+scheduleID+"/shifts/generate",
		generateBody(nil, 3600, now, 1, false))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// ---- 响应解析 helper ----

func decodeAny(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, w.Body.String())
	}
	return resp
}

func decodeShiftResp(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decodeAny(t, w)
}

func decodeShiftsResp(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var resp struct {
		Shifts []map[string]any `json:"shifts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, w.Body.String())
	}
	return resp.Shifts
}
