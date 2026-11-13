//go:build integration

package agentdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"alert-system/internal/agentprotocol"
	"alert-system/internal/store"
)

func setupDeliveryTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := t.Context()
	pg, err := testcontainers.Run(ctx, "postgres:16-alpine", testcontainers.WithEnv(map[string]string{"POSTGRES_PASSWORD": "test"}), testcontainers.WithExposedPorts("5432/tcp"), testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2)))
	testcontainers.CleanupContainer(t, pg)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := pg.PortEndpoint(ctx, "5432/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, "postgres://postgres:test@"+endpoint+"/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	paths, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("migrations: %v (%d files)", err, len(paths))
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(path), err)
		}
	}
	return st
}

func TestPollResponseByteBudget(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx := t.Context()
	queue := NewQueue(st)
	api := testAPI(ctx, queue)
	// Each repeat contributes 18 bytes on the wire after HTML/Unicode escaping.
	// Two ordinary envelopes fit individually but exceed the budget together.
	for _, row := range []struct {
		key      string
		priority int
		repeats  int
	}{{"oversized", 3, 940000}, {"first", 2, 475000}, {"second", 1, 475000}, {"small", 0, 1}} {
		env := Envelope{SchemaVersion: 1, DedupKey: row.key}
		p := Policy{Role: "ops", Priority: row.priority, TTL: time.Hour, MaxAttempts: 2}
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			if err := queue.Enqueue(ctx, tx, env, p); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE agent_messages SET envelope=jsonb_set(envelope,'{labels}',
				jsonb_build_object('detail',repeat($2::text,$3::int))) WHERE dedup_key=$1`, row.key, "<&\u2028", row.repeats)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	pollKeys := func(want ...string) {
		t.Helper()
		w := postAPI(api, "poll", "test-token", `{"client_id":"worker","wait":0,"max":8}`)
		if w.Code != http.StatusOK || w.Body.Len() > agentprotocol.MaxPollResponseBytes {
			t.Fatalf("HTTP %d, body bytes %d", w.Code, w.Body.Len())
		}
		var res struct {
			Messages []Delivery `json:"messages"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || len(res.Messages) != len(want) {
			t.Fatalf("expected %d deliveries, got %d: %v", len(want), len(res.Messages), err)
		}
		for i, key := range want {
			if res.Messages[i].Envelope.DedupKey != key || res.Messages[i].Attempt != 1 {
				t.Fatalf("delivery %d key %q, attempt %d", i, res.Messages[i].Envelope.DedupKey, res.Messages[i].Attempt)
			}
		}
	}
	pollKeys("first")
	var pending, audited int
	err := st.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_messages WHERE dedup_key IN ('second','small') AND status='pending' AND attempts=0 AND lease_id IS NULL),
		(SELECT count(*) FROM agent_message_events e JOIN agent_messages m ON m.id=e.message_id
		 WHERE m.dedup_key='oversized' AND m.status='dead' AND m.attempts=0 AND e.kind='poll_size_limit')`).Scan(&pending, &audited)
	if err != nil || pending != 2 || audited != 1 {
		t.Fatalf("remaining pending %d, oversized audit %d: %v", pending, audited, err)
	}
	pollKeys("second", "small")
}

func TestDeliveryQueue(t *testing.T) {
	st := setupDeliveryTestStore(t)
	q := NewQueue(st)
	ctx := t.Context()
	enqueue := func(t *testing.T, role, key, trace string, maxAttempts int, arrival time.Time) {
		t.Helper()
		env := Envelope{SchemaVersion: 1, Source: "alertmanager", SourceID: "test", TraceID: trace, DedupKey: key}
		p := Policy{Role: role, RouteID: "test", ConfigVersion: "v1", TTL: time.Hour, MaxAttempts: maxAttempts, EnqueuedAt: arrival}
		if err := st.WithTx(ctx, func(tx pgx.Tx) error { return q.Enqueue(ctx, tx, env, p) }); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, who Principal) Delivery {
		t.Helper()
		ds, err := q.Claim(ctx, who, "v1", 1)
		if err != nil || len(ds) != 1 {
			t.Fatalf("claim got %d messages: %v", len(ds), err)
		}
		return ds[0]
	}
	noClaim := func(t *testing.T, who Principal) {
		t.Helper()
		ds, err := q.Claim(ctx, who, "v1", 1)
		if err != nil || len(ds) != 0 {
			t.Fatalf("claim got %d messages: %v", len(ds), err)
		}
	}
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(t *testing.T, want int, sql string, args ...any) {
		t.Helper()
		var n int
		if err := st.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil || n != want {
			t.Fatalf("want %d, got %d: %v", want, n, err)
		}
	}

	t.Run("提交时才发送角色唤醒且回滚不留下任务", func(t *testing.T) {
		listener, err := st.Pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Release()
		if _, err := listener.Exec(ctx, `LISTEN agent_delivery_ready`); err != nil {
			t.Fatal(err)
		}
		defer listener.Exec(ctx, `UNLISTEN agent_delivery_ready`)
		aborted := errors.New("rollback projection")
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			err := q.Enqueue(ctx, tx, Envelope{DedupKey: "test"}, Policy{Role: "aborted", TTL: time.Hour, MaxAttempts: 1})
			if err != nil {
				return err
			}
			count(t, 0, `SELECT count(*) FROM agent_messages WHERE role='aborted'`)
			return aborted
		}); !errors.Is(err, aborted) {
			t.Fatal(err)
		}
		enqueue(t, "visible", "key", "trace", 3, time.Time{})
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := listener.Conn().WaitForNotification(deadline)
		if err != nil {
			t.Fatal(err)
		}
		if n.Channel != "agent_delivery_ready" || n.Payload != "visible" {
			t.Fatalf("unexpected role notification: %#v", n)
		}
		count(t, 0, `SELECT count(*) FROM agent_messages WHERE role='aborted'`)
	})
	t.Run("并发领取互斥且每行使用独立租约", func(t *testing.T) {
		const n = 16
		for i := range n {
			enqueue(t, "ops", fmt.Sprintf("g-%d", i), "trace", 3, time.Time{})
		}
		type result struct {
			messages []Delivery
			err      error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for i := range 2 {
			go func() {
				<-start
				ds, err := NewQueue(st).Claim(ctx, Principal{ClientID: fmt.Sprint(i), Role: "ops"}, "v1", n/2)
				results <- result{ds, err}
			}()
		}
		close(start)
		ids := map[string]bool{}
		leases := map[string]bool{}
		for range 2 {
			res := <-results
			if res.err != nil {
				t.Fatal(res.err)
			}
			for _, d := range res.messages {
				if ids[d.MessageID] || leases[d.LeaseID] || d.Envelope.ID != d.MessageID || d.Attempt != 1 {
					t.Fatalf("duplicate or invalid delivery: %#v", d.Receipt)
				}
				ids[d.MessageID] = true
				leases[d.LeaseID] = true
			}
		}
		if len(ids) != n {
			t.Fatalf("got %d messages, want %d", len(ids), n)
		}
		count(t, 2, `SELECT count(*) FROM agent_consumers WHERE role='ops'`)
		count(t, n, `SELECT count(*) FROM agent_message_events WHERE kind='claim'`)
	})
	t.Run("ACK 重试幂等且校验主体和代次", func(t *testing.T) {
		who := Principal{ClientID: "a", Role: "ack"}
		enqueue(t, who.Role, "key", "trace", 3, time.Time{})
		d := claim(t, who)
		for _, other := range []Principal{{ClientID: "b", Role: "ack"}, {ClientID: "a", Role: "dev"}} {
			if !errors.Is(q.Ack(ctx, other, d.Receipt), ErrLeaseConflict) {
				t.Fatal("accepted a foreign ACK")
			}
		}
		stale := d.Receipt
		stale.Attempt++
		if !errors.Is(q.Ack(ctx, who, stale), ErrLeaseConflict) {
			t.Fatal("accepted wrong attempt")
		}
		for range 2 {
			if err := q.Ack(ctx, who, d.Receipt); err != nil {
				t.Fatal(err)
			}
		}
		if !errors.Is(q.Nack(ctx, who, NegativeReceipt{Receipt: d.Receipt, Retryable: true}), ErrLeaseConflict) {
			t.Fatal("accepted nack after ACK")
		}
		exec(t, `UPDATE agent_messages SET expires_at=now() WHERE id=$1`, d.MessageID)
		if err := q.Reap(ctx); err != nil {
			t.Fatal(err)
		}
		noClaim(t, who)
		count(t, 1, `SELECT count(*) FROM agent_message_events WHERE kind='ack' AND message_id=$1`, d.MessageID)
		count(t, 1, `SELECT count(*) FROM agent_messages WHERE status='acked' AND id=$1`, d.MessageID)
		exec(t, `UPDATE agent_messages SET settled_at=now()-interval '8 days' WHERE id=$1`, d.MessageID)
		if err := q.Reap(ctx); err != nil {
			t.Fatal(err)
		}
		count(t, 0, `SELECT count(*) FROM agent_message_events WHERE message_id=$1`, d.MessageID)
	})
	t.Run("ACK 前失败只在下一次领取增加 attempts", func(t *testing.T) {
		who := Principal{ClientID: "a", Role: "retry"}
		enqueue(t, who.Role, "key", "trace", 2, time.Time{})
		d := claim(t, who)
		exec(t, `UPDATE agent_messages SET lease_expires_at=now() WHERE id=$1`, d.MessageID)
		if !errors.Is(q.Ack(ctx, who, d.Receipt), ErrLeaseConflict) {
			t.Fatal("accepted expired lease before reaping")
		}
		if err := q.Reap(ctx); err != nil {
			t.Fatal(err)
		}
		noClaim(t, who)
		count(t, 1, `SELECT count(*) FROM agent_messages WHERE id=$1 AND attempts=1 AND status='pending'`, d.MessageID)
		exec(t, `UPDATE agent_messages SET visible_at=now() WHERE id=$1`, d.MessageID)
		next := claim(t, who)
		if next.Attempt != 2 || next.LeaseID == d.LeaseID {
			t.Fatal("invalid new lease")
		}
		if !errors.Is(q.Ack(ctx, who, d.Receipt), ErrLeaseConflict) {
			t.Fatal("accepted stale receipt")
		}
		if err := q.Nack(ctx, who, NegativeReceipt{Receipt: next.Receipt, Retryable: true, Reason: "unavailable"}); err != nil {
			t.Fatal(err)
		}
		count(t, 1, `SELECT count(*) FROM agent_messages WHERE id=$1 AND attempts=2 AND status='dead'`, d.MessageID)
	})
	t.Run("nack 使用指定的最短和最长重试延迟", func(t *testing.T) {
		for _, delay := range []int{1, 3600} {
			who := Principal{ClientID: "a", Role: fmt.Sprintf("nack-%d", delay)}
			enqueue(t, who.Role, "key", "trace", 2, time.Time{})
			d := claim(t, who)
			var before time.Time
			if err := st.Pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := q.Nack(ctx, who, NegativeReceipt{Receipt: d.Receipt, Retryable: true, RetryAfter: delay}); err != nil {
				t.Fatal(err)
			}
			// 用两次 DB 时间包围 nack 的处理时刻；慢速 CI 不依赖固定的时间容差。
			count(t, 1, `SELECT count(*) FROM agent_messages WHERE id=$1 AND status='pending'
                AND visible_at BETWEEN $2::timestamptz+make_interval(secs => $3) AND clock_timestamp()+make_interval(secs => $3)`, d.MessageID, before, delay)
			exec(t, `UPDATE agent_messages SET visible_at=now() WHERE id=$1`, d.MessageID)
			next := claim(t, who)
			if next.Attempt != 2 {
				t.Fatalf("got attempt %d", next.Attempt)
			}
			if err := q.Nack(ctx, who, NegativeReceipt{Receipt: next.Receipt, Retryable: false}); err != nil {
				t.Fatal(err)
			}
			count(t, 1, `SELECT count(*) FROM agent_messages WHERE id=$1 AND status='dead'`, d.MessageID)
		}
	})
	t.Run("TTL 与超时并发时优先过期", func(t *testing.T) {
		who := Principal{ClientID: "a", Role: "expiry"}
		enqueue(t, who.Role, "leased", "trace", 1, time.Time{})
		leased := claim(t, who)
		enqueue(t, who.Role, "pending", "trace", 1, time.Time{})
		exec(t, `UPDATE agent_messages SET expires_at=now(), lease_expires_at=CASE WHEN status='leased' THEN now() END WHERE role=$1`, who.Role)
		noClaim(t, who)
		if !errors.Is(q.Nack(ctx, who, NegativeReceipt{Receipt: leased.Receipt}), ErrLeaseConflict) {
			t.Fatal("nack won against TTL")
		}
		result := make(chan error, 2)
		for range 2 {
			go func() { result <- NewQueue(st).Reap(ctx) }()
		}
		for range 2 {
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		}
		count(t, 2, `SELECT count(*) FROM agent_messages WHERE role=$1 AND status='expired'`, who.Role)
		count(t, 2, `SELECT count(*) FROM agent_messages m JOIN agent_message_events e ON e.message_id=m.id WHERE m.role=$1 AND e.kind='ttl'`, who.Role)
	})
	t.Run("入站时刻决定 TTL，重复组不延寿且不覆盖已领取快照", func(t *testing.T) {
		who := Principal{ClientID: "a", Role: "dedup"}
		var now time.Time
		if err := st.Pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		start := now.Add(-time.Minute)
		enqueue(t, who.Role, "key", "old", 3, start)
		enqueue(t, who.Role, "key", "new", 3, now)
		enqueue(t, who.Role, "key", "out-of-order", 3, start)
		enqueue(t, who.Role, "key", "expired-in-outbox", 3, now.Add(-2*time.Hour))
		count(t, 1, `SELECT count(*) FROM agent_messages WHERE role=$1 AND status='expired'`, who.Role)
		d := claim(t, who)
		if d.Envelope.TraceID != "new" {
			t.Fatalf("unexpected snapshot %q", d.Envelope.TraceID)
		}
		enqueue(t, who.Role, "key", "after-claim", 3, now)
		count(t, 1, `SELECT count(*) FROM agent_messages WHERE id=$1 AND dup_count=4 AND envelope->>'trace_id'='new' AND expires_at=$2`, d.MessageID, start.Add(time.Hour))
		exec(t, `UPDATE agent_messages SET expires_at=now() WHERE id=$1`, d.MessageID)
		if !errors.Is(q.Ack(ctx, who, d.Receipt), ErrLeaseConflict) {
			t.Fatal("accepted TTL-expired message")
		}
		enqueue(t, who.Role, "key", "new-group", 3, time.Time{})
		next := claim(t, who)
		if next.MessageID == d.MessageID || next.Envelope.TraceID != "new-group" {
			t.Fatal("expired active row swallowed next episode")
		}
		count(t, 1, `SELECT count(*) FROM agent_message_events WHERE kind='ttl' AND message_id=$1`, d.MessageID)
	})
}
