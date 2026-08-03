package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"alert-system/internal/alertsource"
)

type failingAlertSourceReconciler struct{ err error }

func (f failingAlertSourceReconciler) ReconcileSource(context.Context, string) (alertsource.ReconcileResult, error) {
	return alertsource.ReconcileResult{}, f.err
}

func TestAlertSourceReconcileDoesNotExposeUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/alert-sources/source-1/reconcile", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "source-1"}}
	handler := &alertSourceHandlers{
		router: &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		reconciler: failingAlertSourceReconciler{err: errors.New(
			"GET https://alertmanager.internal/api/v2/alerts: bearer secret-token; response=private upstream body",
		)},
	}

	handler.reconcile(ctx)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502; body=%s", recorder.Code, recorder.Body.String())
	}
	for _, forbidden := range []string{"alertmanager.internal", "secret-token", "private upstream body"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("502 响应泄露上游错误 %q: %s", forbidden, recorder.Body.String())
		}
	}
}
