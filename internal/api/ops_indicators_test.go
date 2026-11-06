package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/ops"
)

func TestOpsIndicators_AdminOK_Member403_Unauthenticated401(t *testing.T) {
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	adminCookie := doLogin(t, router, "admin", "correct-password")
	addTestUser(users, "member-1", "member", "correct-password", true)
	memberCookie := doLogin(t, router, "member", "correct-password")

	rate := 0.5
	median := 1800.0
	unresolved := 3
	router.RegisterOpsIndicatorsRoutes(func(ctx context.Context, tx pgx.Tx, window ops.IndicatorWindow) (ops.Indicators, error) {
		return ops.Indicators{
			FiringMappingRate:        &rate,
			ActiveFaultsWithoutOwner: 2,
			FaultAckMedianSeconds:    &median,
			RecoveryNudgeFiredCount:  4,
			UnresolvedIdentityCount:  &unresolved,
		}, nil
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/ops/indicators", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 status = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/ops/indicators", memberCookie))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member status = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/ops/indicators", adminCookie))
	if w.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body["firing_mapping_rate"] != 0.5 {
		t.Fatalf("firing_mapping_rate = %v", body["firing_mapping_rate"])
	}
	if body["active_faults_without_owner"] != float64(2) {
		t.Fatalf("active_faults_without_owner = %v", body["active_faults_without_owner"])
	}
	if body["recovery_nudge_fired_count"] != float64(4) {
		t.Fatalf("recovery_nudge_fired_count = %v", body["recovery_nudge_fired_count"])
	}
	if body["fault_ack_duration_median_seconds"] != 1800.0 {
		t.Fatalf("fault_ack_duration_median_seconds = %v", body["fault_ack_duration_median_seconds"])
	}
	if body["unresolved_identity_count"] != float64(3) {
		t.Fatalf("unresolved_identity_count = %v", body["unresolved_identity_count"])
	}
}

func TestOpsIndicators_NullFieldsSerializeAsNull(t *testing.T) {
	router, _, users, _, memberships := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "admin-1", "admin", "correct-password", true)
	memberships.admins["admin-1"] = true
	cookie := doLogin(t, router, "admin", "correct-password")
	router.RegisterOpsIndicatorsRoutes(func(ctx context.Context, tx pgx.Tx, window ops.IndicatorWindow) (ops.Indicators, error) {
		return ops.Indicators{}, nil
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/api/v1/ops/indicators", cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	for _, key := range []string{"firing_mapping_rate", "fault_ack_duration_median_seconds", "unresolved_identity_count"} {
		if !strings.Contains(raw, `"`+key+`":null`) {
			t.Fatalf("无数据字段 %s 必须是 JSON null，不能缺省或 0: %s", key, raw)
		}
	}
	if strings.Contains(raw, "NaN") {
		t.Fatalf("响应不得含 NaN: %s", raw)
	}
	if !strings.Contains(raw, `"active_faults_without_owner":0`) {
		t.Fatalf("真实零必须是 0: %s", raw)
	}
	if !strings.Contains(raw, `"recovery_nudge_fired_count":0`) {
		t.Fatalf("真实零必须是 0: %s", raw)
	}
}
