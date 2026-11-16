//go:build integration

package agentdelivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Exercise the listener and poll API against a real PostgreSQL instance. A login
// used exclusively by the listener lets the test keep it offline while writers
// and pollers continue to use their normal connection pool.
func TestListenerDisconnectAndMissedNotification(t *testing.T) {
	st := setupDeliveryTestStore(t)
	ctx, stop := context.WithCancel(t.Context())
	if _, err := st.Pool.Exec(ctx, `CREATE ROLE delivery_listener_test LOGIN PASSWORD 'local-test-only'`); err != nil {
		t.Fatal(err)
	}
	dsn, err := url.Parse(st.Pool.Config().ConnConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	dsn.User = url.UserPassword("delivery_listener_test", "local-test-only")
	queue := NewQueue(st)
	api := testAPI(ctx, queue)
	rt := Runtime{dsn: dsn.String(), api: api}
	stopped := make(chan struct{})
	go func() { defer close(stopped); rt.listen(ctx) }()
	defer func() { stop(); <-stopped }()

	awaitPositive := func(query string, args ...any) int {
		t.Helper()
		until, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		for {
			var value int
			if err := st.Pool.QueryRow(until, query, args...).Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value > 0 {
				return value
			}
			select {
			case <-until.Done():
				t.Fatalf("timed out waiting for %s", query)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	const readyListener = `SELECT coalesce(max(pid),0) FROM pg_stat_activity
        WHERE usename='delivery_listener_test' AND state='idle' AND query='LISTEN agent_delivery_ready'`
	beginPoll := func(version string) <-chan *httptest.ResponseRecorder {
		t.Helper()
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			result <- postAPI(api, "poll", "test-token", fmt.Sprintf(`{"client_id":"worker","version":%q,"wait":25,"max":1}`, version))
		}()
		// The heartbeat is written in the same transaction as the first claim.
		// Once visible, that request has already checked the empty role queue.
		awaitPositive(`SELECT count(*) FROM agent_consumers WHERE client_id='worker' AND role='ops' AND version=$1`, version)
		return result
	}
	enqueue := func(trace string) {
		t.Helper()
		if err := st.WithTx(ctx, func(tx pgx.Tx) error {
			return queue.Enqueue(ctx, tx, Envelope{SchemaVersion: 1, TraceID: trace, DedupKey: trace}, Policy{Role: "ops", RouteID: "ops", TTL: time.Minute, MaxAttempts: 3})
		}); err != nil {
			t.Fatal(err)
		}
	}
	expectDelivery := func(result <-chan *httptest.ResponseRecorder, trace string) {
		t.Helper()
		select {
		case w := <-result:
			var res struct {
				Messages []Delivery `json:"messages"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil || len(res.Messages) != 1 || res.Messages[0].Envelope.TraceID != trace {
				t.Fatalf("poll returned %d %s", w.Code, w.Body.String())
			}
		case <-time.After(8 * time.Second):
			t.Fatal("long poll did not claim the committed message within the recheck window")
		}
	}

	firstPID := awaitPositive(readyListener)
	if _, err := st.Pool.Exec(ctx, `ALTER ROLE delivery_listener_test NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := st.Pool.QueryRow(ctx, `SELECT pg_terminate_backend($1,5000)`, firstPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminating listener: terminated=%t err=%v", terminated, err)
	}
	missed := beginPoll("offline")
	enqueue("missed-notify")
	expectDelivery(missed, "missed-notify")

	// Subscribe before allowing the listener to reconnect. This distinguishes
	// its one-time reconnect broadcast from the subsequent role notification.
	reconnected := api.wake.watch("reconnect-probe")
	if _, err := st.Pool.Exec(ctx, `ALTER ROLE delivery_listener_test LOGIN`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reconnected:
	case <-time.After(15 * time.Second):
		t.Fatal("listener did not reconnect")
	}
	if pid := awaitPositive(readyListener); pid == firstPID {
		t.Fatal("expected a new listener session")
	}

	ready := beginPoll("online")
	roleNotification := api.wake.watch("ops")
	enqueue("after-reconnect")
	select {
	case <-roleNotification:
	case <-time.After(8 * time.Second):
		t.Fatal("resumed LISTEN did not wake the notified role")
	}
	expectDelivery(ready, "after-reconnect")
}
