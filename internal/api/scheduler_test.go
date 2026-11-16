package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"alert-system/internal/scheduler"
)

func TestSchedulerStatus_AdminOnlyAndReadinessIndependent(t *testing.T) {
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	adminCookie := doLogin(t, router, "admin", "correct-password")
	addTestUser(users, "member-1", "member", "correct-password", true)
	memberCookie := doLogin(t, router, "member", "correct-password")
	calls := 0
	router.RegisterSchedulerRoutes(func(context.Context) (scheduler.Status, error) {
		calls++
		return scheduler.Status{Workers: []scheduler.WorkerStatus{{Pool: "detached", Kind: "notify_send", Running: true, DurationSeconds: 100, Degraded: true}}}, nil
	})
	for _, tt := range []struct {
		cookie *http.Cookie
		code   int
	}{{nil, 401}, {memberCookie, 403}, {adminCookie, 200}} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/ops/scheduler", tt.cookie))
		if w.Code != tt.code {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if tt.code == 200 {
			for _, key := range []string{"payload", "ref_id", "target", "credential"} {
				if strings.Contains(w.Body.String(), key) {
					t.Fatalf("sensitive field %s", key)
				}
			}
			if !strings.Contains(w.Body.String(), `"kinds":[]`) || !strings.Contains(w.Body.String(), `"pools":[]`) {
				t.Fatal("nil collection")
			}
		}
	}
	if calls != 1 {
		t.Fatal("unauthorized query executed")
	}
	router.RegisterHealthRoutes(ComposeReady(DatabaseReady(func(context.Context) error { return nil }), SchedulerReady(fakeNormalReady(true)), WorkerReady("pipeline", fakeHeartbeat{last: time.Now(), interval: time.Second})))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != 200 {
		t.Fatal("scheduler degraded removed ingest readiness")
	}
}

type fakeNormalReady bool

func (f fakeNormalReady) NormalWorkersReady() bool { return bool(f) }
func TestSchedulerReady_NormalPoolFailure(t *testing.T) {
	if SchedulerReady(fakeNormalReady(false))(context.Background()) != ErrReadyWorkers {
		t.Fatal("normal pool stall hidden")
	}
}
