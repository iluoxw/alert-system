package agentdelivery

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"alert-system/internal/ingest"
	"alert-system/internal/store"

	"github.com/jackc/pgx/v5"
)

// Runtime shares the same adapter, queue and HTTP wiring in production and tests.
type Runtime struct {
	projector *Projector
	api       *API
	queue     *Queue
	dsn       string
}

func NewRuntime(cfg *ServerConfig, st *store.Store, dsn string, logger *slog.Logger, stop context.Context) *Runtime {
	q := NewQueue(st)
	return &Runtime{queue: q, dsn: dsn, projector: NewProjector(st, q, cfg.Routing, logger),
		api: &API{queue: q, config: cfg, stop: stop, logger: logger, wake: &wakeups{roles: make(map[string]chan struct{})}},
	}
}

func (rt *Runtime) AttachIngest(s *ingest.PGStore) {
	s.WithBatchRecorder(rt.projector.Record)
}

func (rt *Runtime) Mount(register func(string, string, http.Handler)) {
	for _, op := range []string{"poll", "ack", "nack"} {
		register(http.MethodPost, "/api/v1/agent/"+op, rt.api)
	}
}

func (rt *Runtime) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { rt.projector.Run(ctx) })
	wg.Go(func() { rt.listen(ctx) })
	defer wg.Wait()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if err := rt.queue.Reap(ctx); err != nil && ctx.Err() == nil {
			rt.api.logger.Error("agent reaper failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (rt *Runtime) listen(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, rt.dsn)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN agent_delivery_ready")
			if err == nil {
				rt.api.wake.reconnect()
				delay = time.Second
				for ctx.Err() == nil {
					n, e := conn.WaitForNotification(ctx)
					if e != nil {
						err = e
						break
					}
					rt.api.wake.signal(n.Payload)
				}
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		// Poll rechecks the table independently during listener outages.
		rt.api.logger.Warn("agent listener disconnected", "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(30*time.Second, delay*2)
	}
}
