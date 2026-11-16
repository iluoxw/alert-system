//go:build integration

package omaruntime

import (
	"alert-system/internal/agentdelivery"
	"alert-system/internal/agentprotocol"
	"alert-system/internal/domain"
	"alert-system/internal/store"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := t.Context()
	pg, e := testcontainers.Run(ctx, "postgres:16-alpine", testcontainers.WithEnv(map[string]string{"POSTGRES_PASSWORD": "test"}), testcontainers.WithExposedPorts("5432/tcp"), testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2)))
	if e != nil {
		t.Fatal(e)
	}
	testcontainers.CleanupContainer(t, pg)
	endpoint, e := pg.PortEndpoint(ctx, "5432/tcp", "")
	if e != nil {
		t.Fatal(e)
	}
	st, e := store.New(ctx, "postgres://postgres:test@"+endpoint+"/postgres?sslmode=disable")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(st.Close)
	files, e := filepath.Glob("../../migrations/*.up.sql")
	if e != nil || len(files) == 0 {
		t.Fatal("no migrations")
	}
	for _, f := range files {
		raw, e := os.ReadFile(f)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = st.Pool.Exec(ctx, string(raw)); e != nil {
			t.Fatalf("%s: %v", f, e)
		}
	}
	return st
}

type fakeModel struct {
	acceptSendError               bool
	mu                            sync.Mutex
	byRun                         map[string]string
	events                        map[string][]Event
	creates, sends, subscriptions int
	acceptCreateError             bool
	rejectCreate                  bool
	failRead                      bool
	omitSecondPost                bool
}

func newFakeModel() *fakeModel {
	return &fakeModel{byRun: map[string]string{}, events: map[string][]Event{}}
}
func (m *fakeModel) add(session, kind, text, stop string) {
	m.events[session] = append(m.events[session], Event{ID: uuid.NewString(), Type: kind, Text: text, StopReason: stop, ProcessedAt: time.Now().UTC()})
}
func (m *fakeModel) Create(_ context.Context, input SessionRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates++
	if m.rejectCreate {
		return "", errors.New("fake 503")
	}
	session := "sesn_" + input.RunID
	m.byRun[input.RunID] = session
	raw, _ := json.Marshal(input)
	m.add(session, "user.message", string(raw), "")
	var in struct {
		Allowed []string `json:"allowed_query_ids"`
	}
	_ = json.Unmarshal(input.Input, &in)
	q := []string{"sources"}
	if len(in.Allowed) > 0 {
		q = in.Allowed[:1]
	}
	intent, _ := json.Marshal(Intent{Schema: IntentSchema, QueryIDs: q, Rationale: "fixed observation"})
	m.add(session, "agent.message", string(intent), "")
	m.add(session, "session.status_idle", "", "end_turn")
	if m.acceptCreateError {
		return "", errors.New("response lost after accepted")
	}
	return session, nil
}
func (m *fakeModel) Find(_ context.Context, id string, _ time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead {
		return "", false, errors.New("fake 503")
	}
	s, ok := m.byRun[id]
	return s, ok, nil
}
func (m *fakeModel) Send(_ context.Context, session string, input Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends++
	if m.omitSecondPost {
		return errors.New("fake unavailable before acceptance")
	}
	raw, _ := json.Marshal(input)
	m.add(session, "user.message", string(raw), "")
	var evidence struct {
		Evidence EvidenceBundle `json:"evidence"`
	}
	if e := json.Unmarshal(input.Input, &evidence); e != nil {
		return e
	}
	report := systemReport("", "model_answer")
	report.ReasonCode = ""
	report.Summary = "Observed current snapshot only"
	report.Verdict = "unknown"
	report.EvidenceGaps = []string{}
	for _, item := range evidence.Evidence.Items {
		if item.Status == "ok" {
			report.Facts = append(report.Facts, Fact{EvidenceID: item.ID, Statement: "current numeric sample available"})
		}
	}
	if len(report.Facts) > 0 {
		report.Verdict = "observed_healthy"
	}
	out, _ := json.Marshal(report)
	m.add(session, "agent.message", string(out), "")
	m.add(session, "session.status_idle", "", "end_turn")
	if m.acceptSendError {
		return errors.New("accepted second POST response lost")
	}
	return nil
}
func (m *fakeModel) Events(_ context.Context, session string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead {
		return nil, errors.New("fake 503")
	}
	return append([]Event{}, m.events[session]...), nil
}
func (m *fakeModel) Subscribe(ctx context.Context, session, _ string, emit func(Event) error) error {
	m.mu.Lock()
	m.subscriptions++
	events := append([]Event{}, m.events[session]...)
	fail := m.failRead
	m.mu.Unlock()
	if e := emit(Event{Type: "runtime.stream_connected"}); e != nil {
		return e
	}
	if !fail {
		for _, event := range events {
			if e := emit(event); e != nil {
				return e
			}
		}
	}
	<-ctx.Done()
	return ctx.Err()
}
func goodCollector(_ context.Context, ids []string) (json.RawMessage, error) {
	items := []map[string]any{}
	for _, id := range ids {
		status := "ok"
		if id == "entry_coverage" || id == "vip_coverage" {
			status = "unknown"
		}
		items = append(items, map[string]any{"schema": "iac_evidence_v1", "query_id": id, "status": status, "observed_at": time.Now().UTC(), "data": map[string]any{"samples": []any{map[string]int{"value": 1}}, "url": "https://secret.invalid/hook/token"}})
	}
	return json.Marshal(map[string]any{"schema": "iac_evidence_v1", "items": items, "partial": false})
}

type fixture struct {
	st               *store.Store
	worker           *Worker
	model            *fakeModel
	queue            *agentdelivery.Queue
	env              agentprotocol.Envelope
	alertID, faultID string
	role             string
}

func setupFixture(t *testing.T, st *store.Store, alertname, rule string) *fixture {
	t.Helper()
	ctx := t.Context()
	q := agentdelivery.NewQueue(st)
	role := "iac-" + uuid.NewString()[:8]
	sourceID := uuid.NewString()
	at := time.Now().UTC().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	fp := uuid.NewString()
	var alert domain.Alert
	var fault domain.Fault
	e := st.WithTx(ctx, func(tx pgx.Tx) error {
		in, e := store.NewIntegrationRepo().Create(ctx, tx, store.Integration{Kind: "grafana", Name: role, HookIDHash: strings.ReplaceAll(sourceID, "-", "") + strings.ReplaceAll(sourceID, "-", ""), DefaultSeverity: "warning", Enabled: true})
		if e != nil {
			return e
		}
		sourceID = in.ID
		alert, _, _, e = store.NewAlertRepo().UpsertAuthoritativeOccurrence(ctx, tx, domain.AlertEvent{Fingerprint: uuid.NewString(), UpstreamFingerprint: fp, Source: "grafana", Status: domain.StatusFiring, Severity: domain.SeverityWarning, RuleID: alertname, Title: alertname, StartsAt: at, ResolvePolicy: domain.ResolvePolicySource, Labels: map[string]string{"sre.rule.id": rule}}, sourceID, time.Now(), 0)
		if e != nil {
			return e
		}
		fault, _, e = store.NewFaultRepo().Upsert(ctx, tx, domain.Fault{CorrelationKey: role, Scope: domain.FaultScopeCapability, Status: domain.FaultStatusActive, Severity: domain.SeverityWarning, Kind: "iac_test", Title: role, EntityRef: role, Confidence: 1, FirstDetected: at, LastEvaluated: at})
		if e != nil {
			return e
		}
		return store.NewFaultRepo().UpsertAutomatedEvidence(ctx, tx, fault.ID, domain.FaultEvidence{AlertID: alert.ID, Role: domain.EvidenceRoleDirectImpact, Rationale: "fixture", Confidence: 1})
	})
	if e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(GroupPayload{Origin: "webhook", Alerts: []Member{{Ref: agentprotocol.OccurrenceRef("grafana", sourceID, fp, at), RuleID: rule, Alertname: alertname, Status: "firing", Severity: "warning", StartsAt: at}}})
	env := agentprotocol.Envelope{SchemaVersion: 1, Source: "grafana", SourceID: sourceID, EventType: "alert.firing", DedupKey: uuid.NewString(), Severity: "warning", Payload: payload}
	if e := st.WithTx(ctx, func(tx pgx.Tx) error {
		return q.Enqueue(ctx, tx, env, agentdelivery.Policy{Role: role, TTL: time.Hour, MaxAttempts: 5})
	}); e != nil {
		t.Fatal(e)
	}
	model := newFakeModel()
	worker, e := NewWorker(st, q, model, goodCollector, Options{Principal: agentdelivery.Principal{Role: role, ClientID: "worker"}, WorkerID: uuid.NewString(), SourceIDs: []string{sourceID}, QueryIDs: []string{"sources", "freshness", "traffic", "entry_coverage", "vip_coverage"}, Concurrency: 1, PollInterval: 100 * time.Millisecond, Lease: 6 * time.Second, RoundTimeout: time.Second, RunTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{st, worker, model, q, env, alert.ID, fault.ID, role}
}
func (f *fixture) step(t *testing.T) {
	t.Helper()
	if worked, e := f.worker.Step(t.Context()); e != nil || !worked {
		t.Fatalf("step worked=%v error=%v", worked, e)
	}
}
func (f *fixture) current(t *testing.T) Run {
	t.Helper()
	var runs []Run
	if e := f.st.WithTx(t.Context(), func(tx pgx.Tx) error {
		var e error
		runs, e = f.worker.Repo.List(t.Context(), tx, Filter{Role: f.role})
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if len(runs) != 1 {
		t.Fatalf("runs=%d", len(runs))
	}
	return runs[0]
}
func (f *fixture) result(t *testing.T) Result {
	t.Helper()
	run := f.current(t)
	var out Result
	if e := f.st.WithTx(t.Context(), func(tx pgx.Tx) error { var e error; out, e = f.worker.Repo.Result(t.Context(), tx, run.ID); return e }); e != nil {
		t.Fatal(e)
	}
	return out
}
func TestRuntimeAtomicReceiptCrashAndResume(t *testing.T) {
	st := testStore(t)
	f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
	ctx := t.Context()
	deliveries, e := f.queue.Claim(ctx, f.worker.Options.Principal, "test", 1)
	if e != nil || len(deliveries) != 1 {
		t.Fatal(e)
	}
	old := deliveries[0]
	tx, e := st.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = f.worker.Repo.Accept(ctx, tx, f.worker.Options.Principal, old); e != nil {
		t.Fatal(e)
	}
	var pid int32
	if e = tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); e != nil {
		t.Fatal(e)
	}
	// Kill the connection after inbox INSERT but before AckTx: neither inbox nor ACK may commit.
	if _, e = st.Pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e == nil {
		t.Fatal("killed transaction committed")
	}
	var count int
	if e = st.Pool.QueryRow(ctx, `SELECT count(*) FROM oma_analysis_runs WHERE role=$1`, f.role).Scan(&count); e != nil || count != 0 {
		t.Fatal("unacked inbox committed", count, e)
	}
	if _, e = st.Pool.Exec(ctx, `UPDATE agent_messages SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, old.MessageID); e != nil {
		t.Fatal(e)
	}
	if e = f.queue.Reap(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = st.Pool.Exec(ctx, `UPDATE agent_messages SET visible_at=now()-interval '1 second' WHERE id=$1`, old.MessageID); e != nil {
		t.Fatal(e)
	}
	f.step(t)
	run := f.current(t)
	if run.State != "received" || run.Receipt.Attempt != 2 {
		t.Fatalf("resume=%+v", run)
	}
	if e = f.queue.Ack(ctx, f.worker.Options.Principal, old.Receipt); !errors.Is(e, agentdelivery.ErrLeaseConflict) {
		t.Fatal("old receipt accepted", e)
	}
	// Simulate a new worker after the atomic transfer committed but before model execution.
	f.worker.Options.WorkerID = uuid.NewString()
	f.step(t)
	result := f.result(t)
	if result.Run.State != "completed" || result.Report == nil || f.model.creates != 1 || f.model.sends != 1 || result.Run.StreamConnections < 1 {
		t.Fatalf("result=%+v creates=%d sends=%d", result, f.model.creates, f.model.sends)
	}
	if len(result.Run.AlertIDs) != 1 || result.Run.AlertIDs[0] != f.alertID || len(result.Run.FaultIDs) != 1 || result.Run.FaultIDs[0] != f.faultID {
		t.Fatal("occurrence association failed", result.Run)
	}
	var state string
	if e = st.Pool.QueryRow(ctx, `SELECT status FROM faults WHERE id=$1`, f.faultID).Scan(&state); e != nil || state != "active" {
		t.Fatal("analysis changed domain", state, e)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "secret.invalid") || strings.Contains(string(raw), "lease_id") {
		t.Fatal("public result leaked private data")
	}
}
func TestRuntimeSubmissionRecoveryAndPartial(t *testing.T) {
	st := testStore(t)
	t.Run("create accepted response lost reconciles without second POST", func(t *testing.T) {
		f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
		f.model.acceptCreateError = true
		f.step(t)
		f.step(t)
		if f.result(t).Run.State != "completed" || f.model.creates != 1 {
			t.Fatal("create repeated")
		}
	})
	t.Run("unknown create is terminal and never blindly resent", func(t *testing.T) {
		f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
		f.model.rejectCreate = true
		f.step(t)
		f.step(t)
		if f.result(t).Run.State != "submission_unknown" {
			t.Fatal(f.result(t))
		}
		worked, e := f.worker.Step(t.Context())
		if e != nil || worked || f.model.creates != 1 {
			t.Fatal("blind resubmit", worked, e)
		}
	})
	t.Run("second POST unknown is partial without replay", func(t *testing.T) {
		f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
		f.model.omitSecondPost = true
		f.step(t)
		f.step(t)
		if f.result(t).Run.State != "submission_unknown" || f.model.sends != 1 {
			t.Fatal(f.result(t))
		}
	})
	t.Run("accepted second POST response lost is reconciled once", func(t *testing.T) {
		f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
		f.model.acceptSendError = true
		f.step(t)
		f.step(t)
		if f.result(t).Run.State != "completed" || f.model.sends != 1 {
			t.Fatal(f.result(t))
		}
	})
	t.Run("model read 503 is bounded and records partial", func(t *testing.T) {
		f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
		f.model.failRead = true
		f.step(t)
		f.step(t)
		out := f.result(t)
		if out.Run.State != "partial" || out.Report.ReasonCode != "model_intent_unavailable" {
			t.Fatal(out)
		}
	})
	t.Run("broker failure persists error evidence and forces unknown", func(t *testing.T) {
		f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
		f.worker.Collect = func(context.Context, []string) (json.RawMessage, error) {
			return nil, errors.New("fake broker unavailable")
		}
		f.step(t)
		f.step(t)
		out := f.result(t)
		if out.Run.State != "partial" || out.Report.Verdict != "unknown" || len(out.Evidence) != 1 || out.Evidence[0].Status != "error" {
			t.Fatal(out)
		}
	})
	t.Run("DatasourceError cannot become business healthy", func(t *testing.T) {
		f := setupFixture(t, st, "DatasourceError", "iac-go-system-errors")
		f.step(t)
		f.step(t)
		out := f.result(t)
		if out.Run.State != "partial" || out.Report.Verdict != "unknown" {
			t.Fatal(out)
		}
	})
	t.Run("shared probe required unknown is not optional to model", func(t *testing.T) {
		f := setupFixture(t, st, "IACProbe", "iac-api-probe-not-all-200")
		f.step(t)
		f.step(t)
		out := f.result(t)
		if out.Run.State != "partial" || out.Report.Verdict != "unknown" {
			t.Fatal(out)
		}
		found := false
		for _, e := range out.Evidence {
			if e.QueryID == "entry_coverage" && e.Status == "unknown" {
				found = true
			}
		}
		if !found {
			t.Fatal("model omitted mandatory coverage")
		}
	})
}

func TestRuntimeDurableActionFencingAndReportIdempotency(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
	f.step(t)
	var run Run
	var action Action
	if e := st.WithTx(ctx, func(tx pgx.Tx) error {
		var e error
		run, e = f.worker.Repo.Acquire(ctx, tx, f.role, "old-worker", time.Minute)
		if e != nil {
			return e
		}
		action, _, e = f.worker.Repo.Action(ctx, tx, run, "create_session", map[string]any{"allowed_query_ids": []string{"sources"}})
		if e != nil {
			return e
		}
		return f.worker.Repo.ActionState(ctx, tx, run, "create_session", "submitting")
	}); e != nil {
		t.Fatal(e)
	}
	// HTTP succeeded but process died before session_id checkpoint: recovery finds the same session.
	if _, e := f.model.Create(ctx, SessionRequest{RunID: run.ID, SubmissionID: action.SubmissionID, Input: action.Request}); e != nil {
		t.Fatal(e)
	}
	if _, e := st.Pool.Exec(ctx, `UPDATE oma_analysis_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, run.ID); e != nil {
		t.Fatal(e)
	}
	f.step(t)
	out := f.result(t)
	if out.Run.State != "completed" || f.model.creates != 1 {
		t.Fatal("created duplicate session", out)
	}
	if e := st.WithTx(ctx, func(tx pgx.Tx) error { return f.worker.Repo.Finish(ctx, tx, run, systemReport("failed", "stale")) }); !errors.Is(e, ErrFence) {
		t.Fatal("old execution overwrote current result", e)
	}
	// A new fixture verifies report replay with the same active fencing token and immutable content.
	g := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
	g.step(t)
	var lease Run
	if e := st.WithTx(ctx, func(tx pgx.Tx) error {
		var e error
		lease, e = g.worker.Repo.Acquire(ctx, tx, g.role, "worker", time.Minute)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	report := systemReport("partial", "fixture")
	for i := 0; i < 2; i++ {
		if e := st.WithTx(ctx, func(tx pgx.Tx) error { return g.worker.Repo.Finish(ctx, tx, lease, report) }); e != nil {
			t.Fatal(e)
		}
	}
	changed := report
	changed.Summary = "different report must not replace committed one"
	if e := st.WithTx(ctx, func(tx pgx.Tx) error { return g.worker.Repo.Finish(ctx, tx, lease, changed) }); !errors.Is(e, ErrReportConflict) {
		t.Fatal("report conflict not rejected", e)
	}
	var count int
	if e := st.Pool.QueryRow(ctx, `SELECT count(*) FROM analysis_results WHERE run_id=$1`, lease.ID).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	// SSE and GET replay of an event share a unique persisted identity and cursor.
	lease.SessionID = "sesn_dedup"
	event := Event{ID: "event-fixed", Type: "agent.message", Text: `{"value":1}`}
	for i := 0; i < 2; i++ {
		if e := st.WithTx(ctx, func(tx pgx.Tx) error {
			inserted, e := g.worker.Repo.AppendEvent(ctx, tx, lease, event)
			if e == nil && inserted != (i == 0) {
				t.Error("duplicate event was not deduplicated")
			}
			return e
		}); e != nil {
			t.Fatal(e)
		}
	}
	if e := st.Pool.QueryRow(ctx, `SELECT count(*) FROM oma_analysis_events WHERE run_id=$1`, lease.ID).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}

type gatedModel struct {
	Model
	entered chan struct{}
	release chan struct{}
}

func (g *gatedModel) Create(ctx context.Context, input SessionRequest) (string, error) {
	g.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-g.release:
		return g.Model.Create(ctx, input)
	}
}
func TestRuntimeClaimsOnlyWhenExecutionSlotAvailable(t *testing.T) {
	st := testStore(t)
	f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
	ctx := t.Context()
	for i := 0; i < 2; i++ {
		env := f.env
		env.DedupKey = uuid.NewString()
		if e := st.WithTx(ctx, func(tx pgx.Tx) error {
			return f.queue.Enqueue(ctx, tx, env, agentdelivery.Policy{Role: f.role, TTL: time.Hour, MaxAttempts: 3})
		}); e != nil {
			t.Fatal(e)
		}
	}
	gate := &gatedModel{Model: f.model, entered: make(chan struct{}, 3), release: make(chan struct{})}
	opts := f.worker.Options
	opts.Concurrency = 2
	worker, e := NewWorker(st, f.queue, gate, f.worker.Collect, opts)
	if e != nil {
		t.Fatal(e)
	}
	f.worker = worker
	runCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() { defer close(finished); f.worker.Run(runCtx) }()
	defer cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("execution slots did not fill")
		}
	}
	if !f.worker.Ready() {
		t.Fatal("both active slots should have real successful progress")
	}
	var pending, acked, runs int
	if e := st.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='pending'),count(*) FILTER(WHERE status='acked') FROM agent_messages WHERE role=$1`, f.role).Scan(&pending, &acked); e != nil {
		t.Fatal(e)
	}
	if e := st.Pool.QueryRow(ctx, `SELECT count(*) FROM oma_analysis_runs WHERE role=$1`, f.role).Scan(&runs); e != nil {
		t.Fatal(e)
	}
	if pending != 1 || acked != 2 || runs != 2 {
		t.Fatalf("overclaim: pending=%d acked=%d runs=%d", pending, acked, runs)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop with in-flight model context")
	}
}

func TestWorkerReadinessDoesNotFakeMissingSchemaOrFailedClaim(t *testing.T) {
	st := testStore(t)
	f := setupFixture(t, st, "IACSystemErrors", "iac-go-system-errors")
	ctx := t.Context()
	w := f.worker
	w.slotRunning(ctx, true)
	if w.Ready() {
		t.Fatal("worker has not polled")
	}
	if _, e := st.Pool.Exec(ctx, `ALTER TABLE oma_analysis_runs RENAME TO missing_analysis_schema`); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Step(ctx); e == nil || w.Ready() {
		t.Fatal("missing schema fabricated readiness", e)
	}
	if _, e := st.Pool.Exec(ctx, `ALTER TABLE missing_analysis_schema RENAME TO oma_analysis_runs`); e != nil {
		t.Fatal(e)
	}
	if _, e := st.Pool.Exec(ctx, `ALTER TABLE agent_consumers RENAME TO missing_claim_schema`); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Step(ctx); e == nil || w.Ready() {
		t.Fatal("failed claim fabricated readiness", e)
	}
	if _, e := st.Pool.Exec(ctx, `ALTER TABLE missing_claim_schema RENAME TO agent_consumers`); e != nil {
		t.Fatal(e)
	}
	w.Options.Principal.Role = "empty-" + uuid.NewString()[:8]
	if worked, e := w.Step(ctx); e != nil || worked || !w.Ready() {
		t.Fatal("successful idle poll not ready", worked, e)
	}
}
