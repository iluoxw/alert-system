package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

type faultsFakeStore struct {
	faults    map[string]domain.Fault
	timeline  []domain.FaultTimelineEntry
	ackCalls  int
	appendErr error
}

func (f *faultsFakeStore) GetByID(_ context.Context, _ pgx.Tx, id string) (domain.Fault, error) {
	fault, ok := f.faults[id]
	if !ok {
		return domain.Fault{}, store.ErrFaultNotFound
	}
	return fault, nil
}

func (f *faultsFakeStore) Ack(_ context.Context, _ pgx.Tx, id, actorID, actorName string, at time.Time) (domain.Fault, bool, error) {
	f.ackCalls++
	fault, ok := f.faults[id]
	if !ok {
		return domain.Fault{}, false, store.ErrFaultNotFound
	}
	if fault.IsAcked() {
		return fault, false, nil
	}
	fault.AckedBy = actorID
	fault.AckedByName = actorName
	fault.AckedAt = at
	fault.UpdatedAt = at
	f.faults[id] = fault
	return fault, true, nil
}

func (f *faultsFakeStore) AppendTimeline(_ context.Context, _ pgx.Tx, entry domain.FaultTimelineEntry) (string, error) {
	if f.appendErr != nil {
		return "", f.appendErr
	}
	f.timeline = append(f.timeline, entry)
	return "tl-1", nil
}

func newFaultsTestRouter(t *testing.T) (*Router, *faultsFakeStore, *http.Cookie) {
	t.Helper()
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "member1", "correct-password", true)
	cookie := doLogin(t, router, "member1", "correct-password")
	fake := &faultsFakeStore{faults: map[string]domain.Fault{}}
	router.RegisterFaultRoutes(fake)
	return router, fake, cookie
}

func doFaultsRequest(router *Router, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestFaultsAck_Unauthenticated_Returns401(t *testing.T) {
	router, _, _ := newFaultsTestRouter(t)
	w := doFaultsRequest(router, http.MethodPost, "/api/v1/faults/f1/ack", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 body=%s", w.Code, w.Body.String())
	}
}

func TestFaultsAck_NotFound_Returns404(t *testing.T) {
	router, _, cookie := newFaultsTestRouter(t)
	w := doFaultsRequest(router, http.MethodPost, "/api/v1/faults/missing/ack", cookie)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 body=%s", w.Code, w.Body.String())
	}
}

func TestFaultsAck_FirstAck_WritesTimelineAndReturnsFault(t *testing.T) {
	router, fake, cookie := newFaultsTestRouter(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fake.faults["f1"] = domain.Fault{
		ID: "f1", CorrelationKey: "ck-1", Status: domain.FaultStatusActive,
		Kind: "capacity_gap", Title: "host down", OwnerTeamID: "team-1",
	}
	router.clock = &fakeClock{now: now}

	w := doFaultsRequest(router, http.MethodPost, "/api/v1/faults/f1/ack", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}

	var body struct {
		Fault domain.Fault `json:"fault"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Fault.AckedBy != "u1" || body.Fault.AckedByName != "member1" || body.Fault.AckedAt.IsZero() {
		t.Fatalf("acked 字段未写入: %+v", body.Fault)
	}
	if body.Fault.OwnerTeamID != "team-1" {
		t.Fatalf("owner_team_id 应原样返回: %+v", body.Fault)
	}
	if fake.ackCalls != 1 || len(fake.timeline) != 1 {
		t.Fatalf("首次 ack 应写 faults + 时间线各一次: ackCalls=%d timeline=%d", fake.ackCalls, len(fake.timeline))
	}
	if fake.timeline[0].Action != domain.FaultTimelineActionAck || fake.timeline[0].Actor != "member1" || fake.timeline[0].ActorID != "u1" {
		t.Fatalf("时间线条目不符合不变量 7: %+v", fake.timeline[0])
	}
}

func TestFaultsAck_AlreadyAcked_Idempotent200_NoTimelineRewrite(t *testing.T) {
	router, fake, cookie := newFaultsTestRouter(t)
	ackedAt := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	fake.faults["f1"] = domain.Fault{
		ID: "f1", CorrelationKey: "ck-1", Status: domain.FaultStatusActive,
		AckedBy: "other", AckedByName: "前人", AckedAt: ackedAt,
	}

	w1 := doFaultsRequest(router, http.MethodPost, "/api/v1/faults/f1/ack", cookie)
	w2 := doFaultsRequest(router, http.MethodPost, "/api/v1/faults/f1/ack", cookie)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("重复 ack 应 200，got %d / %d", w1.Code, w2.Code)
	}

	var body struct {
		Fault domain.Fault `json:"fault"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Fault.AckedBy != "other" || body.Fault.AckedByName != "前人" {
		t.Fatalf("重复 ack 不得覆盖认领人: %+v", body.Fault)
	}
	if fake.ackCalls != 0 || len(fake.timeline) != 0 {
		t.Fatalf("重复 ack 不得调 store.Ack 或写时间线: ackCalls=%d timeline=%d", fake.ackCalls, len(fake.timeline))
	}
}

func TestFaultsAck_StoreErrorDoesNotWriteTimeline(t *testing.T) {
	router, fake, cookie := newFaultsTestRouter(t)
	fake.faults["f1"] = domain.Fault{ID: "f1", CorrelationKey: "ck-1", Status: domain.FaultStatusActive}
	fake.appendErr = errors.New("append boom")

	w := doFaultsRequest(router, http.MethodPost, "/api/v1/faults/f1/ack", cookie)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 body=%s", w.Code, w.Body.String())
	}
}
