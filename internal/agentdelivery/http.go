package agentdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type deliveryQueue interface {
	Claim(context.Context, Principal, string, int) ([]Delivery, error)
	Ack(context.Context, Principal, Receipt) error
	Nack(context.Context, Principal, NegativeReceipt) error
}

// The channel for a role is replaced on each signal. A poll takes a snapshot
// before checking the database so a concurrent commit cannot slip past it.
type wakeups struct {
	mu    sync.Mutex
	roles map[string]chan struct{}
}

func (w *wakeups) watch(role string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.roles[role] == nil {
		w.roles[role] = make(chan struct{})
	}
	return w.roles[role]
}

func (w *wakeups) signal(role string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch := w.roles[role]; ch != nil {
		close(ch)
		delete(w.roles, role)
	}
}

func (w *wakeups) reconnect() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for role, ch := range w.roles {
		close(ch)
		delete(w.roles, role)
	}
}

type API struct {
	queue  deliveryQueue
	config *ServerConfig
	wake   *wakeups
	stop   context.Context
	logger *slog.Logger
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > 1024 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	who, ok := a.config.principals[sha256.Sum256([]byte(token))]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var err error
	switch r.URL.Path {
	case "/api/v1/agent/poll":
		a.poll(w, r, who)
		return
	case "/api/v1/agent/ack":
		var req Receipt
		if !readRequest(r, &req) || !validReceipt(req) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		err = a.queue.Ack(r.Context(), who, req)
	case "/api/v1/agent/nack":
		var req NegativeReceipt
		if !readRequest(r, &req) || !validReceipt(req.Receipt) || req.RetryAfter < 0 || req.RetryAfter > 3600 || len(req.Reason) > 256 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		err = a.queue.Nack(r.Context(), who, req)
	default:
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, ErrLeaseConflict) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err != nil {
		a.failure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readRequest(r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst) == nil && dec.Decode(new(any)) == io.EOF
}

func validReceipt(r Receipt) bool {
	return validID(r.MessageID) && validID(r.LeaseID) && r.Attempt > 0
}

func (a *API) failure(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		a.logger.Error("agent queue request failed", "path", r.URL.Path, "error", err)
		http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
	}
}

type pollRequest struct {
	ClientID string `json:"client_id"`
	Version  string `json:"version"`
	Wait     int    `json:"wait"`
	Max      int    `json:"max"`
}

func (a *API) poll(w http.ResponseWriter, r *http.Request, who Principal) {
	req := pollRequest{Wait: 25, Max: 1}
	if !readRequest(r, &req) || req.ClientID == "" || len(req.Version) > 128 || req.Max < 1 || req.Max > 8 || req.Wait < 0 || req.Wait > 30 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.ClientID != who.ClientID {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopCancel := context.AfterFunc(a.stop, cancel)
	defer stopCancel()
	deadline := time.Now().Add(time.Duration(req.Wait) * time.Second)
	for {
		if a.stop.Err() != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		wakeup := a.wake.watch(who.Role)
		// Pool acquisition and the claim transaction also need a finite timeout.
		claimCtx, endClaim := context.WithTimeout(ctx, 3*time.Second)
		items, err := a.queue.Claim(claimCtx, who, req.Version, req.Max)
		endClaim()
		if err != nil {
			if a.stop.Err() != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			a.failure(w, r, err)
			return
		}
		if len(items) > 0 {
			w.Header().Set("Content-Type", "application/json")
			// A broken response leaves the committed leases for the reaper.
			_ = json.NewEncoder(w).Encode(struct {
				Messages []Delivery `json:"messages"`
			}{items})
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Handles lost NOTIFY and retry_after becoming visible without a write.
		timer := time.NewTimer(min(remaining, 3*time.Second))
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-a.stop.Done():
			timer.Stop()
			w.WriteHeader(http.StatusNoContent)
			return
		case <-wakeup:
			timer.Stop()
		case <-timer.C:
		}
	}
}
