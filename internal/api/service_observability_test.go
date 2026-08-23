// Service Observability 三个只读聚合 API 的 HTTP 边界覆盖。
//
// 这里测的是边界本身：鉴权、参数白名单、typed 错误到状态码的映射、响应不泄露内部细节、
// 请求取消是否传导、以及既有 /api/v1/services 路由不被影响。业务编排与 Health/Data 判定
// 由 internal/serviceobs/facade 与 internal/serviceobs/health 的测试覆盖。
//
// 契约 example 只在测试里出现：它们提供 schema-valid 的 Page View，用来证明 HTTP 层
// 不会把形状改坏（例如把空数组序列化成 null）。live 路径不读任何 fixture。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/facade"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/view"
)

const serviceObservabilityExamplesDir = "../../docs/contracts/service-observability/v1/examples"

// 编译期保证 facade 就是本层声明的接口形状：装配时不需要额外适配器。
var _ PageViewService = (*facade.Service)(nil)

// fakePageViews 记录收到的查询并返回注入结果。它让"参数如何被解析、actor 从哪里来、
// 取消是否传导"都可以被断言。
type fakePageViews struct {
	directory     model.DirectoryView
	overview      model.OverviewView
	workspace     model.WorkspaceView
	directoryErr  error
	overviewErr   error
	workspaceErr  error
	directoryCall *facade.DirectoryQuery
	overviewCall  *facade.OverviewQuery
	workspaceCall *facade.WorkspaceQuery
	sawCanceled   bool
	calls         int
}

func (f *fakePageViews) Directory(ctx context.Context, query facade.DirectoryQuery) (model.DirectoryView, error) {
	f.calls++
	f.directoryCall = &query
	if err := ctx.Err(); err != nil {
		f.sawCanceled = true
		return model.DirectoryView{}, err
	}
	return f.directory, f.directoryErr
}

func (f *fakePageViews) Overview(ctx context.Context, query facade.OverviewQuery) (model.OverviewView, error) {
	f.calls++
	f.overviewCall = &query
	if err := ctx.Err(); err != nil {
		f.sawCanceled = true
		return model.OverviewView{}, err
	}
	return f.overview, f.overviewErr
}

func (f *fakePageViews) Workspace(ctx context.Context, query facade.WorkspaceQuery) (model.WorkspaceView, error) {
	f.calls++
	f.workspaceCall = &query
	if err := ctx.Err(); err != nil {
		f.sawCanceled = true
		return model.WorkspaceView{}, err
	}
	return f.workspace, f.workspaceErr
}

// newServiceObservabilityRouter 装配一个只挂载三个聚合端点的 Router，并返回一个已登录
// 的 member cookie（不持有任何团队 admin 角色）。
func newServiceObservabilityRouter(t *testing.T) (*Router, *fakePageViews, *http.Cookie) {
	t.Helper()
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u-observer", "observer", "correct-password", true)
	pageViews := &fakePageViews{
		directory: loadExampleDirectory(t),
		overview:  loadExampleOverview(t),
		workspace: loadExampleWorkspace(t),
	}
	router.RegisterServiceObservabilityRoutes(pageViews)
	cookie := doLogin(t, router, "observer", "correct-password")
	return router, pageViews, cookie
}

func loadExample[T any](t *testing.T, filename string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(serviceObservabilityExamplesDir, filename))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", filename, err)
	}
	var target T
	if err := json.Unmarshal(raw, &target); err != nil {
		t.Fatalf("解析 %s 失败: %v", filename, err)
	}
	return target
}

func loadExampleDirectory(t *testing.T) model.DirectoryView {
	directory := loadExample[model.DirectoryView](t, "e2b.directory-view.json")
	directory.Normalize()
	return directory
}

func loadExampleOverview(t *testing.T) model.OverviewView {
	overview := loadExample[model.OverviewView](t, "e2b.overview-view.json")
	overview.Normalize()
	return overview
}

func loadExampleWorkspace(t *testing.T) model.WorkspaceView {
	workspace := loadExample[model.WorkspaceView](t, "e2b.workspace-view.json")
	workspace.Normalize()
	return workspace
}

func serviceObservabilityValidator(t *testing.T) *contract.Validator {
	t.Helper()
	validator, err := contract.NewValidator()
	if err != nil {
		t.Fatalf("编译契约 schema 失败: %v", err)
	}
	return validator
}

func getJSON(t *testing.T, router *Router, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, authedRequest(http.MethodGet, path, cookie))
	return recorder
}

func decodeErrorBody(t *testing.T, recorder *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error      string `json:"error"`
		ReasonCode string `json:"reason_code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误响应失败: %v; body=%s", err, recorder.Body.String())
	}
	return body.Error, body.ReasonCode
}

// ---- 鉴权 ----

// 未登录访问三个端点都必须 401，且不得触达 facade。
func TestServiceObservabilityRequiresSession(t *testing.T) {
	router, pageViews, _ := newServiceObservabilityRouter(t)
	for _, path := range []string{
		"/api/v1/service-observability",
		"/api/v1/service-observability/svc_e2b_fixture",
		"/api/v1/service-observability/svc_e2b_fixture/workspace",
	} {
		recorder := getJSON(t, router, path, nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d, want 401", path, recorder.Code)
		}
	}
	if pageViews.calls != 0 {
		t.Fatalf("未登录请求不得触达 facade，实际调用 %d 次", pageViews.calls)
	}
}

// 登录 member 可读三个端点；URL 里自报的角色/团队/权限快照一律被忽略。
func TestServiceObservabilityMemberCanReadAndUrlRoleIsIgnored(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)

	recorder := getJSON(t, router,
		"/api/v1/service-observability?role=admin&is_admin=true&team_id=3f6a5b52-6c4a-4a1e-9b4d-2a1c8f0d7e11"+
			"&permission_snapshot_id=psid_forged", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("member 应可读目录, status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	subject := pageViews.directoryCall.Subject
	if subject.UserID != "u-observer" {
		t.Fatalf("actor 必须来自 session，实际 %q", subject.UserID)
	}
	for _, role := range subject.Roles {
		if role == "admin" {
			t.Fatal("URL 自报角色不得进入 actor")
		}
	}
	// team_id 是合法的目录筛选参数，但它不能变成 actor 的团队归属。
	if len(subject.TeamIDs) != 0 {
		t.Fatalf("actor 团队不得来自 URL，实际 %v", subject.TeamIDs)
	}
	if pageViews.directoryCall.TeamID != "3f6a5b52-6c4a-4a1e-9b4d-2a1c8f0d7e11" {
		t.Fatalf("team_id 筛选未下传: %+v", pageViews.directoryCall)
	}

	for _, path := range []string{
		"/api/v1/service-observability/svc_e2b_fixture?role=admin",
		"/api/v1/service-observability/svc_e2b_fixture/workspace?role=admin",
	} {
		if code := getJSON(t, router, path, cookie).Code; code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200", path, code)
		}
	}
	if pageViews.overviewCall.Subject.UserID != "u-observer" ||
		pageViews.workspaceCall.Subject.UserID != "u-observer" {
		t.Fatal("总览/工作区的 actor 同样必须来自 session")
	}
}

// ---- 响应形状 ----

// 三个端点的响应都必须过 Page View schema：集合字段即使为空也是 []，不是 null。
func TestServiceObservabilityResponsesAreSchemaValid(t *testing.T) {
	router, _, cookie := newServiceObservabilityRouter(t)
	validator := serviceObservabilityValidator(t)

	for _, path := range []string{
		"/api/v1/service-observability",
		"/api/v1/service-observability/svc_e2b_fixture",
		"/api/v1/service-observability/svc_e2b_fixture/workspace",
	} {
		recorder := getJSON(t, router, path, cookie)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		if err := validator.ValidateJSON(contract.KindPageView, recorder.Body.Bytes()); err != nil {
			t.Fatalf("%s 响应未过 Page View schema: %v", path, err)
		}
		// 集合字段即使为空也必须是 []：null 会让页面崩溃。profile 与 next_cursor 是
		// 显式可空的标量引用，不在这条约束内。
		for _, collection := range []string{
			"items", "sources", "reason_codes", "golden_signals", "sections", "links",
			"faults", "dependencies", "recent_events", "focus_kinds", "journey",
			"nodes", "relations", "signals", "evidence_refs", "timeline",
			"alerts", "impacts", "incidents", "statements", "evidence_ids",
			"missing_requirements", "entity_ids", "relation_ids",
		} {
			if strings.Contains(recorder.Body.String(), `"`+collection+`":null`) {
				t.Fatalf("%s 响应里 %s 是 null，必须是 []", path, collection)
			}
		}
	}
}

// Partial/Forbidden/Empty/Stale 都是 200 的 view 状态，绝不误用 500。
func TestServiceObservabilityDegradedViewsStay200(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	base := pageViews.workspace

	for _, state := range []model.DataState{
		model.DataStateComplete, model.DataStatePartial, model.DataStateStale,
	} {
		degraded := base
		degraded.DataStatus = model.DataStatus{State: state, Sources: []model.SourceStatus{{
			SourceKey: serviceobs.SourceKeyTelemetry, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonTelemetryNotConnected,
		}}}
		degraded.Sections = []model.SectionStatus{
			{SectionKey: serviceobs.SectionKeyGolden, State: model.AvailabilityUnavailable,
				ReasonCode: serviceobs.ReasonTelemetryNotConnected},
			{SectionKey: serviceobs.SectionKeyEvidence, State: model.AvailabilityForbidden,
				ReasonCode: "logs_read_forbidden"},
			{SectionKey: serviceobs.SectionKeyJourney, State: model.AvailabilityEmpty},
		}
		pageViews.workspace = degraded

		recorder := getJSON(t, router, "/api/v1/service-observability/svc_e2b_fixture/workspace", cookie)
		if recorder.Code != http.StatusOK {
			t.Fatalf("Data=%s 必须仍是 200，实际 %d", state, recorder.Code)
		}
		if err := serviceObservabilityValidator(t).ValidateJSON(contract.KindPageView, recorder.Body.Bytes()); err != nil {
			t.Fatalf("降级视图必须仍过 schema: %v", err)
		}
	}
}

// ---- 目录参数白名单 ----

// 非法枚举/ID/长度一律 422，并且绝不触达 facade（因此也不会到达 SQL）。
func TestDirectoryRejectsNonAllowlistedParametersBeforeFacade(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"未知排序字段", "?sort=cpu", reasonInvalidFilter},
		{"未知排序方向", "?direction=sideways", reasonInvalidFilter},
		{"未知健康状态", "?health=exploded", reasonInvalidFilter},
		{"未知数据状态", "?data_status=fresh", reasonInvalidFilter},
		{"空 CSV 值", "?health=critical,", reasonInvalidFilter},
		{"CSV 过多", "?health=" + strings.Repeat("critical,", 9) + "critical", reasonInvalidFilter},
		{"team_id 非 UUID", "?team_id=platform-sre", reasonInvalidFilter},
		{"tier 非法", "?tier=tier-9", reasonInvalidFilter},
		{"environment 非法", "?environment=" + strings.Repeat("p", 65), reasonInvalidFilter},
		{"q 过长", "?q=" + strings.Repeat("x", 129), reasonInvalidFilter},
		{"limit 非整数", "?limit=abc", reasonInvalidLimit},
		{"limit 越界", "?limit=201", reasonInvalidLimit},
		{"limit 为零", "?limit=0", reasonInvalidLimit},
		{"游标超长", "?cursor=" + strings.Repeat("a", facade.MaxCursorLength+1), reasonInvalidCursor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, pageViews, cookie := newServiceObservabilityRouter(t)
			recorder := getJSON(t, router, "/api/v1/service-observability"+tc.query, cookie)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d, want 422; body=%s", recorder.Code, recorder.Body.String())
			}
			_, reason := decodeErrorBody(t, recorder)
			if reason != tc.want {
				t.Fatalf("reason_code=%q, want %q", reason, tc.want)
			}
			if pageViews.calls != 0 {
				t.Fatal("非法参数必须在任何查询之前被拒绝")
			}
		})
	}
}

// 合法参数按白名单原样下传，limit 缺省交给 facade 决定默认值。
func TestDirectoryPassesAllowlistedParametersThrough(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	recorder := getJSON(t, router, "/api/v1/service-observability"+
		"?environment=production&team_id=3f6a5b52-6c4a-4a1e-9b4d-2a1c8f0d7e11&tier=tier-1"+
		"&health=critical,degraded&data_status=complete,partial&q=e2b&sort=health&direction=desc&limit=25",
		cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	query := pageViews.directoryCall
	if query.Environment != "production" || query.Tier != "tier-1" || query.Query != "e2b" {
		t.Fatalf("metadata 过滤未原样下传: %+v", query)
	}
	if len(query.Health) != 2 || query.Health[0] != model.HealthCritical {
		t.Fatalf("health 筛选解析错误: %+v", query.Health)
	}
	if len(query.DataStatus) != 2 || query.DataStatus[1] != model.DataStatePartial {
		t.Fatalf("data_status 筛选解析错误: %+v", query.DataStatus)
	}
	if query.Sort.Key != view.SortKeyHealth || !query.Sort.Descending {
		t.Fatalf("排序解析错误: %+v", query.Sort)
	}
	if query.Limit != 25 {
		t.Fatalf("limit=%d, want 25", query.Limit)
	}

	// 每个合法排序字段都必须被接受，避免白名单与 view 的定义漂移。
	for _, key := range view.DirectorySortKeys() {
		path := "/api/v1/service-observability?sort=" + string(key)
		if code := getJSON(t, router, path, cookie).Code; code != http.StatusOK {
			t.Fatalf("sort=%s status=%d, want 200", key, code)
		}
	}
}

// 游标指纹不匹配是 422：facade 的 typed 错误必须映射成稳定 code，而不是 500。
func TestDirectoryCursorMismatchIsUnprocessable(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	pageViews.directoryErr = facade.ErrInvalidCursor

	recorder := getJSON(t, router, "/api/v1/service-observability?cursor=abc", cookie)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", recorder.Code)
	}
	if _, reason := decodeErrorBody(t, recorder); reason != reasonInvalidCursor {
		t.Fatalf("reason_code=%q, want %q", reason, reasonInvalidCursor)
	}
}

// 候选基数超界是可由收窄筛选解决的 422 scope_too_broad，不是 503。
func TestDirectoryCandidateOverflowIsScopeTooBroad(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	pageViews.directoryErr = serviceobs.ErrCandidateSetTooLarge

	recorder := getJSON(t, router, "/api/v1/service-observability", cookie)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422; body=%s", recorder.Code, recorder.Body.String())
	}
	if _, reason := decodeErrorBody(t, recorder); reason != reasonScopeTooBroad {
		t.Fatalf("reason_code=%q, want %q", reason, reasonScopeTooBroad)
	}
}

// registry 等关键依赖不可用是 503 页面级错误。
func TestDirectoryRegistryFailureIs503(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	pageViews.directoryErr = errors.New("registration 读取失败：dial tcp 10.0.0.7:5432 connect: refused")

	recorder := getJSON(t, router, "/api/v1/service-observability", cookie)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", recorder.Code)
	}
	message, reason := decodeErrorBody(t, recorder)
	if reason != reasonUpstreamUnavailable {
		t.Fatalf("reason_code=%q, want %q", reason, reasonUpstreamUnavailable)
	}
	if strings.Contains(message, "5432") || strings.Contains(message, "dial tcp") {
		t.Fatalf("响应泄露了内部错误细节: %q", message)
	}
}

// ---- 总览与工作区参数 ----

func TestOverviewParameterHandling(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)

	recorder := getJSON(t, router,
		"/api/v1/service-observability/svc_e2b_fixture?environment=production&window_seconds=300", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if pageViews.overviewCall.ServiceID != "svc_e2b_fixture" ||
		pageViews.overviewCall.Environment != "production" ||
		pageViews.overviewCall.WindowSeconds != 300 {
		t.Fatalf("总览参数解析错误: %+v", pageViews.overviewCall)
	}

	// environment 未传时不伪造：由服务端按 registration 环境决定。
	if code := getJSON(t, router, "/api/v1/service-observability/svc_e2b_fixture", cookie).Code; code != http.StatusOK {
		t.Fatalf("缺省 environment status=%d, want 200", code)
	}
	if pageViews.overviewCall.Environment != "" {
		t.Fatalf("缺省 environment 不得被伪造成 %q", pageViews.overviewCall.Environment)
	}

	for _, tc := range []struct{ query, reason string }{
		{"?window_seconds=0", reasonInvalidWindow},
		{"?window_seconds=abc", reasonInvalidWindow},
		{"?window_seconds=-5", reasonInvalidWindow},
		{"?environment=prod%20uction", reasonInvalidFilter},
	} {
		recorder := getJSON(t, router, "/api/v1/service-observability/svc_e2b_fixture"+tc.query, cookie)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status=%d, want 422", tc.query, recorder.Code)
		}
		if _, reason := decodeErrorBody(t, recorder); reason != tc.reason {
			t.Fatalf("%s reason_code=%q, want %q", tc.query, reason, tc.reason)
		}
	}
}

// 显式环境不匹配与服务不存在统一 404，不泄露其它环境是否存在。
func TestOverviewNotFoundIsUniform(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	pageViews.overviewErr = serviceobs.ErrServiceNotFound

	for _, path := range []string{
		"/api/v1/service-observability/svc_missing",
		"/api/v1/service-observability/svc_e2b_fixture?environment=staging",
	} {
		recorder := getJSON(t, router, path, cookie)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", path, recorder.Code)
		}
		if _, reason := decodeErrorBody(t, recorder); reason != reasonServiceNotFound {
			t.Fatalf("%s reason_code=%q, want %q", path, reason, reasonServiceNotFound)
		}
	}

	// 形状都不满足的 service ID 同样 404，且不触达 facade。
	pageViews.calls = 0
	long := "/api/v1/service-observability/" + strings.Repeat("s", maxServiceIDLength+1)
	if code := getJSON(t, router, long, cookie).Code; code != http.StatusNotFound {
		t.Fatalf("超长 service ID status=%d, want 404", code)
	}
	if pageViews.calls != 0 {
		t.Fatal("超长 service ID 不得下传")
	}
}

func TestWorkspaceParameterHandling(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)

	recorder := getJSON(t, router, "/api/v1/service-observability/svc_e2b_fixture/workspace"+
		"?environment=production&focus_kind=sandbox_id&focus_value=sbx_hp16c3"+
		"&from=2026-08-22T02:04:22Z&to=2026-08-22T02:09:22Z&timezone=Asia/Shanghai", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	call := pageViews.workspaceCall
	if call.Focus == nil || call.Focus.Kind != "sandbox_id" || call.Focus.Value != "sbx_hp16c3" {
		t.Fatalf("focus 解析错误: %+v", call.Focus)
	}
	if call.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone 未原样下传: %q", call.Timezone)
	}
	wantFrom := time.Date(2026, 8, 22, 2, 4, 22, 0, time.UTC)
	wantTo := time.Date(2026, 8, 22, 2, 9, 22, 0, time.UTC)
	if !call.From.Equal(wantFrom) || !call.To.Equal(wantTo) {
		t.Fatalf("时间窗解析错误: from=%v to=%v", call.From, call.To)
	}

	// 两个时间都缺省时保持零值，由服务端用单次 now 与 Profile 默认窗口生成。
	if code := getJSON(t, router,
		"/api/v1/service-observability/svc_e2b_fixture/workspace", cookie).Code; code != http.StatusOK {
		t.Fatalf("缺省时间窗 status=%d, want 200", code)
	}
	if !pageViews.workspaceCall.From.IsZero() || !pageViews.workspaceCall.To.IsZero() {
		t.Fatal("缺省时间窗不得由 HTTP 层伪造")
	}
	if pageViews.workspaceCall.Focus != nil {
		t.Fatal("无 focus 时不得伪造 focus")
	}

	for _, tc := range []struct{ query, reason string }{
		{"?focus_kind=sandbox_id", reasonInvalidFocus},
		{"?focus_value=sbx_hp16c3", reasonInvalidFocus},
		{"?focus_kind=Sandbox_ID&focus_value=sbx_hp16c3", reasonInvalidFocus},
		{"?focus_kind=sandbox_id&focus_value=" + strings.Repeat("s", maxFocusValueLength+1), reasonInvalidFocus},
		{"?from=2026-08-22T02:04:22Z", reasonInvalidTimeRange},
		{"?to=2026-08-22T02:09:22Z", reasonInvalidTimeRange},
		{"?from=2026-08-22%2002:04:22&to=2026-08-22T02:09:22Z", reasonInvalidTimeRange},
		{"?timezone=" + strings.Repeat("z", maxTimezoneLength+1), reasonInvalidTimezone},
	} {
		recorder := getJSON(t, router,
			"/api/v1/service-observability/svc_e2b_fixture/workspace"+tc.query, cookie)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status=%d, want 422; body=%s", tc.query, recorder.Code, recorder.Body.String())
		}
		if _, reason := decodeErrorBody(t, recorder); reason != tc.reason {
			t.Fatalf("%s reason_code=%q, want %q", tc.query, reason, tc.reason)
		}
	}
}

// facade 的 typed 错误映射：409 profile_unconfigured、422 invalid_focus/scope_too_broad。
func TestWorkspaceTypedErrorsMapToStableStatuses(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"未配置 Profile", serviceobs.ErrProfileUnconfigured, http.StatusConflict, reasonProfileUnconfigured},
		{"focus 非法", serviceobs.ErrInvalidFocus, http.StatusUnprocessableEntity, reasonInvalidFocus},
		{"范围超限", serviceobs.ErrScopeTooBroad, http.StatusUnprocessableEntity, reasonScopeTooBroad},
		{"服务不存在", serviceobs.ErrServiceNotFound, http.StatusNotFound, reasonServiceNotFound},
		{"时间窗非法", soctx.ErrInvalidTimeRange, http.StatusUnprocessableEntity, reasonInvalidTimeRange},
		{"时区非法", soctx.ErrInvalidTimezone, http.StatusUnprocessableEntity, reasonInvalidTimezone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, pageViews, cookie := newServiceObservabilityRouter(t)
			pageViews.workspaceErr = tc.err
			recorder := getJSON(t, router,
				"/api/v1/service-observability/svc_e2b_fixture/workspace", cookie)
			if recorder.Code != tc.status {
				t.Fatalf("status=%d, want %d; body=%s", recorder.Code, tc.status, recorder.Body.String())
			}
			message, reason := decodeErrorBody(t, recorder)
			if reason != tc.reason {
				t.Fatalf("reason_code=%q, want %q", reason, tc.reason)
			}
			if message == "" {
				t.Fatal("错误响应必须带可展示文案")
			}
		})
	}
}

// ---- 泄露与取消 ----

// 响应绝不包含 Profile 路径、SQL、PromQL、凭据或 Go error 文本。
func TestServiceObservabilityResponsesDoNotLeakInternals(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)
	pageViews.workspaceErr = errors.New(
		"profile dir /etc/alert-system/profiles/e2b.json: SELECT * FROM alerts WHERE token='s3cr3t' " +
			"promql=histogram_quantile(0.95, rate(e2b_trace_bucket[5m]))")

	recorder := getJSON(t, router, "/api/v1/service-observability/svc_e2b_fixture/workspace", cookie)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", recorder.Code)
	}
	body := recorder.Body.String()
	for _, leaked := range []string{
		"/etc/alert-system", "SELECT", "token", "s3cr3t", "promql", "histogram_quantile", "e2b_trace",
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(leaked)) {
			t.Fatalf("响应泄露 %q: %s", leaked, body)
		}
	}
}

// 请求取消必须原样传导到 facade，而不是被 HTTP 层吞成空视图。
func TestServiceObservabilityPropagatesRequestCancellation(t *testing.T) {
	router, pageViews, cookie := newServiceObservabilityRouter(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := authedRequest(http.MethodGet, "/api/v1/service-observability", cookie).WithContext(ctx)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if !pageViews.sawCanceled {
		t.Fatal("facade 必须看到已取消的请求 context")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("取消后的 status=%d, want 503（不泄露内部原因）", recorder.Code)
	}
}

// ---- 既有路由不受影响 ----

// 新增路由不得改动或遮蔽既有 GET /api/v1/services。
func TestServiceObservabilityRoutesDoNotShadowServiceCatalog(t *testing.T) {
	router, _, _ := newServiceObservabilityRouter(t)
	paths := map[string]string{}
	for _, route := range router.Engine().Routes() {
		paths[route.Method+" "+route.Path] = route.Handler
	}
	for _, path := range []string{
		"GET /api/v1/service-observability",
		"GET /api/v1/service-observability/:service_id",
		"GET /api/v1/service-observability/:service_id/workspace",
	} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("缺少路由 %s", path)
		}
	}
	for path := range paths {
		if strings.HasPrefix(path, "GET /api/v1/services") {
			t.Fatalf("本次注册不得引入或改写 %s", path)
		}
	}
}
