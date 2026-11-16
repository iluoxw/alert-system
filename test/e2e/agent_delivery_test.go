package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"alert-system/internal/agentclient"
	"alert-system/internal/agentdelivery"
	"alert-system/internal/domain"
	"alert-system/internal/ingest"
)

// Stop at the durable 202 boundary, then start a fresh runtime and an HTTPS
// consumer. The regular pipeline continues processing the same two occurrences.
func TestAgentDelivery_WebhookSurvivesRestartAndRunsHandler(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	admin := newLoggedInClient(t)
	created := adminCreate(t, admin, "/api/v1/admin/integrations", map[string]any{
		"kind": "alertmanager", "name": "agent-" + uniqueSuffix(), "default_severity": "warning",
	}, http.StatusCreated)
	id := created["id"].(string)
	hook := created["webhook_path"].(string)
	token := "agent-e2e-" + uniqueSuffix()
	path := filepath.Join(t.TempDir(), "server.yaml")
	data := fmt.Sprintf("routing:\n  source_ids: [%s]\n  fallback: {role: ops, ttl: 1h}\nclients:\n  - {client_id: reader, role: ops, token_sha256: '%x'}\n", id, sha256.Sum256([]byte(token)))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := agentdelivery.LoadServerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	producer := agentdelivery.NewRuntime(cfg, sys.Store, testDSN, slog.Default(), ctx)
	st := ingest.NewPGStore(sys.Store)
	producer.AttachIngest(st)
	reg := ingest.NewRegistry()
	reg.Register(ingest.NewAlertmanagerAdapter(domain.SeverityWarning))
	mux := http.NewServeMux()
	mux.Handle("/ingest/", ingest.NewServer(reg, st, slog.Default()).Handler())
	ingress := httptest.NewTLSServer(mux)
	defer ingress.Close()
	alerts := []map[string]any{}
	for _, name := range []string{"HighCPU", "NodeDown"} {
		alerts = append(alerts, map[string]any{"status": "firing", "fingerprint": name + uniqueSuffix(), "startsAt": time.Now().UTC(), "labels": map[string]string{"alertname": name, "severity": "critical", "agent_e2e": id}, "annotations": map[string]string{"summary": "sensitive-e2e-annotation"}})
	}
	payload := mustJSON(map[string]any{"groupKey": "test-group", "version": "4", "status": "firing", "receiver": "test", "alerts": alerts})
	resp, err := ingress.Client().Post(ingress.URL+hook, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("ingest %d", resp.StatusCode)
	}

	// There is no in-memory handoff between producer and consumer.
	ingress.Close()
	consumer := agentdelivery.NewRuntime(cfg, sys.Store, testDSN, slog.Default(), ctx)
	workerMux := http.NewServeMux()
	consumer.Mount(func(method, path string, h http.Handler) { workerMux.Handle(method+" "+path, h) })
	srv := httptest.NewTLSServer(workerMux)
	defer srv.Close()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); consumer.Run(ctx) }()
	defer func() { stop(); <-workerDone }()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	handler := agentclient.Handler{Exec: []string{"/bin/cat"}, Timeout: 5 * time.Second}
	handler.Match.EventType = "alert.*"
	client, err := agentclient.NewWithTransport(agentclient.Config{Server: srv.URL, ClientID: "reader", TokenEnv: "AGENT_TOKEN", Concurrency: 1, ShutdownGrace: time.Second, ResultDir: dir, Handlers: []agentclient.Handler{handler}}, token, slog.Default(), srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client failed to drain")
		}
	}()
	var output struct {
		MessageID string `json:"message_id"`
		ExitCode  int    `json:"exit_code"`
		Stdout    string `json:"stdout"`
	}
	waitFor(t, 15*time.Second, 50*time.Millisecond, func() bool {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil || len(files) != 1 {
			return false
		}
		b, err := os.ReadFile(files[0])
		return err == nil && json.Unmarshal(b, &output) == nil
	}, "重启后客户端接收并执行 handler")
	var env agentdelivery.Envelope
	if err := json.Unmarshal([]byte(output.Stdout), &env); err != nil {
		t.Fatal(err)
	}
	if output.ExitCode != 0 || env.ID != output.MessageID || env.SourceID != id || env.Severity != "critical" || bytes.Contains(env.Payload, []byte("sensitive-e2e-annotation")) {
		t.Fatal("handler did not receive expected sanitized batch")
	}
	var group struct {
		Alerts []struct{ Alertname string } `json:"alerts"`
	}
	if err := json.Unmarshal(env.Payload, &group); err != nil || len(group.Alerts) != 2 || group.Alerts[0].Alertname != "HighCPU" || group.Alerts[1].Alertname != "NodeDown" {
		t.Fatalf("expected both webhook members in one handler input, got %+v (err %v)", group, err)
	}
	var status string
	if err := sys.Store.Pool.QueryRow(ctx, "SELECT status FROM agent_messages WHERE id=$1", env.ID).Scan(&status); err != nil || status != "acked" {
		t.Fatalf("delivery status %q, err %v", status, err)
	}
	waitFor(t, 5*time.Second, 100*time.Millisecond, func() bool {
		alert, ok, err := findAlertByLabel(admin, "agent_e2e", id)
		return err == nil && ok && alert.SourceState == "firing" && alert.ResponseState == "unacknowledged"
	}, "任务 ACK 不改变告警处置状态")
}
