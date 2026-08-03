package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"alert-system/internal/runbook"
)

func TestRunbookHandlerMapsExpectedUATFailuresToBadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failure := range []error{
		runbook.ErrE2BUATTransport,
		errors.Join(runbook.ErrE2BUATOperation, errors.New("pause 返回 HTTP 500")),
		runbook.ErrUATCleanupFailed,
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		handler := &runbookHandlers{router: &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
		handler.handleError(ctx, failure, "UAT test")
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 for %v, got %d body=%s", failure, recorder.Code, recorder.Body.String())
		}
	}
}
