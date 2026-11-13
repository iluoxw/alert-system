//go:build integration

package agentdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/ingest"
)

// The writer fails after a previous role has already been enqueued in the same batch.
type partialProjection struct{ queue *Queue }

func (w partialProjection) Enqueue(ctx context.Context, tx pgx.Tx, env Envelope, p Policy) error {
	if p.Role == "ops" {
		return errors.New("projection interrupted")
	}
	return w.queue.Enqueue(ctx, tx, env, p)
}

func TestProjectionManagedHookTransactionAndRestart(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx := t.Context()
	queue := NewQueue(st)
	hook := uuid.NewString()
	var source string
	err := st.Pool.QueryRow(ctx, `INSERT INTO integrations(kind,name,hook_id_hash) VALUES ('alertmanager','test-source',$1) RETURNING id::text`, fmt.Sprintf("%x", sha256.Sum256([]byte(hook)))).Scan(&source)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../ingest/testdata/alertmanager_batch.json")
	if err != nil {
		t.Fatal(err)
	}
	old := Routing{SourceIDs: []string{source}, Routes: []Rule{{Name: "old-route", Role: "dev", TTL: time.Minute, MaxAttempts: 2}, {Name: "ops", Role: "ops", TTL: time.Hour, MaxAttempts: 3}}, ConfigVersion: "old-config"}
	p := NewProjector(st, partialProjection{queue}, old, nil)
	post := func(record ingest.BatchRecorder) int {
		t.Helper()
		registry := ingest.NewRegistry()
		registry.Register(ingest.NewAlertmanagerAdapter(domain.SeverityWarning))
		h := ingest.NewServer(registry, ingest.NewPGStore(st).WithBatchRecorder(record), nil)
		req := httptest.NewRequest(http.MethodPost, "/ingest/v1/hook/"+hook, strings.NewReader(string(raw)))
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	count := func(q string, want int) {
		t.Helper()
		var got int
		if err := st.Pool.QueryRow(ctx, q).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: want %d got %d: %v", q, want, got, err)
		}
	}
	// A recorder error after writing the batch must roll back both original events and intent.
	if code := post(func(ctx context.Context, tx pgx.Tx, id string, b json.RawMessage, es []domain.AlertEvent) error {
		if err := p.Record(ctx, tx, id, b, es); err != nil {
			return err
		}
		return errors.New("injected save failure")
	}); code != 500 {
		t.Fatalf("rollback response %d", code)
	}
	count("SELECT count(*) FROM alert_events", 0)
	count("SELECT count(*) FROM agent_batches", 0)
	if code := post(NewProjector(st, queue, Routing{}, nil).Record); code != 202 {
		t.Fatalf("unconfigured source: %d", code)
	}
	count("SELECT count(*) FROM alert_events", 2)
	count("SELECT count(*) FROM agent_batches", 0)
	if code := post(p.Record); code != 202 {
		t.Fatalf("accepted response %d", code)
	}
	count("SELECT count(*) FROM alert_events", 4)
	count("SELECT count(*) FROM agent_batches", 1)
	if found, err := p.projectOne(ctx); !found || err != nil {
		t.Fatalf("failed fan-out: found=%t, %v", found, err)
	}
	count("SELECT count(*) FROM agent_messages", 0)
	count("SELECT count(*) FROM agent_batches WHERE failures=1 AND visible_at>created_at", 1)
	if _, err := st.Pool.Exec(ctx, "UPDATE agent_batches SET visible_at=now()"); err != nil {
		t.Fatal(err)
	}
	// Restart with an unrelated configuration; replay must honour the accepted role and TTL snapshot.
	restarted := NewProjector(st, queue, Routing{}, nil)
	if found, err := restarted.projectOne(ctx); !found || err != nil {
		t.Fatalf("restart: found=%t, %v", found, err)
	}
	if found, err := restarted.projectOne(ctx); found || err != nil {
		t.Fatalf("replay: found=%t, %v", found, err)
	}
	count("SELECT count(*) FROM agent_batches", 0)
	count("SELECT count(*) FROM agent_messages WHERE config_version='old-config' AND status='pending'", 2)
	count("SELECT count(*) FROM agent_messages WHERE role='dev' AND route_id='old-route' AND expires_at-created_at=interval '1 minute'", 1)
}
