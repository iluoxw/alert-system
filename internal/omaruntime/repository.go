package omaruntime

import (
	"alert-system/internal/agentdelivery"
	"alert-system/internal/agentprotocol"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sort"
	"time"
)

type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

const runColumns = `id::text,message_id::text,role,client_id,source_id::text,event_type,state,session_id,cursor,stream_connections,last_stream_at,reason_code,alert_ids::text[],fault_ids::text[],binding_status,attempts,created_at,updated_at,receipt,envelope,intent,evidence,lease_token`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var receipt, envelope, intent, evidence []byte
	e := row.Scan(&r.ID, &r.MessageID, &r.Role, &r.ClientID, &r.SourceID, &r.EventType, &r.State, &r.SessionID, &r.Cursor, &r.StreamConnections, &r.LastStreamAt, &r.ReasonCode, &r.AlertIDs, &r.FaultIDs, &r.BindingStatus, &r.Attempts, &r.CreatedAt, &r.UpdatedAt, &receipt, &envelope, &intent, &evidence, &r.LeaseToken)
	if errors.Is(e, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if e != nil {
		return r, e
	}
	for _, p := range []struct {
		raw []byte
		out any
	}{{receipt, &r.Receipt}, {envelope, &r.Envelope}, {intent, &r.Intent}, {evidence, &r.Evidence}} {
		if len(p.raw) > 0 {
			if e = json.Unmarshal(p.raw, p.out); e != nil {
				return r, e
			}
		}
	}
	return r, nil
}
func (r *Repository) Accept(ctx context.Context, tx pgx.Tx, who agentdelivery.Principal, d agentdelivery.Delivery) (string, error) {
	receipt, e := json.Marshal(d.Receipt)
	if e != nil {
		return "", e
	}
	env, e := json.Marshal(d.Envelope)
	if e != nil {
		return "", e
	}
	var id string
	e = tx.QueryRow(ctx, `INSERT INTO oma_analysis_runs(message_id,role,client_id,source_id,event_type,receipt,envelope)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(message_id) DO UPDATE SET
 receipt=CASE WHEN oma_analysis_runs.state='receiving' THEN EXCLUDED.receipt ELSE oma_analysis_runs.receipt END,
 client_id=CASE WHEN oma_analysis_runs.state='receiving' THEN EXCLUDED.client_id ELSE oma_analysis_runs.client_id END
 RETURNING id::text`, d.MessageID, who.Role, who.ClientID, d.Envelope.SourceID, d.Envelope.EventType, receipt, env).Scan(&id)
	return id, e
}
func (r *Repository) Acquire(ctx context.Context, tx pgx.Tx, role, owner string, lease time.Duration) (Run, error) {
	return scanRun(tx.QueryRow(ctx, `WITH candidate AS (SELECT id FROM oma_analysis_runs WHERE role=$1
 AND state IN ('receiving','received','awaiting_intent','collecting','awaiting_report') AND next_attempt_at<=clock_timestamp()
 AND (lease_expires_at IS NULL OR lease_expires_at<=clock_timestamp()) ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
 UPDATE oma_analysis_runs SET lease_owner=$2,lease_token=lease_token+1,lease_expires_at=clock_timestamp()+$3::interval,attempts=attempts+1,updated_at=now()
 WHERE id=(SELECT id FROM candidate) RETURNING `+runColumns, role, owner, fmt.Sprintf("%f seconds", lease.Seconds())))
}
func fence(ctx context.Context, tx pgx.Tx, run Run) error {
	var ok bool
	e := tx.QueryRow(ctx, `SELECT true FROM oma_analysis_runs WHERE id=$1 AND lease_token=$2 AND lease_expires_at>clock_timestamp() FOR UPDATE`, run.ID, run.LeaseToken).Scan(&ok)
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrFence
	}
	return e
}
func (r *Repository) Renew(ctx context.Context, tx pgx.Tx, run Run, lease time.Duration) error {
	tag, e := tx.Exec(ctx, `UPDATE oma_analysis_runs SET lease_expires_at=clock_timestamp()+$3::interval WHERE id=$1 AND lease_token=$2 AND lease_expires_at>clock_timestamp()`, run.ID, run.LeaseToken, fmt.Sprintf("%f seconds", lease.Seconds()))
	if e == nil && tag.RowsAffected() != 1 {
		return ErrFence
	}
	return e
}
func (r *Repository) Release(ctx context.Context, tx pgx.Tx, run Run, delay time.Duration) error {
	_, e := tx.Exec(ctx, `UPDATE oma_analysis_runs SET lease_owner='',lease_expires_at=NULL,next_attempt_at=clock_timestamp()+$3::interval WHERE id=$1 AND lease_token=$2`, run.ID, run.LeaseToken, fmt.Sprintf("%f seconds", delay.Seconds()))
	return e
}
func (r *Repository) State(ctx context.Context, tx pgx.Tx, run Run, state, session, reason string, intent *Intent, evidence *EvidenceBundle) error {
	if e := fence(ctx, tx, run); e != nil {
		return e
	}
	a, e := json.Marshal(intent)
	if e != nil {
		return e
	}
	b, e := json.Marshal(evidence)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE oma_analysis_runs SET state=$3,session_id=CASE WHEN $4<>'' THEN $4 ELSE session_id END,
 reason_code=$5,intent=CASE WHEN $6::jsonb<>'null'::jsonb THEN $6 ELSE intent END,evidence=CASE WHEN $7::jsonb<>'null'::jsonb THEN $7 ELSE evidence END,updated_at=now() WHERE id=$1 AND lease_token=$2`, run.ID, run.LeaseToken, state, session, reason, a, b)
	return e
}
func (r *Repository) Get(ctx context.Context, tx pgx.Tx, id string) (Run, error) {
	return scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM oma_analysis_runs WHERE id=$1`, id))
}
func (r *Repository) Action(ctx context.Context, tx pgx.Tx, run Run, stage string, request any) (Action, bool, error) {
	if e := fence(ctx, tx, run); e != nil {
		return Action{}, false, e
	}
	raw, e := json.Marshal(request)
	if e != nil {
		return Action{}, false, e
	}
	var inserted string
	e = tx.QueryRow(ctx, `INSERT INTO oma_analysis_actions(run_id,stage,submission_id,request,event_after) VALUES($1,$2,$3,$4,(SELECT coalesce(max(sequence),0) FROM oma_analysis_events WHERE run_id=$1)) ON CONFLICT DO NOTHING RETURNING submission_id`, run.ID, stage, run.ID+":"+stage, raw).Scan(&inserted)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return Action{}, false, e
	}
	var a Action
	a.Stage = stage
	e = tx.QueryRow(ctx, `SELECT submission_id,request,status,event_after FROM oma_analysis_actions WHERE run_id=$1 AND stage=$2`, run.ID, stage).Scan(&a.SubmissionID, &a.Request, &a.Status, &a.EventAfter)
	return a, inserted != "", e
}
func (r *Repository) ActionState(ctx context.Context, tx pgx.Tx, run Run, stage, status string) error {
	if e := fence(ctx, tx, run); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `UPDATE oma_analysis_actions SET status=$3,updated_at=now() WHERE run_id=$1 AND stage=$2`, run.ID, stage, status)
	return e
}
func (r *Repository) AppendEvent(ctx context.Context, tx pgx.Tx, run Run, event Event) (bool, error) {
	if e := fence(ctx, tx, run); e != nil {
		return false, e
	}
	if event.ID == "" || len(event.ID) > 256 || len(event.Type) > 100 || len(event.Text) > 256*1024 {
		return false, errors.New("oma runtime: invalid event")
	}
	tag, e := tx.Exec(ctx, `INSERT INTO oma_analysis_events(run_id,session_id,event_id,event_type,text,stop_reason,processed_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(session_id,event_id) DO NOTHING`, run.ID, run.SessionID, event.ID, event.Type, event.Text, event.StopReason, nullableTime(event.ProcessedAt))
	if e != nil {
		return false, e
	}
	// SSE重放不倒退cursor。GET补漏仍重叠扫描并按eventID去重，不假设cursor服务端可回放。
	if tag.RowsAffected() > 0 {
		_, e = tx.Exec(ctx, `UPDATE oma_analysis_runs SET cursor=$2,updated_at=now() WHERE id=$1`, run.ID, event.ID)
	}
	return tag.RowsAffected() > 0, e
}
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func (r *Repository) RoundEvents(ctx context.Context, tx pgx.Tx, run Run, after int64) ([]Event, error) {
	rows, e := tx.Query(ctx, `SELECT event_id,event_type,text,stop_reason,processed_at FROM oma_analysis_events WHERE run_id=$1 AND sequence>$2 ORDER BY sequence`, run.ID, after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var item Event
		var at *time.Time
		if e = rows.Scan(&item.ID, &item.Type, &item.Text, &item.StopReason, &at); e != nil {
			return nil, e
		}
		if at != nil {
			item.ProcessedAt = *at
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Bind uses the shared source-aware occurrence reference contract, including Grafana PG microsecond normalization.
func (r *Repository) Bind(ctx context.Context, tx pgx.Tx, run Run) error {
	if e := fence(ctx, tx, run); e != nil {
		return e
	}
	var group GroupPayload
	if e := json.Unmarshal(run.Envelope.Payload, &group); e != nil {
		return e
	}
	alerts := map[string]bool{}
	matched := 0
	for _, member := range group.Alerts {
		rows, e := tx.Query(ctx, `SELECT id::text,upstream_fingerprint FROM alerts WHERE source_integration_id=$1 AND starts_at=$2 LIMIT 128`, run.SourceID, member.StartsAt.UTC().Truncate(time.Microsecond))
		if e != nil {
			return e
		}
		for rows.Next() {
			var id, fp string
			if e = rows.Scan(&id, &fp); e != nil {
				rows.Close()
				return e
			}
			if agentprotocol.OccurrenceRef(run.Envelope.Source, run.SourceID, fp, member.StartsAt) == member.Ref {
				alerts[id] = true
				matched++
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	alertIDs := []string{}
	for id := range alerts {
		alertIDs = append(alertIDs, id)
	}
	sort.Strings(alertIDs)
	rows, e := tx.Query(ctx, `SELECT DISTINCT fault_id::text FROM fault_alert_links WHERE alert_id=ANY($1::uuid[]) ORDER BY fault_id`, alertIDs)
	if e != nil {
		return e
	}
	faultIDs := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		faultIDs = append(faultIDs, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	status := "complete"
	if matched != len(group.Alerts) {
		status = "partial"
	}
	_, e = tx.Exec(ctx, `UPDATE oma_analysis_runs SET alert_ids=$2,fault_ids=$3,binding_status=$4 WHERE id=$1`, run.ID, alertIDs, faultIDs, status)
	return e
}
func (r *Repository) Finish(ctx context.Context, tx pgx.Tx, run Run, report Report) error {
	if e := fence(ctx, tx, run); e != nil {
		return e
	}
	raw, e := json.Marshal(report)
	if e != nil {
		return e
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var actual string
	e = tx.QueryRow(ctx, `INSERT INTO analysis_results(run_id,schema_version,content_hash,report) VALUES($1,1,$2,$3) ON CONFLICT(run_id) DO UPDATE SET run_id=EXCLUDED.run_id RETURNING content_hash`, run.ID, hash, raw).Scan(&actual)
	if e != nil {
		return e
	}
	if actual != hash {
		return ErrReportConflict
	}
	_, e = tx.Exec(ctx, `UPDATE oma_analysis_runs SET state=$3,reason_code=$4,updated_at=now() WHERE id=$1 AND lease_token=$2`, run.ID, run.LeaseToken, report.Status, report.ReasonCode)
	return e
}
func (r *Repository) List(ctx context.Context, tx pgx.Tx, f Filter) ([]Run, error) {
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 50
	}
	rows, e := tx.Query(ctx, `SELECT `+runColumns+` FROM oma_analysis_runs WHERE ($1='' OR message_id=NULLIF($1,'')::uuid) AND ($2='' OR NULLIF($2,'')::uuid=ANY(alert_ids)) AND ($3='' OR NULLIF($3,'')::uuid=ANY(fault_ids)) AND ($4='' OR source_id=NULLIF($4,'')::uuid) AND ($5='' OR role=$5) ORDER BY created_at DESC,id LIMIT $6`, f.MessageID, f.AlertID, f.FaultID, f.SourceID, f.Role, f.Limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		item, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (r *Repository) Result(ctx context.Context, tx pgx.Tx, id string) (Result, error) {
	run, e := r.Get(ctx, tx, id)
	if e != nil {
		return Result{}, e
	}
	out := Result{Run: run, Evidence: []Evidence{}, Events: []EventSummary{}}
	var raw []byte
	e = tx.QueryRow(ctx, `SELECT report FROM analysis_results WHERE run_id=$1`, id).Scan(&raw)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return out, e
	}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out.Report); e != nil {
			return out, e
		}
	}
	if run.Evidence != nil {
		for _, item := range run.Evidence.Items {
			out.Evidence = append(out.Evidence, item)
		}
	}
	rows, e := tx.Query(ctx, `SELECT event_id,event_type,processed_at FROM oma_analysis_events WHERE run_id=$1 ORDER BY sequence DESC LIMIT 100`, id)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var item EventSummary
		var at *time.Time
		if e = rows.Scan(&item.ID, &item.Type, &at); e != nil {
			return out, e
		}
		if at != nil {
			item.ProcessedAt = *at
		}
		out.Events = append(out.Events, item)
	}
	return out, rows.Err()
}

func (r *Repository) StreamConnected(ctx context.Context, tx pgx.Tx, run Run) error {
	if e := fence(ctx, tx, run); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `UPDATE oma_analysis_runs SET stream_connections=stream_connections+1,last_stream_at=clock_timestamp() WHERE id=$1`, run.ID)
	return e
}

// Received is called in the SAME transaction after Queue.AckTx succeeds.
func (r *Repository) Received(ctx context.Context, tx pgx.Tx, id string) error {
	_, e := tx.Exec(ctx, `UPDATE oma_analysis_runs SET state='received',updated_at=now() WHERE id=$1 AND state='receiving'`, id)
	return e
}
