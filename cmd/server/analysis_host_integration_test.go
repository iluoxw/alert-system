//go:build integration

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/store"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestHostRealAnalysisIdleProgressStopKeepsSharedStore(t *testing.T) {
	ctx := t.Context()
	pg, err := testcontainers.Run(ctx, "postgres:16-alpine", testcontainers.WithEnv(map[string]string{"POSTGRES_PASSWORD": "test"}), testcontainers.WithExposedPorts("5432/tcp"), testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2)))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, pg)
	endpoint, err := pg.PortEndpoint(ctx, "5432/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, "postgres://postgres:test@"+endpoint+"/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	files, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("migration files unavailable", err)
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("migration %s: %v", path, err)
		}
	}
	// 此测试没有待分析消息；若意外请求模型，应显式失败而不是访问真实 OMA。
	var modelCalls atomic.Int32
	model := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		http.Error(w, "unexpected model request", 500)
	}))
	defer model.Close()
	cli := filepath.Join(t.TempDir(), "iacctl-test")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	values := testAnalysisValues()
	values["OMA_BASE_URL"] = model.URL
	values["IACCTL_BINARY"] = cli
	values["OMA_POLL_INTERVAL"] = "100ms"
	worker, err := configureAnalysis(func(key string) string { return values[key] }, testDelivery(), st)
	if err != nil {
		t.Fatal(err)
	}
	router := hostTestRouter()
	registerAnalysisRoutes(router, worker)
	if code := hostRequest(router, "GET", "/readyz/analysis"); code != 503 {
		t.Fatal("new worker ready before any actual successful poll", code)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := startAnalysis(workerCtx, worker)
	defer run.stop()
	deadline := time.Now().Add(5 * time.Second)
	for !worker.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if code := hostRequest(router, "GET", "/readyz/analysis"); code != 200 {
		t.Fatal("successful real queue polling did not make analysis ready", code)
	}
	cancel()
	stopped := make(chan struct{})
	go func() { run.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("analysis did not stop after host cancellation")
	}
	if worker.Ready() {
		t.Fatal("stopped analysis still ready")
	}
	if code := hostRequest(router, "GET", "/readyz/analysis"); code != 503 {
		t.Fatal("stopped worker did not remove analysis readiness", code)
	}
	if err := st.Pool.Ping(context.Background()); err != nil {
		t.Fatal("analysis closed or damaged the host's shared store", err)
	}
	if modelCalls.Load() != 0 {
		t.Fatal("empty queue caused an external analysis request")
	}
}
