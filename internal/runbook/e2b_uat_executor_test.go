package runbook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/domain"
)

func TestE2BUATExecutorLifecycleAndCleanup(t *testing.T) {
	var mu sync.Mutex
	requests := make([]string, 0, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "uat-key" {
			t.Errorf("missing UAT API key header")
		}
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /sandboxes":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sbx-sensitive-id"})
		case "GET /sandboxes/sbx-sensitive-id":
			w.WriteHeader(http.StatusOK)
		case "POST /sandboxes/sbx-sensitive-id/pause":
			w.WriteHeader(http.StatusNoContent)
		case "POST /sandboxes/sbx-sensitive-id/resume":
			w.WriteHeader(http.StatusCreated)
		case "DELETE /sandboxes/sbx-sensitive-id":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newTestE2BUATExecutor(t, server.URL)
	result, err := executor.Execute(context.Background(), ExecutionRequest{
		ActionKey: ActionLifecycleVerify,
		Mode:      domain.RunbookModeApply,
		Parameters: map[string]any{
			"environment_id": "isolated-uat", "authorization_ref": "CHG-1",
			"template_id": "tpl-safe", "template_alias": "golden", "confirm_cleanup": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /sandboxes", "GET /sandboxes/sbx-sensitive-id",
		"POST /sandboxes/sbx-sensitive-id/pause", "POST /sandboxes/sbx-sensitive-id/resume",
		"GET /sandboxes/sbx-sensitive-id", "DELETE /sandboxes/sbx-sensitive-id",
	}
	mu.Lock()
	got := append([]string(nil), requests...)
	mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected lifecycle calls:\n got: %v\nwant: %v", got, want)
	}
	if result["cleanup_succeeded"] != true || result["external_mutation"] != true || result["status"] != "pass" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, leaked := result["sandbox_id"]; leaked {
		t.Fatalf("raw sandbox ID must not enter audit result: %+v", result)
	}
	if result["sandbox_ref_hash"] == "" || result["sandbox_ref_hash"] == "sbx-sensitive-id" {
		t.Fatalf("expected irreversible sandbox reference hash: %+v", result)
	}
}

func TestE2BUATExecutorAlwaysCleansUpAfterPostCreateFailure(t *testing.T) {
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /sandboxes":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sbx-clean-me"})
		case "GET /sandboxes/sbx-clean-me":
			w.WriteHeader(http.StatusOK)
		case "POST /sandboxes/sbx-clean-me/pause":
			w.WriteHeader(http.StatusInternalServerError)
		case "DELETE /sandboxes/sbx-clean-me":
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := newTestE2BUATExecutor(t, server.URL).Execute(context.Background(), ExecutionRequest{
		ActionKey: ActionLifecycleVerify, Mode: domain.RunbookModeApply,
		Parameters: map[string]any{
			"environment_id": "isolated-uat", "authorization_ref": "CHG-1",
			"template_id": "tpl-safe", "confirm_cleanup": true,
		},
	})
	if err == nil {
		t.Fatal("pause failure must fail the run")
	}
	if !deleted.Load() || result["cleanup_succeeded"] != true {
		t.Fatalf("created sandbox must be cleaned after failure: result=%+v deleted=%t", result, deleted.Load())
	}
}

func TestE2BUATExecutorCleansUpAfterCallerCancellation(t *testing.T) {
	readyCalled := make(chan struct{})
	var readyOnce sync.Once
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /sandboxes":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sbx-canceled"})
		case "GET /sandboxes/sbx-canceled":
			readyOnce.Do(func() { close(readyCalled) })
			<-r.Context().Done()
		case "DELETE /sandboxes/sbx-canceled":
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	executor := newTestE2BUATExecutor(t, server.URL)
	type outcome struct {
		result map[string]any
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := executor.Execute(ctx, ExecutionRequest{
			ActionKey: ActionLifecycleVerify, Mode: domain.RunbookModeApply,
			Parameters: map[string]any{
				"environment_id": "isolated-uat", "authorization_ref": "CHG-1",
				"template_id": "tpl-safe", "confirm_cleanup": true,
			},
		})
		finished <- outcome{result: result, err: err}
	}()
	<-readyCalled
	cancel()
	got := <-finished
	if !errors.Is(got.err, context.Canceled) || !deleted.Load() || got.result["cleanup_succeeded"] != true {
		t.Fatalf("caller cancellation must retain category and trigger cleanup: err=%v result=%+v deleted=%t", got.err, got.result, deleted.Load())
	}
}

func TestE2BUATExecutorDoesNotFollowRedirects(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer redirected.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/sandboxes", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := newTestE2BUATExecutor(t, source.URL).Execute(context.Background(), ExecutionRequest{
		ActionKey: ActionLifecycleVerify, Mode: domain.RunbookModeApply,
		Parameters: map[string]any{
			"environment_id": "isolated-uat", "authorization_ref": "CHG-1",
			"template_id": "tpl-safe", "confirm_cleanup": true,
		},
	})
	if err == nil || redirectedCalls.Load() != 0 {
		t.Fatalf("redirect must be rejected without forwarding credentials: err=%v calls=%d", err, redirectedCalls.Load())
	}
}

func TestE2BUATExecutorReportsCleanupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /sandboxes":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sbx-cleanup-fails"})
		case "GET /sandboxes/sbx-cleanup-fails":
			w.WriteHeader(http.StatusOK)
		case "POST /sandboxes/sbx-cleanup-fails/pause":
			w.WriteHeader(http.StatusNoContent)
		case "POST /sandboxes/sbx-cleanup-fails/resume":
			w.WriteHeader(http.StatusOK)
		case "DELETE /sandboxes/sbx-cleanup-fails":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := newTestE2BUATExecutor(t, server.URL).Execute(context.Background(), ExecutionRequest{
		ActionKey: ActionLifecycleVerify, Mode: domain.RunbookModeApply,
		Parameters: map[string]any{
			"environment_id": "isolated-uat", "authorization_ref": "CHG-1",
			"template_id": "tpl-safe", "confirm_cleanup": true,
		},
	})
	if !errors.Is(err, ErrUATCleanupFailed) || result["cleanup_succeeded"] != false {
		t.Fatalf("cleanup failure must fail and remain visible: err=%v result=%+v", err, result)
	}
}

func TestE2BUATExecutorTransportErrorsNeverLeakSandboxID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /sandboxes":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sbx-must-not-leak"})
		case "GET /sandboxes/sbx-must-not-leak":
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = connection.Close()
		case "DELETE /sandboxes/sbx-must-not-leak":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := newTestE2BUATExecutor(t, server.URL).Execute(context.Background(), ExecutionRequest{
		ActionKey: ActionLifecycleVerify, Mode: domain.RunbookModeApply,
		Parameters: map[string]any{
			"environment_id": "isolated-uat", "authorization_ref": "CHG-1",
			"template_id": "tpl-safe", "confirm_cleanup": true,
		},
	})
	if !errors.Is(err, ErrE2BUATTransport) {
		t.Fatalf("expected categorized transport error, got %v", err)
	}
	if strings.Contains(err.Error(), "sbx-must-not-leak") || result["cleanup_succeeded"] != true {
		t.Fatalf("error must not leak raw ID and cleanup must still run: err=%v result=%+v", err, result)
	}
}

func TestE2BUATExecutorRejectsProductionAndInsecureEndpoints(t *testing.T) {
	for _, test := range []struct {
		name      string
		baseURL   string
		allowHost string
		allowHTTP bool
	}{
		{name: "known production", baseURL: "https://api.agi.exc.show", allowHost: "api.agi.exc.show"},
		{name: "known unsafe uat alias", baseURL: "https://api.uat.agi.exc.show", allowHost: "api.uat.agi.exc.show"},
		{name: "http disabled", baseURL: "http://api.uat.example.com", allowHost: "api.uat.example.com"},
		{name: "host mismatch", baseURL: "https://api.uat.example.com", allowHost: "other.uat.example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewE2BUATExecutor(E2BUATExecutorConfig{
				BaseURL: test.baseURL, APIKey: "key", AllowedHost: test.allowHost,
				AllowedIPs: []string{"127.0.0.1"}, AllowHTTP: test.allowHTTP,
			})
			if err == nil {
				t.Fatal("unsafe endpoint must be rejected")
			}
		})
	}
}

func newTestE2BUATExecutor(t *testing.T, baseURL string) *E2BUATExecutor {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewE2BUATExecutor(E2BUATExecutorConfig{
		BaseURL: baseURL, APIKey: "uat-key", AllowedHost: parsed.Hostname(),
		AllowedIPs: []string{"127.0.0.1"}, AllowHTTP: true,
		HTTPClient: &http.Client{Timeout: time.Second}, ReadyTimeout: 100 * time.Millisecond,
		PollInterval: time.Millisecond, PauseSettle: time.Millisecond, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}
