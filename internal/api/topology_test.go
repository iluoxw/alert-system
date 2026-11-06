package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/store"
)

type unresolvedFakeStore struct {
	items []store.UnresolvedIdentity
}

func (f *unresolvedFakeStore) ListUnresolvedIdentities(_ context.Context, _ pgx.Tx, _, _ int) ([]store.UnresolvedIdentity, error) {
	if f.items == nil {
		return []store.UnresolvedIdentity{}, nil
	}
	return f.items, nil
}

func newTopologyAdminRouter(t *testing.T, admin bool) (*Router, *unresolvedFakeStore, *http.Cookie) {
	t.Helper()
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "member1", "correct-password", true)
	if admin {
		memberships.admins["u1"] = true
	}
	cookie := doLogin(t, router, "member1", "correct-password")
	fake := &unresolvedFakeStore{}
	router.RegisterTopologyAdminRoutes(fake)
	return router, fake, cookie
}

func TestUnresolvedIdentities_Unauthenticated_401(t *testing.T) {
	router, _, _ := newTopologyAdminRouter(t, true)
	w := doFaultsRequest(router, http.MethodGet, "/api/v1/admin/topology/unresolved", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestUnresolvedIdentities_Member_403(t *testing.T) {
	router, _, cookie := newTopologyAdminRouter(t, false)
	w := doFaultsRequest(router, http.MethodGet, "/api/v1/admin/topology/unresolved", cookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 body=%s", w.Code, w.Body.String())
	}
}

func TestUnresolvedIdentities_EmptySlice(t *testing.T) {
	router, fake, cookie := newTopologyAdminRouter(t, true)
	fake.items = nil
	w := doFaultsRequest(router, http.MethodGet, "/api/v1/admin/topology/unresolved", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Items []store.UnresolvedIdentity `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Items == nil {
		t.Fatal("items 不得为 null")
	}
	if len(body.Items) != 0 {
		t.Fatalf("len = %d", len(body.Items))
	}
}

func TestUnresolvedIdentities_AdminListsItems(t *testing.T) {
	router, fake, cookie := newTopologyAdminRouter(t, true)
	fake.items = []store.UnresolvedIdentity{{
		Namespace: "ccloud.rely_node", Value: "rely-1", HostPort: ":0",
		ServiceType: "REDIS", SampleApp: "app-1",
	}}
	w := doFaultsRequest(router, http.MethodGet, "/api/v1/admin/topology/unresolved", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatal(w.Body.String())
	}
}
