package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// ---- 内存 fake：AlertsStore/AlertTimelineStore/ChangeEventLister/
// EscalationStopper/LifecycleTimerCanceler。命名统一加 alertsFake 前缀，避免与
// auth_test.go 既有 fake（fakeUserStore 等）撞名——那些是只读复用，不在本文件修改。

type alertsFakeStore struct {
	alerts map[string]domain.Alert
}

func (f *alertsFakeStore) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Alert, error) {
	a, ok := f.alerts[id]
	if !ok {
		return domain.Alert{}, store.ErrAlertNotFound
	}
	return a, nil
}

func (f *alertsFakeStore) UpdateStatus(ctx context.Context, tx pgx.Tx, alert domain.Alert) error {
	if _, ok := f.alerts[alert.ID]; !ok {
		return store.ErrAlertNotFound
	}
	f.alerts[alert.ID] = alert
	return nil
}

// List 实现与 store.AlertRepo.List 相同的过滤语义（severity/status/team_id/labels
// 全部 AND，按 LastSeenAt 倒序 + limit/offset），纯内存实现，不需要 Docker。
func (f *alertsFakeStore) List(ctx context.Context, tx pgx.Tx, filter store.AlertFilter) (store.AlertPage, error) {
	var matched []domain.Alert
	for _, a := range f.alerts {
		if filter.Severity != "" && a.Severity != filter.Severity {
			continue
		}
		if filter.Status != "" && a.Status != filter.Status {
			continue
		}
		if filter.TeamID != "" && a.TeamID != filter.TeamID {
			continue
		}
		if len(filter.Labels) > 0 {
			ok := true
			for k, v := range filter.Labels {
				if a.Labels[k] != v {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		matched = append(matched, a)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].LastSeenAt.After(matched[j].LastSeenAt) })

	total := len(matched)
	limit := filter.Limit
	if limit <= 0 {
		limit = store.DefaultAlertPageLimit
	}
	offset := filter.Offset
	if offset > len(matched) {
		offset = len(matched)
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return store.AlertPage{Alerts: matched[offset:end], Total: total}, nil
}

type alertsFakeTimeline struct {
	entries map[string][]domain.TimelineEntry
}

func (f *alertsFakeTimeline) Append(ctx context.Context, tx pgx.Tx, entry domain.TimelineEntry) (string, error) {
	f.entries[entry.AlertID] = append(f.entries[entry.AlertID], entry)
	return fmt.Sprintf("tl-%d", len(f.entries[entry.AlertID])), nil
}

func (f *alertsFakeTimeline) ListByAlert(ctx context.Context, tx pgx.Tx, alertID string) ([]domain.TimelineEntry, error) {
	return f.entries[alertID], nil
}

type alertsFakeChangeEvents struct {
	events []store.ChangeEvent
}

func (f *alertsFakeChangeEvents) ListRecentByResource(ctx context.Context, tx pgx.Tx, resourceRef map[string]string, since time.Time, limit int) ([]store.ChangeEvent, error) {
	return f.events, nil
}

type alertsFakeEscalation struct {
	stopped []string
}

func (f *alertsFakeEscalation) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	f.stopped = append(f.stopped, alertID)
	return nil
}

type alertsFakeLifecycle struct {
	canceled []string
}

func (f *alertsFakeLifecycle) CancelLifecycleTimers(ctx context.Context, tx pgx.Tx, alertID string) error {
	f.canceled = append(f.canceled, alertID)
	return nil
}

// ---- 测试装配 helper ----

// newAlertsTestRouter 装配一个已登录（非 admin 的普通 member）会话，验证"处置动作
// member 即可执行、不要求 admin"（R8.2 的裁决）。返回值供各测试按需断言 fake 内部
// 状态（如 escalation.stopped）。
func newAlertsTestRouter(t *testing.T) (router *Router, alerts *alertsFakeStore, timeline *alertsFakeTimeline, escalation *alertsFakeEscalation, lifecycle *alertsFakeLifecycle, cookie *http.Cookie) {
	t.Helper()
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "member1", "correct-password", true)
	cookie = doLogin(t, router, "member1", "correct-password")

	alerts = &alertsFakeStore{alerts: map[string]domain.Alert{}}
	timeline = &alertsFakeTimeline{entries: map[string][]domain.TimelineEntry{}}
	escalation = &alertsFakeEscalation{}
	lifecycle = &alertsFakeLifecycle{}
	router.RegisterAlertRoutes(alerts, timeline, nil, escalation, lifecycle)
	return router, alerts, timeline, escalation, lifecycle, cookie
}

func doAlertsRequest(router *Router, method, path string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
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
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// ---- 测试矩阵 ----

// 未登录访问列表/详情/处置动作 → 401（R8.3 之外的常规 session 保护，与 auth_test.go
// 的 401 矩阵是同一条中间件链路，这里只补齐 alerts 路由本身确实挂了这条中间件）。
func TestAlertsRoutes_Unauthenticated_Returns401(t *testing.T) {
	router, _, _, _, _, _ := newAlertsTestRouter(t)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/alerts"},
		{http.MethodGet, "/api/v1/alerts/a1"},
		{http.MethodPost, "/api/v1/alerts/a1/ack"},
		{http.MethodPost, "/api/v1/alerts/a1/resolve"},
		{http.MethodPost, "/api/v1/alerts/a1/handoff"},
	}
	for _, tc := range cases {
		w := doAlertsRequest(router, tc.method, tc.path, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}

// 列表过滤组合：severity + team_id + label 全部命中才算匹配（AND 语义），未命中的
// 维度不受影响；分页 total 反映过滤后的全量而不是当前页大小。
func TestAlertsList_FilterCombination(t *testing.T) {
	router, alerts, _, _, _, cookie := newAlertsTestRouter(t)
	now := time.Now()

	const (
		team1 = "11111111-1111-1111-1111-111111111111"
		team2 = "22222222-2222-2222-2222-222222222222"
	)
	alerts.alerts["a1"] = domain.Alert{
		ID: "a1", Status: domain.StatusFiring, Severity: domain.SeverityCritical,
		TeamID: team1, Labels: map[string]string{"env": "prod"}, LastSeenAt: now,
	}
	alerts.alerts["a2"] = domain.Alert{
		ID: "a2", Status: domain.StatusFiring, Severity: domain.SeverityCritical,
		TeamID: team2, Labels: map[string]string{"env": "prod"}, LastSeenAt: now.Add(time.Second),
	}
	alerts.alerts["a3"] = domain.Alert{
		ID: "a3", Status: domain.StatusResolved, Severity: domain.SeverityCritical,
		TeamID: team1, Labels: map[string]string{"env": "staging"}, LastSeenAt: now.Add(2 * time.Second),
	}

	w := doAlertsRequest(router, http.MethodGet, "/api/v1/alerts?severity=critical&status=firing&team_id="+team1+"&label[env]=prod", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Alerts []alertResponse `json:"alerts"`
		Total  int             `json:"total"`
		Limit  int             `json:"limit"`
		Offset int             `json:"offset"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 1 || len(resp.Alerts) != 1 || resp.Alerts[0].ID != "a1" {
		t.Fatalf("过滤结果 = %+v, want 仅 a1", resp)
	}
	if resp.Limit != store.DefaultAlertPageLimit {
		t.Errorf("limit = %d, want 默认值 %d", resp.Limit, store.DefaultAlertPageLimit)
	}
}

// 非法 severity 取值 → 400，而不是把无效过滤条件当成"不过滤"悄悄放过。
func TestAlertsList_InvalidSeverity_Returns400(t *testing.T) {
	router, _, _, _, _, cookie := newAlertsTestRouter(t)
	w := doAlertsRequest(router, http.MethodGet, "/api/v1/alerts?severity=not-a-severity", cookie, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// 详情响应包含告警全字段、完整时间线与"最近变更"（R7.5）。
func TestAlertsDetail_IncludesTimelineAndRecentChanges(t *testing.T) {
	router, alerts, timeline, _, _, cookie := newAlertsTestRouter(t)

	alerts.alerts["a1"] = domain.Alert{
		ID: "a1", Status: domain.StatusFiring, Severity: domain.SeverityWarning,
		ResourceRef: map[string]string{"service.name": "checkout"},
		StartsAt:    time.Now(), LastSeenAt: time.Now(),
	}
	timeline.entries["a1"] = []domain.TimelineEntry{
		{AlertID: "a1", Actor: "system", Action: domain.TimelineActionNotify, At: time.Now()},
	}
	// 覆盖详情 handler 内部构造的 changeEvents 依赖：newAlertsTestRouter 传的是 nil，
	// 这里单独验证"changeEvents 为 nil 时 recent_changes 恒为空列表"这一支路，
	// 非 nil 的场景在下面的 TestAlertsDetail_RecentChanges_QueriesByResourceRef 里覆盖。

	w := doAlertsRequest(router, http.MethodGet, "/api/v1/alerts/a1", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Alert         alertResponse           `json:"alert"`
		Timeline      []timelineEntryResponse `json:"timeline"`
		RecentChanges []changeEventResponse   `json:"recent_changes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Alert.ID != "a1" {
		t.Errorf("alert.id = %q, want a1", resp.Alert.ID)
	}
	if len(resp.Timeline) != 1 || resp.Timeline[0].Action != domain.TimelineActionNotify {
		t.Errorf("timeline = %+v, want 1 条 notify", resp.Timeline)
	}
	if resp.RecentChanges == nil || len(resp.RecentChanges) != 0 {
		t.Errorf("recent_changes = %+v, want 空列表（changeEvents 未装配）", resp.RecentChanges)
	}
}

// changeEvents 已装配、告警 ResourceRef 非空时，详情响应带出最近变更列表。这里不复用
// newAlertsTestRouter（它注册的路由未装配 changeEvents），直接另起一个 router 实例。
func TestAlertsDetail_RecentChanges_QueriesByResourceRef(t *testing.T) {
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "member2", "correct-password", true)
	cookie := doLogin(t, router, "member2", "correct-password")

	alerts := &alertsFakeStore{alerts: map[string]domain.Alert{
		"a1": {
			ID: "a1", Status: domain.StatusFiring,
			ResourceRef: map[string]string{"service.name": "checkout"},
			StartsAt:    time.Now(), LastSeenAt: time.Now(),
		},
	}}
	timeline := &alertsFakeTimeline{entries: map[string][]domain.TimelineEntry{}}
	changeEvents := &alertsFakeChangeEvents{events: []store.ChangeEvent{
		{ID: "c1", Kind: "deploy", ResourceRef: map[string]string{"service.name": "checkout"}, OccurredAt: time.Now()},
	}}
	router.RegisterAlertRoutes(alerts, timeline, changeEvents, &alertsFakeEscalation{}, &alertsFakeLifecycle{})

	w := doAlertsRequest(router, http.MethodGet, "/api/v1/alerts/a1", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		RecentChanges []changeEventResponse `json:"recent_changes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.RecentChanges) != 1 || resp.RecentChanges[0].ID != "c1" {
		t.Fatalf("recent_changes = %+v, want 1 条 c1", resp.RecentChanges)
	}
}

// 详情端点：告警不存在 → 404。
func TestAlertsDetail_NotFound_Returns404(t *testing.T) {
	router, _, _, _, _, cookie := newAlertsTestRouter(t)
	w := doAlertsRequest(router, http.MethodGet, "/api/v1/alerts/no-such-id", cookie, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// Ack：firing -> acknowledged，写时间线、actor 取会话用户名，停止升级链定时器。
// member（非 admin）即可成功执行（R8.2 裁决：处置动作不要求 admin）。
func TestAlertsAck_Firing_TransitionsAndStopsEscalation(t *testing.T) {
	router, alerts, timeline, escalation, _, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusFiring, StartsAt: time.Now()}

	w := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/ack", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Alert alertResponse `json:"alert"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Alert.Status != domain.StatusAcknowledged {
		t.Errorf("status = %s, want acknowledged", resp.Alert.Status)
	}
	if resp.Alert.AckedBy != "u1" {
		t.Errorf("acked_by = %q, want u1（actor 取会话用户 ID，进 UUID 列）", resp.Alert.AckedBy)
	}
	if len(timeline.entries["a1"]) != 1 || timeline.entries["a1"][0].Action != domain.TimelineActionAck {
		t.Errorf("timeline = %+v, want 1 条 ack", timeline.entries["a1"])
	}
	if timeline.entries["a1"][0].Actor != "member1" {
		t.Errorf("timeline actor = %q, want member1（展示用取会话用户名）", timeline.entries["a1"][0].Actor)
	}
	if len(escalation.stopped) != 1 || escalation.stopped[0] != "a1" {
		t.Errorf("escalation.stopped = %v, want [a1]", escalation.stopped)
	}
}

// 重复 Ack 幂等成功：已经 acknowledged 的告警再次 Ack 返回 200 与当前状态，不报
// 409，也不追加第二条时间线；escalation.Stop 仍然会被再调用一次（幂等，防御性
// 保证"Ack 之后一定无残留升级定时器"这一不变量）。
func TestAlertsAck_AlreadyAcked_IdempotentSuccess(t *testing.T) {
	router, alerts, timeline, escalation, _, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusFiring, StartsAt: time.Now()}

	w1 := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/ack", cookie, nil)
	if w1.Code != http.StatusOK {
		t.Fatalf("第一次 ack status = %d, want 200", w1.Code)
	}
	w2 := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/ack", cookie, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("第二次 ack status = %d, want 200（幂等成功，不报 409）", w2.Code)
	}
	if len(timeline.entries["a1"]) != 1 {
		t.Errorf("重复 ack 不应追加新时间线，实际 %d 条", len(timeline.entries["a1"]))
	}
	if len(escalation.stopped) != 2 {
		t.Errorf("escalation.Stop 应该每次 ack 调用都执行（幂等），实际调用 %d 次", len(escalation.stopped))
	}
}

// 已 resolved 的告警再次 Ack 同样幂等成功：不报 409，返回 resolved 状态不变。
func TestAlertsAck_OnResolvedAlert_IdempotentSuccess(t *testing.T) {
	router, alerts, _, _, _, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusResolved, StartsAt: time.Now()}

	w := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/ack", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（对 resolved 告警 ack 也幂等成功）", w.Code)
	}
	var resp struct {
		Alert alertResponse `json:"alert"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Alert.Status != domain.StatusResolved {
		t.Errorf("status = %s, want resolved（保持不变）", resp.Alert.Status)
	}
}

// Resolve：firing -> resolved，停止升级链定时器与生命周期定时器（TTL/repeat）。
func TestAlertsResolve_TransitionsAndCancelsLifecycleTimers(t *testing.T) {
	router, alerts, timeline, escalation, lifecycle, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusFiring, StartsAt: time.Now()}

	w := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/resolve", cookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Alert alertResponse `json:"alert"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Alert.Status != domain.StatusResolved {
		t.Errorf("status = %s, want resolved", resp.Alert.Status)
	}
	if len(timeline.entries["a1"]) != 1 || timeline.entries["a1"][0].Action != domain.TimelineActionResolve {
		t.Errorf("timeline = %+v, want 1 条 resolve", timeline.entries["a1"])
	}
	if len(escalation.stopped) != 1 {
		t.Errorf("escalation.stopped = %v, want 恰好调用 1 次", escalation.stopped)
	}
	if len(lifecycle.canceled) != 1 || lifecycle.canceled[0] != "a1" {
		t.Errorf("lifecycle.canceled = %v, want [a1]", lifecycle.canceled)
	}
}

// 重复 Resolve 幂等成功：已经 resolved 的告警再次 Resolve 返回 200 当前状态，不报
// 409，不追加第二条时间线。
func TestAlertsResolve_AlreadyResolved_IdempotentSuccess(t *testing.T) {
	router, alerts, timeline, _, lifecycle, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusFiring, StartsAt: time.Now()}

	w1 := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/resolve", cookie, nil)
	if w1.Code != http.StatusOK {
		t.Fatalf("第一次 resolve status = %d, want 200", w1.Code)
	}
	w2 := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/resolve", cookie, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("第二次 resolve status = %d, want 200（幂等成功，不报 409）", w2.Code)
	}
	if len(timeline.entries["a1"]) != 1 {
		t.Errorf("重复 resolve 不应追加新时间线，实际 %d 条", len(timeline.entries["a1"]))
	}
	if len(lifecycle.canceled) != 2 {
		t.Errorf("CancelLifecycleTimers 应该每次 resolve 调用都执行（幂等），实际调用 %d 次", len(lifecycle.canceled))
	}
}

// Handoff：转移责任人（AckedBy），不改变 Status；actor/target 都落时间线详情。
func TestAlertsHandoff_TransfersAckedBy(t *testing.T) {
	router, alerts, timeline, _, _, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusFiring, AckedBy: "old-owner", StartsAt: time.Now()}

	w := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/handoff", cookie, map[string]string{"target": "new-owner"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Alert alertResponse `json:"alert"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Alert.AckedBy != "new-owner" {
		t.Errorf("acked_by = %q, want new-owner", resp.Alert.AckedBy)
	}
	if resp.Alert.Status != domain.StatusFiring {
		t.Errorf("status = %s, want firing（handoff 不改变状态）", resp.Alert.Status)
	}
	if len(timeline.entries["a1"]) != 1 || timeline.entries["a1"][0].Action != domain.TimelineActionHandoff {
		t.Errorf("timeline = %+v, want 1 条 handoff", timeline.entries["a1"])
	}
}

// target 缺失 → 400（请求参数错误，不属于状态冲突）。
func TestAlertsHandoff_MissingTarget_Returns400(t *testing.T) {
	router, alerts, _, _, _, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusFiring, StartsAt: time.Now()}

	w := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/handoff", cookie, map[string]string{"target": ""})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// 对已 resolved 的告警执行 handoff 是真正的非法操作（不是"重复同一动作"）→ 409。
func TestAlertsHandoff_OnResolvedAlert_Returns409(t *testing.T) {
	router, alerts, _, _, _, cookie := newAlertsTestRouter(t)
	alerts.alerts["a1"] = domain.Alert{ID: "a1", Status: domain.StatusResolved, StartsAt: time.Now()}

	w := doAlertsRequest(router, http.MethodPost, "/api/v1/alerts/a1/handoff", cookie, map[string]string{"target": "someone"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}
