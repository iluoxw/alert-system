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

func TestRunbookHandlerMapsExecutorDisabledToServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	handler := &runbookHandlers{router: &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	handler.handleError(ctx, runbook.ErrExecutorDisabled, "execute")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRunbookHandlerMapsGuardRejectedToConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	handler := &runbookHandlers{router: &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	handler.handleError(ctx, errors.Join(runbook.ErrGuardRejected, errors.New("role missing")), "execute")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}
