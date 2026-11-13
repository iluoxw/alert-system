package agentdelivery

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubQueue struct {
	claim func(context.Context, Principal, string, int) ([]Delivery, error)
	ack   func(context.Context, Principal, Receipt) error
}

func (q stubQueue) Claim(c context.Context, p Principal, v string, n int) ([]Delivery, error) {
	return q.claim(c, p, v, n)
}
func (q stubQueue) Ack(c context.Context, p Principal, r Receipt) error { return q.ack(c, p, r) }
func (q stubQueue) Nack(context.Context, Principal, NegativeReceipt) error {
	return errors.New("unexpected nack")
}

func testAPI(ctx context.Context, q deliveryQueue) *API {
	return &API{queue: q, config: &ServerConfig{principals: map[[32]byte]Principal{sha256.Sum256([]byte("test-token")): {ClientID: "worker", Role: "ops"}}}, stop: ctx, wake: &wakeups{roles: make(map[string]chan struct{})}, logger: slog.Default()}
}

func postAPI(a *API, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestPollAuthorityAndReceipts(t *testing.T) {
	q := stubQueue{claim: func(_ context.Context, p Principal, _ string, max int) ([]Delivery, error) {
		if p.Role != "ops" || p.ClientID != "worker" || max != 1 {
			t.Fatal("poll not scoped to authenticated client")
		}
		return nil, nil
	}, ack: func(context.Context, Principal, Receipt) error { return ErrLeaseConflict }}
	a := testAPI(context.Background(), q)
	for _, tcase := range []struct {
		path, token, body string
		want              int
	}{
		{"poll", "invalid", `{"client_id":"worker","wait":0}`, 401},
		{"poll", "test-token", `{"client_id":"other","wait":0}`, 403},
		{"poll", "test-token", `{"client_id":"worker","role":"dev","wait":0}`, 400},
		{"poll", "test-token", `{"client_id":"worker","max":9,"wait":0}`, 400},
		{"poll", "test-token", `{"client_id":"worker","wait":0}`, 204},
		{"ack", "test-token", `{"message_id":"x","lease_id":"y","attempt":1}`, 400},
		{"ack", "test-token", `{"message_id":"00000000-0000-4000-8000-000000000003","lease_id":"00000000-0000-4000-8000-000000000002","attempt":1}`, 409},
	} {
		if w := postAPI(a, tcase.path, tcase.token, tcase.body); w.Code != tcase.want {
			t.Errorf("%s got %d, want %d", tcase.body, w.Code, tcase.want)
		}
	}
}

func TestNotifyBetweenSubscriptionAndEmptyClaim(t *testing.T) {
	inClaim, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	q := stubQueue{claim: func(context.Context, Principal, string, int) ([]Delivery, error) {
		if count.Add(1) == 1 {
			close(inClaim)
			<-release
			return nil, nil
		}
		return []Delivery{{AckMode: "on_receive"}}, nil
	}}
	a := testAPI(context.Background(), q)
	result := make(chan int, 1)
	go func() { result <- postAPI(a, "poll", "test-token", `{"client_id":"worker","wait":5}`).Code }()
	<-inClaim
	a.wake.signal("ops")
	close(release)
	select {
	case code := <-result:
		if code != 200 {
			t.Fatalf("poll %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("lost wakeup before wait")
	}
}

func TestShutdownEndsIdlePoll(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	claimed := make(chan struct{}, 1)
	q := stubQueue{claim: func(context.Context, Principal, string, int) ([]Delivery, error) {
		claimed <- struct{}{}
		return nil, nil
	}}
	a := testAPI(ctx, q)
	result := make(chan int, 1)
	go func() { result <- postAPI(a, "poll", "test-token", `{"client_id":"worker","wait":25}`).Code }()
	<-claimed
	stop()
	select {
	case code := <-result:
		if code != 204 {
			t.Fatalf("poll %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("poll did not drain")
	}
}

func TestShutdownCancelsBlockedClaim(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	started := make(chan struct{})
	a := testAPI(ctx, stubQueue{claim: func(ctx context.Context, _ Principal, _ string, _ int) ([]Delivery, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	result := make(chan int, 1)
	go func() { result <- postAPI(a, "poll", "test-token", `{"client_id":"worker","wait":25}`).Code }()
	<-started
	stop()
	select {
	case code := <-result:
		if code != http.StatusNoContent {
			t.Fatalf("poll returned %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("stopping the server did not release the claim")
	}
}
