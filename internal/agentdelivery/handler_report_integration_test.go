//go:build integration

package agentdelivery

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/agentprotocol"
)

func TestHandlerReportPersistsOnlyOnceForAcknowledgedReceipt(t *testing.T) {
	st := setupDeliveryTestStore(t)
	q := NewQueue(st)
	ctx := t.Context()
	who := Principal{Role: "ops", ClientID: "runner"}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return q.Enqueue(ctx, tx, Envelope{SchemaVersion: 1}, Policy{Role: who.Role, TTL: time.Minute, MaxAttempts: 3})
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := q.Claim(ctx, who, "test", 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim returned %d deliveries: %v", len(batch), err)
	}
	code := 17
	report := agentprotocol.HandlerReport{Receipt: batch[0].Receipt, Result: agentprotocol.HandlerResult{
		Outcome: agentprotocol.HandlerFailed, ExitCode: &code, DurationMS: 42,
	}}
	if err := q.Report(ctx, who, report); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("report before ACK: %v", err)
	}
	if err := q.Ack(ctx, who, report.Receipt); err != nil {
		t.Fatal(err)
	}
	for _, other := range []Principal{{Role: "dev", ClientID: who.ClientID}, {Role: who.Role, ClientID: "other"}} {
		if err := q.Report(ctx, other, report); !errors.Is(err, ErrLeaseConflict) {
			t.Fatalf("report from wrong principal: %v", err)
		}
	}
	wrongAttempt := report
	wrongAttempt.Attempt++
	if err := q.Report(ctx, who, wrongAttempt); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("report from wrong attempt: %v", err)
	}
	// Reporting is allowed after the original lease or TTL ends.
	if _, err := st.Pool.Exec(ctx, `UPDATE agent_messages SET expires_at=now()-interval '1 second' WHERE id=$1`, report.MessageID); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			done <- q.Report(ctx, who, report)
		}()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("report or retry: %v", err)
		}
	}
	conflict := report
	conflict.Result.DurationMS++
	if err := q.Report(ctx, who, conflict); !errors.Is(err, ErrReportConflict) {
		t.Fatalf("conflicting report: %v", err)
	}
	var count int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM agent_message_events WHERE message_id=$1 AND kind='handler_report'`, report.MessageID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("report audit count %d: %v", count, err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		d, err := q.MessageDetail(ctx, tx, report.MessageID)
		if err != nil {
			return err
		}
		if d.DeliveryStatus != "acked" || d.ReportedAt == nil || d.HandlerResult == nil || d.HandlerResult.Outcome != agentprotocol.HandlerFailed ||
			d.HandlerResult.ExitCode == nil || *d.HandlerResult.ExitCode != code || d.HandlerResult.DurationMS != report.Result.DurationMS {
			t.Fatalf("unexpected stored report: %+v", d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
