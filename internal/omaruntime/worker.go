package omaruntime

import (
	"alert-system/internal/agentdelivery"
	"alert-system/internal/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"slices"
	"sync"
	"time"
)

type Options struct {
	CCloudL4                                      bool
	QueryProfile                                  func(string) []string
	Principal                                     agentdelivery.Principal
	WorkerID                                      string
	SourceIDs                                     []string
	QueryIDs                                      []string
	Concurrency                                   int
	PollInterval, Lease, RoundTimeout, RunTimeout time.Duration
}

func (o Options) validate() error {
	if o.Principal.Role == "" || o.Principal.ClientID == "" || o.WorkerID == "" || len(o.SourceIDs) < 1 || len(o.QueryIDs) < 1 || o.Concurrency < 1 || o.Concurrency > 8 || o.PollInterval < 100*time.Millisecond || o.PollInterval > 30*time.Second || o.Lease < 3*time.Second || o.Lease > 5*time.Minute || o.RoundTimeout < time.Second || o.RoundTimeout > 5*time.Minute || o.RunTimeout < o.RoundTimeout || o.RunTimeout > 15*time.Minute {
		return errors.New("oma runtime: invalid worker options")
	}
	for _, id := range o.SourceIDs {
		if _, e := uuid.Parse(id); e != nil {
			return errors.New("oma runtime: invalid allowed source")
		}
	}
	return nil
}

type Worker struct {
	progressMu sync.RWMutex
	progress   []slotProgress
	Store      *store.Store
	Repo       *Repository
	Queue      Queue
	Model      Model
	Collect    EvidenceCollector
	Options    Options
	Logger     *slog.Logger
}

func NewWorker(st *store.Store, q Queue, model Model, collect EvidenceCollector, opts Options) (*Worker, error) {
	if e := opts.validate(); e != nil {
		return nil, e
	}
	if st == nil || q == nil || model == nil || collect == nil {
		return nil, errors.New("oma runtime: missing worker dependency")
	}
	return &Worker{Store: st, Repo: NewRepository(), Queue: q, Model: model, Collect: collect, Options: opts, Logger: slog.Default(), progress: make([]slotProgress, opts.Concurrency)}, nil
}
func (w *Worker) tx(ctx context.Context, fn func(pgx.Tx) error) error { return w.Store.WithTx(ctx, fn) }
func (w *Worker) Run(ctx context.Context) {
	var group sync.WaitGroup
	for i := 0; i < w.Options.Concurrency; i++ {
		slot := i
		group.Add(1)
		go func() {
			defer group.Done()
			workerCtx := context.WithValue(ctx, slotKey{}, slot)
			w.slotRunning(workerCtx, true)
			defer w.slotRunning(workerCtx, false)
			w.loop(workerCtx)
		}()
	}
	group.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		worked, e := w.Step(ctx)
		if e != nil && ctx.Err() == nil {
			w.Logger.Warn("OMA analysis worker cycle failed", "error_type", fmt.Sprintf("%T", e))
		}
		if worked && e == nil {
			continue
		}
		timer := time.NewTimer(w.Options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Step first resumes durable work; an idle slot only then claims ONE delivery. No prefetch queue.
func (w *Worker) Step(ctx context.Context) (bool, error) {
	var run Run
	e := w.tx(ctx, func(tx pgx.Tx) error {
		var e error
		run, e = w.Repo.Acquire(ctx, tx, w.Options.Principal.Role, w.Options.WorkerID, w.Options.Lease)
		return e
	})
	if errors.Is(e, ErrNotFound) {
		deliveries, e := w.Queue.Claim(ctx, w.Options.Principal, "oma-worker/v1", 1)
		if e != nil {
			return false, e
		}
		if len(deliveries) == 0 {
			w.recordSuccess(ctx)
			return false, nil
		}
		d := deliveries[0]
		if d.Envelope.ID != "" && d.Envelope.ID != d.MessageID {
			return false, errors.New("oma runtime: delivery identity mismatch")
		}
		// Receipt+envelope persist before ACK. A canceled/failed insert leaves the existing delivery lease recoverable.
		e = w.tx(ctx, func(tx pgx.Tx) error {
			id, e := w.Repo.Accept(ctx, tx, w.Options.Principal, d)
			if e != nil {
				return e
			}
			if e = w.Queue.AckTx(ctx, tx, w.Options.Principal, d.Receipt); e != nil {
				return e
			}
			return w.Repo.Received(ctx, tx, id)
		})
		if e == nil {
			w.recordSuccess(ctx)
		}
		return e == nil, e
	}
	if e != nil {
		return false, e
	}
	w.slotActive(ctx, true)
	defer w.slotActive(ctx, false)
	w.recordSuccess(ctx)
	e = w.executeLeased(ctx, run)
	if e == nil {
		w.recordSuccess(ctx)
	}
	return true, e
}
func (w *Worker) executeLeased(parent context.Context, run Run) error {
	ctx, cancel := context.WithTimeout(parent, w.Options.RunTimeout)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(w.Options.Lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if e := w.tx(ctx, func(tx pgx.Tx) error { return w.Repo.Renew(ctx, tx, run, w.Options.Lease) }); e != nil {
					cancel()
					return
				}
				w.recordSuccess(ctx)
			}
		}
	}()
	e := w.process(ctx, run)
	cancel()
	<-heartbeatDone
	releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	defer releaseCancel()
	_ = w.tx(releaseCtx, func(tx pgx.Tx) error { return w.Repo.Release(releaseCtx, tx, run, w.Options.PollInterval) })
	return e
}
func (w *Worker) load(ctx context.Context, id string) (Run, error) {
	var run Run
	e := w.tx(ctx, func(tx pgx.Tx) error { var e error; run, e = w.Repo.Get(ctx, tx, id); return e })
	return run, e
}
func (w *Worker) transition(ctx context.Context, run Run, state, session, reason string, intent *Intent, evidence *EvidenceBundle) error {
	return w.tx(ctx, func(tx pgx.Tx) error { return w.Repo.State(ctx, tx, run, state, session, reason, intent, evidence) })
}
func (w *Worker) finish(ctx context.Context, run Run, report Report) error {
	return w.tx(ctx, func(tx pgx.Tx) error {
		if e := w.Repo.Bind(ctx, tx, run); e != nil {
			return e
		}
		return w.Repo.Finish(ctx, tx, run, report)
	})
}
func (w *Worker) observation(run Run) (GroupPayload, bool, error) {
	var group GroupPayload
	if run.Envelope.SchemaVersion != 1 || !slices.Contains(w.Options.SourceIDs, run.SourceID) || (run.Envelope.Source != "grafana" && run.Envelope.Source != "alertmanager") || len(run.Envelope.Payload) > 256*1024 || json.Unmarshal(run.Envelope.Payload, &group) != nil || len(group.Alerts) < 1 || len(group.Alerts) > 64 {
		return group, false, errors.New("oma runtime: envelope outside allowed contract")
	}
	observationFault := false
	for _, m := range group.Alerts {
		if len(m.Ref) != 64 || m.StartsAt.IsZero() {
			return group, false, errors.New("oma runtime: invalid occurrence reference")
		}
		if m.Alertname == "DatasourceError" || m.Alertname == "DatasourceNoData" {
			observationFault = true
		}
	}
	return group, observationFault, nil
}
func (w *Worker) process(ctx context.Context, run Run) error {
	if run.Attempts > 5 && run.State != "receiving" {
		return w.finish(ctx, run, systemReport("partial", "analysis_retry_exhausted"))
	}
	if run.State == "receiving" {
		return w.finish(ctx, run, systemReport("failed", "receipt_transfer_incomplete"))
	}

	group, observationFault, e := w.observation(run)
	if e != nil {
		return w.finish(ctx, run, systemReport("failed", "envelope_not_authorized"))
	}
	allowed := append([]string{}, w.Options.QueryIDs...)
	if w.Options.QueryProfile != nil {
		allowed = []string{}
		for _, member := range group.Alerts {
			for _, id := range w.Options.QueryProfile(member.RuleID) {
				if slices.Contains(w.Options.QueryIDs, id) && !slices.Contains(allowed, id) {
					allowed = append(allowed, id)
				}
			}
		}
	}
	required := requiredCoverage(group)
	forcePartial := observationFault || len(required) > 0 || group.TruncatedAlerts > 0
	ccloudCanary := false
	if w.Options.CCloudL4 && !observationFault {
		for _, m := range group.Alerts {
			if m.RuleID == "iac-sre-oma-canary" {
				required = append(required, "ccloud_l4")
				ccloudCanary = true
				break
			}
		}
	}
	if observationFault {
		allowed = []string{}
		for _, id := range []string{"sources", "freshness"} {
			if slices.Contains(w.Options.QueryIDs, id) {
				allowed = append(allowed, id)
			}
		}
	}
	if len(allowed) == 0 {
		return w.finish(ctx, run, systemReport("partial", "no_allowed_observation_query"))
	}
	if run.State == "received" {
		input := map[string]any{"schema": "sre.analysis-request/v1", "role": run.Role, "event_type": run.EventType, "source": run.Envelope.Source,
			"scope": "IAC Go PRO; broker-enforced fixed resources", "group": group, "allowed_query_ids": allowed, "observation_fault": observationFault,
			"constraints": []string{"read-only", "unknown is not healthy", "current samples do not prove historical cause", "no domain lifecycle writes"}}
		if ccloudCanary {
			input["constraints"] = append(input["constraints"].([]string), "This synthetic chain acceptance requires ccloud_l4: cite its current successful evidence_id in facts; if unknown, report the gap without fabricating a fact. Pod counts/Ready are observations only; desired_replicas=null never proves intended replicas or full health.")
		}
		var action Action
		e = w.tx(ctx, func(tx pgx.Tx) error {
			var e error
			action, _, e = w.Repo.Action(ctx, tx, run, "create_session", input)
			return e
		})
		if e != nil {
			return e
		}
		session := ""
		if action.Status == "prepared" {
			if e = w.tx(ctx, func(tx pgx.Tx) error { return w.Repo.ActionState(ctx, tx, run, "create_session", "submitting") }); e != nil {
				return e
			}
			session, e = w.Model.Create(ctx, SessionRequest{RunID: run.ID, SubmissionID: action.SubmissionID, Input: action.Request})
			if e != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if session == "" {
			var found bool
			session, found, e = w.Model.Find(ctx, run.ID, run.CreatedAt)
			if e != nil {
				return e
			}
			if !found {
				return w.uncertain(ctx, run, "create_session", "session_submission_unknown")
			}
		}
		e = w.tx(ctx, func(tx pgx.Tx) error {
			if e := w.Repo.ActionState(ctx, tx, run, "create_session", "submitted"); e != nil {
				return e
			}
			return w.Repo.State(ctx, tx, run, "awaiting_intent", session, "", nil, nil)
		})
		if e != nil {
			return e
		}
		run.SessionID = session
		run.State = "awaiting_intent"
	}
	if run.State == "awaiting_intent" {
		action, e := w.action(ctx, run, "create_session")
		if e != nil {
			return e
		}
		text, e := w.waitRound(ctx, run, action.EventAfter)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			code := "model_intent_unavailable"
			if errors.Is(e, ErrModelTerminated) {
				code = "model_intent_terminated"
			}
			return w.finish(ctx, run, systemReport("partial", code))
		}
		intent, e := parseIntent(text, allowed)
		if e != nil {
			return w.finish(ctx, run, systemReport("partial", "invalid_model_intent"))
		}
		permittedRequired := []string{}
		for _, id := range required {
			if slices.Contains(allowed, id) {
				permittedRequired = append(permittedRequired, id)
			}
		}
		intent.QueryIDs = includeRequiredQueries(intent.QueryIDs, permittedRequired)
		if e = w.transition(ctx, run, "collecting", "", "", &intent, nil); e != nil {
			return e
		}
		run.Intent = &intent
		run.State = "collecting"
	}
	if run.State == "collecting" {
		if run.Intent == nil {
			return w.finish(ctx, run, systemReport("partial", "missing_persisted_intent"))
		}
		evidence := run.Evidence
		if evidence == nil {
			raw, collectErr := w.Collect(ctx, run.Intent.QueryIDs)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			value, e := normalizeEvidence(raw, run.Intent.QueryIDs)
			if collectErr != nil || e != nil {
				value = errorEvidence(run.Intent.QueryIDs)
			}
			for _, id := range required {
				if !slices.Contains(run.Intent.QueryIDs, id) {
					value.Partial = true
					value.CoverageGaps = append(value.CoverageGaps, "required_query_not_allowed:"+id)
				}
			}
			evidence = &value
			if e = w.transition(ctx, run, "collecting", "", "", nil, evidence); e != nil {
				return e
			}
			run.Evidence = evidence
		}
		var action Action
		e = w.tx(ctx, func(tx pgx.Tx) error {
			var e error
			action, _, e = w.Repo.Action(ctx, tx, run, "submit_evidence", map[string]any{"schema": evidence.Schema, "evidence": evidence, "observation_fault": observationFault, "instruction": "Return only sre.analysis-result/v1 JSON, cite successful evidence_id, report unknown/partial when evidence is missing."})
			return e
		})
		if e != nil {
			return e
		}
		submitted := action.Status == "submitted"
		if action.Status == "prepared" {
			if e = w.tx(ctx, func(tx pgx.Tx) error { return w.Repo.ActionState(ctx, tx, run, "submit_evidence", "submitting") }); e != nil {
				return e
			}
			e = w.Model.Send(ctx, run.SessionID, Message{SubmissionID: action.SubmissionID, Input: action.Request})
			if e == nil {
				submitted = true
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if !submitted {
			events, e := w.Model.Events(ctx, run.SessionID)
			if e != nil {
				return e
			}
			if !messageObserved(events, action.SubmissionID) {
				return w.uncertain(ctx, run, "submit_evidence", "evidence_submission_unknown")
			}
			submitted = true
		}
		if submitted {
			e = w.tx(ctx, func(tx pgx.Tx) error {
				if e := w.Repo.ActionState(ctx, tx, run, "submit_evidence", "submitted"); e != nil {
					return e
				}
				return w.Repo.State(ctx, tx, run, "awaiting_report", "", "", nil, nil)
			})
			if e != nil {
				return e
			}
			run.State = "awaiting_report"
		}
	}
	if run.State == "awaiting_report" {
		action, e := w.action(ctx, run, "submit_evidence")
		if e != nil {
			return e
		}
		text, e := w.waitRound(ctx, run, action.EventAfter)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			code := "model_report_unavailable"
			if errors.Is(e, ErrModelTerminated) {
				code = "model_round_terminated"
			}
			return w.finish(ctx, run, systemReport("partial", code))
		}
		if run.Evidence == nil {
			fresh, e := w.load(ctx, run.ID)
			if e != nil {
				return e
			}
			run.Evidence = fresh.Evidence
		}
		if run.Evidence == nil {
			return w.finish(ctx, run, systemReport("partial", "missing_persisted_evidence"))
		}
		report, e := parseReport(text, *run.Evidence, forcePartial)
		if e != nil {
			return w.finish(ctx, run, systemReport("partial", "invalid_or_ungrounded_report"))
		}
		return w.finish(ctx, run, report)
	}
	return nil
}
func (w *Worker) action(ctx context.Context, run Run, stage string) (Action, error) {
	var a Action
	e := w.tx(ctx, func(tx pgx.Tx) error { var e error; a, _, e = w.Repo.Action(ctx, tx, run, stage, nil); return e })
	return a, e
}
func (w *Worker) waitRound(ctx context.Context, run Run, after int64) (string, error) {
	roundCtx, cancel := context.WithTimeout(ctx, w.Options.RoundTimeout)
	defer cancel()
	signals := make(chan struct{}, 1)
	finished := make(chan error, 1)
	streamCtx, streamCancel := context.WithCancel(roundCtx)
	defer streamCancel()
	go func() {
		defer close(finished)
		for streamCtx.Err() == nil {
			fresh, e := w.load(streamCtx, run.ID)
			if e != nil {
				finished <- e
				return
			}
			e = w.Model.Subscribe(streamCtx, run.SessionID, fresh.Cursor, func(event Event) error {
				if event.Type == "runtime.stream_connected" {
					return w.tx(streamCtx, func(tx pgx.Tx) error { return w.Repo.StreamConnected(streamCtx, tx, run) })
				}
				e := w.tx(streamCtx, func(tx pgx.Tx) error { _, e := w.Repo.AppendEvent(streamCtx, tx, run, event); return e })
				if e == nil {
					select {
					case signals <- struct{}{}:
					default:
					}
				}
				return e
			})
			if errors.Is(e, ErrFence) {
				finished <- e
				return
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-streamCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	streamFinished := finished
	defer func() {
		streamCancel()
		for range streamFinished {
		}
	}() // no detached fire-and-forget; stream belongs to this execution slot.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	reconcile := func() error {
		events, e := w.Model.Events(roundCtx, run.SessionID)
		if e != nil {
			return e
		}
		for _, event := range events {
			if e = w.tx(roundCtx, func(tx pgx.Tx) error { _, e := w.Repo.AppendEvent(roundCtx, tx, run, event); return e }); e != nil {
				return e
			}
		}
		return nil
	}
	// SSE is already running; GET repairs events that occurred between session creation and subscription.
	for {
		_ = reconcile() // read-side 503 is retried only within the bounded round; never repeats POST.
		var events []Event
		e := w.tx(roundCtx, func(tx pgx.Tx) error { var e error; events, e = w.Repo.RoundEvents(roundCtx, tx, run, after); return e })
		if e != nil {
			return "", e
		}
		text, done, roundErr := latestRound(events)
		if roundErr != nil {
			return "", roundErr
		}
		if done && text != "" {
			fresh, e := w.load(roundCtx, run.ID)
			if e != nil {
				return "", e
			}
			if fresh.StreamConnections > 0 {
				return text, nil
			}
		}
		select {
		case <-roundCtx.Done():
			return "", roundCtx.Err()
		case <-signals:
		case <-tick.C:
		case e, ok := <-finished:
			if ok && e != nil {
				return "", e
			}
			finished = nil
		}
	}
}

func (w *Worker) uncertain(ctx context.Context, run Run, stage, code string) error {
	return w.tx(ctx, func(tx pgx.Tx) error {
		if e := w.Repo.ActionState(ctx, tx, run, stage, "unknown"); e != nil {
			return e
		}
		if e := w.Repo.Bind(ctx, tx, run); e != nil {
			return e
		}
		return w.Repo.Finish(ctx, tx, run, systemReport("submission_unknown", code))
	})
}
