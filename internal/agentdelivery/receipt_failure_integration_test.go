//go:build integration

package agentdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// droppedPoll makes the response write fail before the consumer receives any data.
type droppedPoll struct {
	*httptest.ResponseRecorder
	attempted bool
}

func (w *droppedPoll) Write([]byte) (int, error) { w.attempted = true; return 0, io.ErrClosedPipe }

func TestLostPollAndCrashAfterAck(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx := t.Context()
	queue := NewQueue(st)
	api := testAPI(ctx, queue)
	const nextToken = "second-test-token"
	api.config.principals[sha256.Sum256([]byte(nextToken))] = Principal{ClientID: "second", Role: "ops"}
	env := Envelope{SchemaVersion: 1, TraceID: "lost-response", DedupKey: "group"}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return queue.Enqueue(ctx, tx, env, Policy{Role: "ops", RouteID: "test", TTL: time.Hour, MaxAttempts: 2})
	}); err != nil {
		t.Fatal(err)
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/api/v1/agent/poll", strings.NewReader(`{"client_id":"worker","wait":0}`))
	firstReq.Header.Set("Authorization", "Bearer test-token")
	dropped := &droppedPoll{ResponseRecorder: httptest.NewRecorder()}
	api.ServeHTTP(dropped, firstReq)
	if !dropped.attempted || dropped.Body.Len() != 0 {
		t.Fatal("expected an interrupted response with no bytes delivered")
	}
	// Capture the committed first receipt for a subsequent stale-request check.
	// The consumer never saw it and cannot ACK during the lost response.
	var first Receipt
	if err := st.Pool.QueryRow(ctx, `SELECT id::text,lease_id::text,attempts FROM agent_messages WHERE role='ops' AND status='leased' AND lease_owner='worker'`).Scan(&first.MessageID, &first.LeaseID, &first.Attempt); err != nil {
		t.Fatal(err)
	}
	if first.Attempt != 1 {
		t.Fatalf("first attempt = %d", first.Attempt)
	}

	const secondPoll = `{"client_id":"second","wait":0}`
	if w := postAPI(api, "poll", nextToken, secondPoll); w.Code != http.StatusNoContent {
		t.Fatalf("lease was handed out before timeout: HTTP %d", w.Code)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE agent_messages SET lease_expires_at=now() WHERE id=$1`, first.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := queue.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE agent_messages SET visible_at=now() WHERE id=$1`, first.MessageID); err != nil {
		t.Fatal(err)
	}
	w := postAPI(api, "poll", nextToken, secondPoll)
	var res struct {
		Messages []Delivery `json:"messages"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil || len(res.Messages) != 1 {
		t.Fatalf("redelivery: HTTP %d %s", w.Code, w.Body.String())
	}
	second := res.Messages[0]
	if second.MessageID != first.MessageID || second.Envelope.TraceID != env.TraceID || second.Attempt != 2 || second.LeaseID == first.LeaseID {
		t.Fatalf("redelivered wrong message or generation: %#v", second.Receipt)
	}
	ack := func(token string, receipt Receipt, want int) {
		t.Helper()
		body, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if w := postAPI(api, "ack", token, string(body)); w.Code != want {
			t.Fatalf("ack attempt %d returned %d, want %d", receipt.Attempt, w.Code, want)
		}
	}
	ack("test-token", first, http.StatusConflict)
	ack(nextToken, second.Receipt, http.StatusNoContent)

	// Simulate the handler process disappearing immediately after the ACK reply.
	// A fresh client request has no receipt or local spool; only DB state remains.
	if w := postAPI(api, "poll", nextToken, secondPoll); w.Code != http.StatusNoContent {
		t.Fatalf("post-ACK restart leased a task before TTL: HTTP %d", w.Code)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE agent_messages SET expires_at=now() WHERE id=$1`, first.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := queue.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if w := postAPI(api, "poll", nextToken, secondPoll); w.Code != http.StatusNoContent {
		t.Fatalf("post-ACK restart leased a task: HTTP %d", w.Code)
	}
	var status string
	var claims, acks int
	if err := st.Pool.QueryRow(ctx, `SELECT m.status,count(*) FILTER (WHERE e.kind='claim'),count(*) FILTER (WHERE e.kind='ack')
        FROM agent_messages m JOIN agent_message_events e ON e.message_id=m.id WHERE m.id=$1 GROUP BY m.status`, first.MessageID).Scan(&status, &claims, &acks); err != nil {
		t.Fatal(err)
	}
	if status != "acked" || claims != 2 || acks != 1 {
		t.Fatalf("unexpected terminal state %s, claims %d, acks %d", status, claims, acks)
	}
}
