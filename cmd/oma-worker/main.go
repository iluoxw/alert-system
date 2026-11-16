// oma-worker consumes the existing PG role queue. It never requires a user laptop or changes domain state.
package main

import (
	"alert-system/internal/agentdelivery"
	"alert-system/internal/omaruntime"
	"alert-system/internal/store"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func run() error {
	show := flag.Bool("version", false, "print build version")
	flag.Parse()
	if *show {
		fmt.Println(version)
		return nil
	}
	if os.Getenv("DB_DSN") == "" {
		return errors.New("required environment variable DB_DSN is missing")
	}
	cfg, e := omaruntime.LoadConfig(os.Getenv)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	st, e := store.New(ctx, os.Getenv("DB_DSN"))
	if e != nil {
		return errors.New("analysis database unavailable")
	}
	defer st.Close()
	worker, e := omaruntime.NewConfiguredWorker(st, cfg)
	if e != nil {
		return e
	}
	queue := agentdelivery.NewQueue(st)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		qctx, stop := context.WithTimeout(r.Context(), time.Second)
		defer stop()
		if st.Pool.Ping(qctx) != nil {
			http.Error(w, "analysis storage unavailable", 503)
			return
		}
		if !worker.Ready() {
			http.Error(w, "analysis worker progress unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	server := &http.Server{Addr: cfg.HealthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	finished := make(chan struct{})
	go func() { defer close(finished); worker.Run(ctx) }()
	// The same bounded reaper is idempotent with the API process's existing reaper.
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if e := queue.Reap(ctx); e != nil && ctx.Err() == nil {
					slog.Warn("analysis queue reaper unavailable")
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
	case e = <-serveErr:
		if !errors.Is(e, http.ErrServerClosed) {
			slog.Error("analysis health listener stopped")
		}
		cancel()
	}
	cancel()
	stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = server.Shutdown(stopCtx)
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		return errors.New("analysis worker shutdown timed out")
	}
	<-reaperDone
	return nil
}
func main() {
	if e := run(); e != nil {
		slog.Error("OMA worker stopped", "reason", e.Error())
		os.Exit(1)
	}
}
