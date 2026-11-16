package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMappingGovernanceRBACAndBounds(t *testing.T) {
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "admin-1", "admin", "password", true)
	memberships.admins["admin-1"] = true
	admin := doLogin(t, router, "admin", "password")
	addTestUser(users, "member-1", "member", "password", true)
	member := doLogin(t, router, "member", "password")
	router.RegisterServiceCatalogRoutes(nil, nil, nil, nil, nil)
	for _, endpoint := range []struct{ method, path string }{{"POST", "/api/v1/admin/alert-mapping/preview"}, {"GET", "/api/v1/admin/alert-mapping/governance"}} {
		for _, tc := range []struct {
			cookie *http.Cookie
			want   int
		}{{nil, 401}, {member, 403}} {
			w := httptest.NewRecorder()
			req := authedRequest(endpoint.method, endpoint.path, tc.cookie)
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%s status=%d want=%d", endpoint.path, w.Code, tc.want)
			}
		}
	}
	for _, body := range []string{`{"alert_ids":[]}`, `{"alert_ids":["invalid"]}`, `{"alert_ids":[` + strings.Repeat(`"00000000-0000-0000-0000-000000000000",`, 100) + `"00000000-0000-0000-0000-000000000000"]}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/admin/alert-mapping/preview", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(admin)
		router.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("bad request code=%d body=%s", w.Code, w.Body.String())
		}
	}
}
