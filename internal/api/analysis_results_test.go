package api

import (
	"alert-system/internal/agentprotocol"
	"alert-system/internal/omaruntime"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type analysisReadTx struct {
	pgx.Tx
	readOnly bool
}

func (t *analysisReadTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if sql == "SET TRANSACTION READ ONLY" {
		t.readOnly = true
	}
	return pgconn.NewCommandTag("SET"), nil
}

type analysisReadRunner struct{}

func (analysisReadRunner) WithTx(_ context.Context, fn func(pgx.Tx) error) error {
	return fn(&analysisReadTx{})
}

type fakeAnalysisRepo struct {
	called  int
	filter  omaruntime.Filter
	missing bool
}

func (f *fakeAnalysisRepo) List(_ context.Context, tx pgx.Tx, filter omaruntime.Filter) ([]omaruntime.Run, error) {
	f.called++
	f.filter = filter
	if !tx.(*analysisReadTx).readOnly {
		panic("result query not READ ONLY")
	}
	return nil, nil
}
func (f *fakeAnalysisRepo) Result(_ context.Context, tx pgx.Tx, id string) (omaruntime.Result, error) {
	f.called++
	if !tx.(*analysisReadTx).readOnly {
		panic("result query not READ ONLY")
	}
	if f.missing {
		return omaruntime.Result{}, omaruntime.ErrNotFound
	}
	return omaruntime.Result{Run: omaruntime.Run{ID: id, Envelope: agentprotocol.Envelope{Labels: map[string]string{"secret": "do-not-expose"}}}, Evidence: []omaruntime.Evidence{}, Events: []omaruntime.EventSummary{}}, nil
}
func TestAnalysisResultRoutesAdminOnlyReadOnlyAndNoPrivateEnvelope(t *testing.T) {
	r, _, users, _, members := newTestRouter(time.Hour, time.Hour)
	r.tx = analysisReadRunner{}
	addTestUser(users, "user", "reader", "correct-password", true)
	cookie := doLogin(t, r, "reader", "correct-password")
	repo := &fakeAnalysisRepo{}
	r.RegisterAnalysisResultRoutes(repo)
	request := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}
	if out := request("/api/v1/admin/analysis-runs", nil); out.Code != 401 {
		t.Fatal(out.Code)
	}
	if out := request("/api/v1/admin/analysis-runs", cookie); out.Code != 403 {
		t.Fatal(out.Code)
	}
	if repo.called != 0 {
		t.Fatal("unauthorized request reached analysis store")
	}
	members.admins["user"] = true
	out := request("/api/v1/admin/analysis-runs?role=iac&limit=10", cookie)
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"items":[]`) || repo.filter.Limit != 10 {
		t.Fatal(out.Code, out.Body.String())
	}
	if out = request("/api/v1/admin/analysis-runs?alert_id=not-a-uuid", cookie); out.Code != 400 {
		t.Fatal(out.Code)
	}
	id := "11111111-1111-4111-8111-111111111111"
	out = request("/api/v1/admin/analysis-runs/"+id, cookie)
	if out.Code != 200 || strings.Contains(out.Body.String(), "do-not-expose") || strings.Contains(out.Body.String(), "receipt") {
		t.Fatal(out.Code, out.Body.String())
	}
	var result map[string]any
	if e := json.Unmarshal(out.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	repo.missing = true
	if out = request("/api/v1/admin/analysis-runs/"+id, cookie); out.Code != 404 {
		t.Fatal(out.Code)
	}
}
