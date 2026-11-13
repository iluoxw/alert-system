package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"alert-system/internal/agentprotocol"
)

const version = "agent-client/1"
const waitSeconds = 25

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Client struct {
	cfg   Config
	token string
	http  *http.Client
	log   *slog.Logger
}

func New(cfg Config, token string, logger *slog.Logger) (*Client, error) {
	return NewWithTransport(cfg, token, logger, nil)
}

// NewWithTransport allows a deployment to supply its own TLS roots. Timeouts and redirect policy still apply.
func NewWithTransport(cfg Config, token string, logger *slog.Logger, transport http.RoundTripper) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, " \n\r\t") {
		return nil, errors.New("token environment variable requires a non-empty bearer token")
	}
	if !supportsGroups {
		return nil, errors.New("agent-client requires a Unix host with process groups")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{cfg: cfg, token: token, log: logger, http: &http.Client{Transport: transport, Timeout: (waitSeconds + 10) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("agent API HTTP %d", e) }

func retryable(err error) bool {
	var status statusError
	if errors.As(err, &status) {
		return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
	}
	return true // transport error or interrupted/invalid response
}

func (c *Client) post(ctx context.Context, op string, request, response any) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.Server, "/")+"/api/v1/agent/"+op, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK || response == nil {
		return statusError(resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, agentprotocol.MaxPollResponseBytes+1))
	if err != nil {
		return err
	}
	if len(b) > agentprotocol.MaxPollResponseBytes {
		return errors.New("poll response exceeds size limit")
	}
	if err := json.Unmarshal(b, response); err != nil {
		return errors.New("invalid poll response JSON")
	}
	return nil
}

func (c *Client) poll(ctx context.Context, max int) ([]agentprotocol.Delivery, error) {
	var res struct {
		Messages []agentprotocol.Delivery `json:"messages"`
	}
	err := c.post(ctx, "poll", map[string]any{"client_id": c.cfg.ClientID, "version": version, "wait": waitSeconds, "max": max}, &res)
	if err != nil {
		return nil, err
	}
	if len(res.Messages) > max {
		return nil, errors.New("server exceeded requested poll batch")
	}
	ids := make(map[string]bool, len(res.Messages))
	for _, d := range res.Messages {
		if !uuid.MatchString(d.MessageID) || !uuid.MatchString(d.LeaseID) || d.Attempt < 1 || ids[d.MessageID] {
			return nil, errors.New("invalid or repeated delivery receipt")
		}
		ids[d.MessageID] = true
	}
	return res.Messages, nil
}

func pause(ctx context.Context, delay time.Duration) bool {
	// Equal jitter avoids synchronized clients returning after an outage.
	t := time.NewTimer(delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// settle retries only the exact same receipt. Server lease and TTL checks remain authoritative.
func (c *Client) settle(ctx context.Context, op string, body any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for delay := 250 * time.Millisecond; ; delay = min(delay*2, 3*time.Second) {
		attempt, done := context.WithTimeout(ctx, 4*time.Second)
		err := c.post(attempt, op, body, nil)
		done()
		if err == nil || !retryable(err) {
			return err
		}
		if !pause(ctx, delay) {
			return ctx.Err()
		}
	}
}

func (c *Client) handle(pollCtx, handlerCtx context.Context, d agentprotocol.Delivery) error {
	h := c.cfg.match(d.Envelope.EventType, d.Envelope.Severity)
	reason := ""
	switch {
	case d.Envelope.SchemaVersion != 1 || d.AckMode != "on_receive":
		reason = "unsupported_contract"
	case d.Envelope.ID != d.MessageID || d.Envelope.EventType == "" || len(d.Envelope.Payload) == 0:
		reason = "invalid_envelope"
	case h == nil:
		reason = "no_matching_handler"
	}
	if reason != "" {
		return c.settle(pollCtx, "nack", agentprotocol.NegativeReceipt{Receipt: d.Receipt, Reason: reason})
	}
	if err := c.settle(pollCtx, "ack", d.Receipt); err != nil {
		return err
	}
	c.execute(handlerCtx, *h, d)
	return nil
}

// Reserve slots before polling. Only the poll loop reserves and each worker returns its own slot.
func reserve(ctx context.Context, slots chan struct{}) int {
	select {
	case <-ctx.Done():
		return 0
	case <-slots:
	}
	for n := 1; n < maxBatch; n++ {
		select {
		case <-slots:
		default:
			return n
		}
	}
	return maxBatch
}

func (c *Client) Run(ctx context.Context) error {
	pollCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	handlerCtx, killHandlers := context.WithCancel(context.Background())
	defer killHandlers()
	slots := make(chan struct{}, c.cfg.Concurrency)
	for i := 0; i < cap(slots); i++ {
		slots <- struct{}{}
	}
	var workers sync.WaitGroup
	delay := time.Second
	for pollCtx.Err() == nil {
		available := reserve(pollCtx, slots)
		if available == 0 {
			break
		}
		batch, err := c.poll(pollCtx, available)
		for i := len(batch); i < available; i++ {
			slots <- struct{}{}
		}
		if err != nil {
			if !retryable(err) {
				stop(err)
				break
			}
			if pollCtx.Err() != nil {
				break
			}
			c.log.Warn("poll failed", "error", err)
			if !pause(pollCtx, delay) {
				break
			}
			delay = min(delay*2, time.Minute)
			continue
		}
		delay = time.Second
		for _, delivery := range batch {
			workers.Add(1)
			go func(d agentprotocol.Delivery) {
				defer workers.Done()
				defer func() { slots <- struct{}{} }()
				err := c.handle(pollCtx, handlerCtx, d)
				if err == nil || pollCtx.Err() != nil {
					return
				}
				// A stale lease is expected when the poll/ACK response was lost.
				if !retryable(err) && !errors.Is(err, statusError(http.StatusConflict)) {
					stop(err)
				}
				c.log.Warn("delivery not executed", "message_id", d.MessageID, "error", err)
			}(delivery)
		}
	}
	// Cancel pending poll/settlement immediately, letting unconfirmed leases expire server-side.
	// Already acknowledged handlers finish within the grace period before cancellation kills their groups.
	drained := make(chan struct{})
	go func() { workers.Wait(); close(drained) }()
	timer := time.NewTimer(c.cfg.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		killHandlers()
		<-drained
	}
	cause := context.Cause(pollCtx)
	if errors.Is(cause, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return cause
}
