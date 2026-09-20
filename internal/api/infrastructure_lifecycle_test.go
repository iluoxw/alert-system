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

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

type lifecycleFakeStore struct {
	nodes        map[string]domain.ResourceNode
	preview      store.EntityLifecyclePreview
	activeFaults bool
	setCalls     int
	cascadeCalls int
	restoreCalls int
	rebuildCalls int
	cascaded     []domain.Fault
}

func (f *lifecycleFakeStore) GetByID(_ context.Context, _ pgx.Tx, id string) (domain.ResourceNode, error) {
	node, ok := f.nodes[id]
	if !ok {
		return domain.ResourceNode{}, store.ErrResourceNodeNotFound
	}
	return node, nil
}

func (f *lifecycleFakeStore) SetEntityLifecycle(_ context.Context, _ pgx.Tx, id, actorID, reason string, state domain.EntityLifecycleState, at time.Time) (domain.ResourceNode, bool, error) {
	f.setCalls++
	node, ok := f.nodes[id]
	if !ok {
		return domain.ResourceNode{}, false, store.ErrResourceNodeNotFound
	}
	if node.LifecycleState == state {
		return node, false, nil
	}
	if state == domain.EntityLifecycleDecommissioned {
		if reason == "" {
			return domain.ResourceNode{}, false, store.ErrEntityLifecycleReasonRequired
		}
		atCopy := at
		node.LifecycleState = state
		node.DecommissionedAt = &atCopy
		node.DecommissionedBy = actorID
		node.DecommissionReason = reason
	} else {
		node.LifecycleState = domain.EntityLifecycleActive
		node.DecommissionedAt = nil
		node.DecommissionedBy = ""
		node.DecommissionReason = ""
	}
	f.nodes[id] = node
	return node, true, nil
}

func (f *lifecycleFakeStore) HasActiveRelatedFaults(context.Context, pgx.Tx, string) (bool, error) {
	return f.activeFaults, nil
}

func (f *lifecycleFakeStore) Preview(context.Context, pgx.Tx, string) (store.EntityLifecyclePreview, error) {
	preview := f.preview
	if preview.ActiveFaults == nil {
		preview.ActiveFaults = []domain.Fault{}
	}
	if preview.OpenIncidents == nil {
		preview.OpenIncidents = []domain.Incident{}
	}
	if preview.FiringAlerts == nil {
		preview.FiringAlerts = []domain.Alert{}
	}
	return preview, nil
}

func (f *lifecycleFakeStore) CascadeDecommission(context.Context, pgx.Tx, domain.ResourceNode, string, string, string, time.Time) ([]domain.Fault, error) {
	f.cascadeCalls++
	if f.cascaded == nil {
		return []domain.Fault{}, nil
	}
	return f.cascaded, nil
}

func (f *lifecycleFakeStore) NoteRestore(context.Context, pgx.Tx, domain.ResourceNode, string, string, time.Time) error {
	f.restoreCalls++
	return nil
}

func (f *lifecycleFakeStore) RebuildImpacts(context.Context, pgx.Tx, domain.ResourceNode) error {
	f.rebuildCalls++
	return nil
}

func newLifecycleTestRouter(t *testing.T) (*Router, *lifecycleFakeStore, *http.Cookie, *http.Cookie) {
	t.Helper()
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	addTestUser(users, "member-1", "member", "correct-password", true)
	adminCookie := doLogin(t, router, "admin", "correct-password")
	memberCookie := doLogin(t, router, "member", "correct-password")
	fake := &lifecycleFakeStore{nodes: map[string]domain.ResourceNode{
		"node-1": {
			ID: "node-1", EntityKey: "host:10.61.17.11", Name: "p-ai-sandbox-001",
			LifecycleState: domain.EntityLifecycleActive,
		},
	}}
	router.RegisterEntityLifecycleRoutes(fake)
	return router, fake, adminCookie, memberCookie
}

func doLifecycleJSON(router *Router, method, path string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestLifecyclePreview_EmptyCollectionsAreArrays(t *testing.T) {
	router, _, adminCookie, _ := newLifecycleTestRouter(t)
	w := doLifecycleJSON(router, http.MethodGet, "/api/v1/admin/entities/node-1/lifecycle-preview", adminCookie, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"active_faults", "open_incidents", "firing_alerts"} {
		if string(body[key]) != "[]" {
			t.Fatalf("%s = %s, want []", key, body[key])
		}
	}
}

func TestLifecyclePreview_MemberForbiddenEntityMissing(t *testing.T) {
	router, _, adminCookie, memberCookie := newLifecycleTestRouter(t)
	w := doLifecycleJSON(router, http.MethodGet, "/api/v1/admin/entities/node-1/lifecycle-preview", memberCookie, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member status = %d, want 403", w.Code)
	}
	w = doLifecycleJSON(router, http.MethodGet, "/api/v1/admin/entities/missing/lifecycle-preview", adminCookie, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", w.Code)
	}
	w = doLifecycleJSON(router, http.MethodGet, "/api/v1/admin/entities/node-1/lifecycle-preview", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anon status = %d, want 401", w.Code)
	}
}

func TestLifecyclePatch_ActiveFaultsWithoutCascadeConflict(t *testing.T) {
	router, fake, adminCookie, _ := newLifecycleTestRouter(t)
	fake.activeFaults = true
	w := doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", adminCookie, map[string]any{
		"state": "decommissioned", "reason": "节点已从集群剔除",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 body=%s", w.Code, w.Body.String())
	}
	if fake.setCalls != 0 || fake.cascadeCalls != 0 {
		t.Fatalf("409 不得写生命周期或 cascade: set=%d cascade=%d", fake.setCalls, fake.cascadeCalls)
	}
}

func TestLifecyclePatch_CascadeClosesFaultsLeavesIncident(t *testing.T) {
	router, fake, adminCookie, _ := newLifecycleTestRouter(t)
	fake.activeFaults = true
	fake.cascaded = []domain.Fault{{
		ID: "f1", Status: domain.FaultStatusResolved, Title: "host down",
	}}

	w := doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", adminCookie, map[string]any{
		"state": "decommissioned", "reason": "节点已从集群剔除", "cascade_faults": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Entity         domain.ResourceNode `json:"entity"`
		CascadedFaults []domain.Fault      `json:"cascaded_faults"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Entity.LifecycleState != domain.EntityLifecycleDecommissioned || body.Entity.DecommissionReason != "节点已从集群剔除" {
		t.Fatalf("实体未下线: %+v", body.Entity)
	}
	if len(body.CascadedFaults) != 1 || body.CascadedFaults[0].ID != "f1" || body.CascadedFaults[0].Status != domain.FaultStatusResolved {
		t.Fatalf("cascade 列表不对: %+v", body.CascadedFaults)
	}
	if fake.cascadeCalls != 1 || fake.rebuildCalls != 1 {
		t.Fatalf("首次下线应 cascade+rebuild 各一次: cascade=%d rebuild=%d", fake.cascadeCalls, fake.rebuildCalls)
	}
	if fake.restoreCalls != 0 {
		t.Fatalf("下线不得走恢复时间线: restore=%d", fake.restoreCalls)
	}
}

func TestLifecyclePatch_RepeatDecommissionShortCircuits(t *testing.T) {
	router, fake, adminCookie, _ := newLifecycleTestRouter(t)
	w1 := doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", adminCookie, map[string]any{
		"state": "decommissioned", "reason": "第一次", "cascade_faults": true,
	})
	w2 := doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", adminCookie, map[string]any{
		"state": "decommissioned", "reason": "第二次不应写入", "cascade_faults": true,
	})
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("重复下线应 200, got %d / %d", w1.Code, w2.Code)
	}
	var body struct {
		Entity         domain.ResourceNode `json:"entity"`
		CascadedFaults []domain.Fault      `json:"cascaded_faults"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Entity.DecommissionReason != "第一次" {
		t.Fatalf("重复下线覆盖了原因: %+v", body.Entity)
	}
	if len(body.CascadedFaults) != 0 {
		t.Fatalf("短路不得再收口: %+v", body.CascadedFaults)
	}
	if fake.setCalls != 2 || fake.cascadeCalls != 1 || fake.rebuildCalls != 1 {
		t.Fatalf("第二次不得 cascade/rebuild: set=%d cascade=%d rebuild=%d", fake.setCalls, fake.cascadeCalls, fake.rebuildCalls)
	}
}

func TestLifecyclePatch_ReasonRequiredAndMemberForbidden(t *testing.T) {
	router, fake, adminCookie, memberCookie := newLifecycleTestRouter(t)
	w := doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", adminCookie, map[string]any{
		"state": "decommissioned",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("缺原因 status = %d, want 422", w.Code)
	}
	w = doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", memberCookie, map[string]any{
		"state": "decommissioned", "reason": "x",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("member status = %d, want 403", w.Code)
	}
	if fake.setCalls != 0 {
		t.Fatalf("校验失败不得写库: set=%d", fake.setCalls)
	}
}

func TestLifecyclePatch_RestoreDoesNotCascade(t *testing.T) {
	router, fake, adminCookie, _ := newLifecycleTestRouter(t)
	fake.nodes["node-1"] = domain.ResourceNode{
		ID: "node-1", EntityKey: "host:10.61.17.11",
		LifecycleState: domain.EntityLifecycleDecommissioned, DecommissionReason: "旧原因",
	}
	w := doLifecycleJSON(router, http.MethodPatch, "/api/v1/admin/entities/node-1/lifecycle", adminCookie, map[string]any{
		"state": "active", "cascade_faults": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if fake.cascadeCalls != 0 || fake.restoreCalls != 1 || fake.rebuildCalls != 1 {
		t.Fatalf("恢复不得 cascade: cascade=%d restore=%d rebuild=%d", fake.cascadeCalls, fake.restoreCalls, fake.rebuildCalls)
	}
	var body struct {
		Entity domain.ResourceNode `json:"entity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Entity.LifecycleState != domain.EntityLifecycleActive || body.Entity.DecommissionReason != "" {
		t.Fatalf("恢复未清空审计: %+v", body.Entity)
	}
}
