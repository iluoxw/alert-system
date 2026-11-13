//go:build unix

package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/agentprotocol"
)

func delivery() agentprotocol.Delivery {
	r := agentprotocol.Receipt{MessageID: "97812dfd-e690-45c7-a44d-62f271be9513", LeaseID: "41646ff3-e3e4-4017-bf4a-bc36c2c8ca20", Attempt: 2}
	return agentprotocol.Delivery{Receipt: r, Envelope: agentprotocol.Envelope{ID: r.MessageID, SchemaVersion: 1, EventType: "alert.firing", Severity: "critical", Payload: json.RawMessage(`{"note":"host & path=$HOME"}`)}, LeaseExpiresAt: time.Now().Add(time.Minute), AckMode: "on_receive"}
}

func clientFor(t *testing.T, server *httptest.Server, h Handler) *Client {
	t.Helper()
	cfg := Config{Server: server.URL, TokenEnv: "AGENT_TOKEN", ClientID: "ops-1", Concurrency: 1, Handlers: []Handler{h}, ShutdownGrace: time.Second}
	c, err := NewWithTransport(cfg, "private-test-token", slog.New(slog.NewTextHandler(io.Discard, nil)), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestLostAckResponseRetriesReceiptAndRunsOnce(t *testing.T) {
	d := delivery()
	var acks, polls atomic.Int32
	marker := filepath.Join(t.TempDir(), "count")
	input := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("AGENT_TOKEN", "secret-never-in-child")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("incorrect token")
		}
		switch r.URL.Path {
		case "/api/v1/agent/poll":
			var req struct {
				Max      int
				ClientID string `json:"client_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Max != 1 || req.ClientID != "ops-1" {
				t.Error("wrong poll", req, err)
			}
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"messages": []agentprotocol.Delivery{d}})
				return
			}
			w.WriteHeader(http.StatusUnauthorized) // stop the run after the sole slot is returned
		case "/api/v1/agent/ack":
			var receipt agentprotocol.Receipt
			if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil || receipt != d.Receipt {
				t.Error("ack receipt changed", receipt, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Error("handler started before receiving ACK confirmation")
			}
			acks.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	h := Handler{Exec: []string{"/bin/sh", "-c", `test -z "${AGENT_TOKEN:-}" && printf '%s\n' "$MESSAGE_ID" >> "$1" && cat > "$2"`, "handler", marker, input}, Timeout: time.Second}
	h.Match.EventType = "alert.*"
	c := clientFor(t, srv, h)
	tr := c.http.Transport
	var lost atomic.Bool
	c.http.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		res, err := tr.RoundTrip(req)
		if err == nil && req.URL.Path == "/api/v1/agent/ack" && lost.CompareAndSwap(false, true) {
			res.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		return res, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, statusError(401)) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != d.MessageID+"\n" || acks.Load() != 2 {
		t.Fatalf("handler=%q, err=%v, acks=%d", b, err, acks.Load())
	}
	var got agentprotocol.Envelope
	b, err = os.ReadFile(input)
	if err != nil || json.Unmarshal(b, &got) != nil {
		t.Fatalf("bad stdin %s: %v", b, err)
	}
	var content map[string]string
	if json.Unmarshal(got.Payload, &content) != nil || content["note"] != "host & path=$HOME" {
		t.Fatalf("bad payload %s", got.Payload)
	}
}

func TestRejectBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*agentprotocol.Delivery)
		op     string
		status int
	}{
		{"lease conflict", func(*agentprotocol.Delivery) {}, "ack", 409},
		{"schema", func(d *agentprotocol.Delivery) { d.Envelope.SchemaVersion++ }, "nack", 204},
		{"unmatched", func(d *agentprotocol.Delivery) { d.Envelope.EventType = "deploy" }, "nack", 204},
		{"mode", func(d *agentprotocol.Delivery) { d.AckMode = "on_complete" }, "nack", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := delivery()
			tc.mutate(&d)
			var calls atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/agent/"+tc.op {
					t.Error("unexpected", r.URL.Path)
				}
				if tc.op == "nack" {
					var n agentprotocol.NegativeReceipt
					if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.Retryable || n.Reason == "" || n.Receipt != d.Receipt {
						t.Error("invalid rejection", n, err)
					}
				}
				calls.Add(1)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			marker := filepath.Join(t.TempDir(), "ran")
			h := Handler{Exec: []string{"/usr/bin/touch", marker}, Timeout: time.Second}
			h.Match.EventType = "alert.*"
			c := clientFor(t, srv, h)
			err := c.handle(context.Background(), context.Background(), d)
			if tc.status == 409 && !errors.Is(err, statusError(409)) || tc.status == 204 && err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) || calls.Load() != 1 {
				t.Fatal("unexpected execution or request count", err, calls.Load())
			}
		})
	}
}

func TestHandlerTimeoutKillsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	started := filepath.Join(t.TempDir(), "started")
	h := Handler{Exec: []string{"/bin/sh", "-c", `touch "$2"; (sleep 0.6; echo survived > "$1") & wait`, "handler", marker, started}, Timeout: 200 * time.Millisecond}
	h.Match.EventType = "*"
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	c := clientFor(t, srv, h)
	c.execute(context.Background(), h, delivery())
	if _, err := os.Stat(started); err != nil {
		t.Fatal("handler did not start", err)
	}
	time.Sleep(650 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("grandchild outlived timed-out handler", err)
	}
}

func TestWrapperExitKillsBackgroundGroup(t *testing.T) {
	for _, tc := range []struct{ name, exit, streams string }{
		{"success_detached_streams", "0", "redirect"},
		{"failure_inherited_streams", "17", "inherit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Confirm a child is running before the wrapper exits. It writes the marker
			// only after execute returns (when the worker would release its slot).
			script := `(
                if [ "$3" = redirect ]; then exec >/dev/null 2>&1; fi
                touch "$1/ready"
                while [ ! -e "$1/release" ]; do sleep 0.02; done
                touch "$1/escaped"
            ) </dev/null &
            while [ ! -e "$1/ready" ]; do sleep 0.02; done
            exit "$2"`
			h := Handler{Exec: []string{"/bin/sh", "-c", script, "handler", dir, tc.exit, tc.streams}, Timeout: 5 * time.Second}
			h.Match.EventType = "*"
			srv := httptest.NewTLSServer(http.NotFoundHandler())
			defer srv.Close()
			c := clientFor(t, srv, h)
			c.execute(context.Background(), h, delivery())
			if _, err := os.Stat(filepath.Join(dir, "ready")); err != nil {
				t.Fatal("child never started", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			// An escaped child polls the release file every 20ms.
			time.Sleep(250 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
				t.Fatal("child survived wrapper exit", err)
			}
		})
	}
}

func TestResultCaptureIsPrivateAndBounded(t *testing.T) {
	d := delivery()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	h := Handler{Exec: []string{"/bin/sh", "-c", `head -c 20000 /dev/zero; printf 'private-test-token' >&2`}, Timeout: time.Second}
	h.Match.EventType = "alert.*"
	c := clientFor(t, srv, h)
	c.cfg.ResultDir = dir
	c.execute(context.Background(), h, d)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	file := filepath.Join(dir, entries[0].Name())
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	var out struct {
		Stdout, Stderr string
		Truncated      bool `json:"stdout_truncated"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) != outputLimit || !out.Truncated || out.Stderr != "[REDACTED]" {
		t.Error("unsafe/unbounded output", len(out.Stdout), out.Truncated, out.Stderr)
	}
}

func TestDuplicateReceiptInBatchNeverAcknowledged(t *testing.T) {
	d := delivery()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/poll" {
			t.Error("unexpected settlement of duplicate delivery")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []agentprotocol.Delivery{d, d}})
	}))
	defer srv.Close()
	h := Handler{Exec: []string{"/bin/cat"}, Timeout: time.Second}
	h.Match.EventType = "*"
	c := clientFor(t, srv, h)
	got, err := c.poll(context.Background(), 2)
	if err == nil || len(got) != 0 {
		t.Fatal("repeated receipt accepted", err, got)
	}
}

func TestShutdownGraceBoundsRunningHandler(t *testing.T) {
	d := delivery()
	started := filepath.Join(t.TempDir(), "started")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []agentprotocol.Delivery{d}})
		case "/api/v1/agent/ack":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	h := Handler{Exec: []string{"/bin/sh", "-c", `touch "$1"; sleep 30`, "handler", started}, Timeout: time.Minute}
	h.Match.EventType = "*"
	c := clientFor(t, srv, h)
	c.cfg.ShutdownGrace = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handler not started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler remained active beyond shutdown grace")
	}
}

func TestClientConfigValidatesExecutableAndCredentials(t *testing.T) {
	cfg, err := LoadConfig("../../cmd/agent-client/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg, "", nil); err == nil {
		t.Fatal("accepted missing credential")
	}
	for _, url := range []string{"http://localhost:8000", "https://user:secret@example.com", "https://example.com?key=secret"} {
		copy := cfg
		copy.Server = url
		if err := copy.validate(); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	cfg.Handlers[0].Exec = []string{filepath.Join(t.TempDir(), "missing")}
	if err := cfg.validate(); err == nil {
		t.Error("accepted missing handler")
	}
}
