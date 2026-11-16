package api

import (
	"alert-system/internal/app"
	"context"
	"github.com/jackc/pgx/v5"
	"net/http"
	"testing"
	"time"
)

type fakeFaultNotificationService struct {
	calls int
	retry bool
}

func (f *fakeFaultNotificationService) Preview(context.Context, pgx.Tx, string, bool) (app.FaultNotificationPreview, error) {
	f.calls++
	return app.FaultNotificationPreview{}, nil
}
func (f *fakeFaultNotificationService) Submit(_ context.Context, _ pgx.Tx, id string, retry bool, _, _ string) (app.FaultNotificationPreview, error) {
	f.calls++
	f.retry = retry
	return app.FaultNotificationPreview{FaultID: id}, nil
}
func TestFaultNotificationsRBAC(t *testing.T) {
	r, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "ops", "correct-password", true)
	cookie := doLogin(t, r, "ops", "correct-password")
	f := &fakeFaultNotificationService{}
	r.RegisterFaultNotificationRoutes(f)
	path := "/api/v1/admin/faults/11111111-1111-4111-8111-111111111111/notifications/submit?retry_failed=true"
	if w := doFaultsRequest(r, http.MethodPost, path, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := doFaultsRequest(r, http.MethodPost, path, cookie); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if f.calls != 0 {
		t.Fatal("越权调用service")
	}
	memberships.admins["u1"] = true
	if w := doFaultsRequest(r, http.MethodPost, path, cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !f.retry || f.calls != 1 {
		t.Fatal(f)
	}
	if w := doFaultsRequest(r, http.MethodPost, "/api/v1/admin/faults/bad/notifications/submit", cookie); w.Code != 400 {
		t.Fatal(w.Code)
	}
}
