// Package agentprotocol defines the versioned JSON exchanged by the server and agent client.
package agentprotocol

import (
	"alert-system/internal/domain"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// MaxPollResponseBytes includes the complete encoded poll response and its trailing newline.
const MaxPollResponseBytes = 16 << 20

type Envelope struct {
	ID            string            `json:"id"`
	SchemaVersion int               `json:"schema_version"`
	TraceID       string            `json:"trace_id"`
	Source        string            `json:"source"`
	SourceID      string            `json:"source_id"`
	EventType     string            `json:"event_type"`
	DedupKey      string            `json:"dedup_key"`
	Severity      string            `json:"severity"`
	Labels        map[string]string `json:"labels"`
	Payload       json.RawMessage   `json:"payload"`
}

type Receipt struct {
	MessageID string `json:"message_id"`
	LeaseID   string `json:"lease_id"`
	Attempt   int    `json:"attempt"`
}
type NegativeReceipt struct {
	Receipt
	Retryable  bool   `json:"retryable"`
	RetryAfter int    `json:"retry_after,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type HandlerOutcome string

const (
	HandlerSucceeded HandlerOutcome = "succeeded"
	HandlerFailed    HandlerOutcome = "failed"
	HandlerTimedOut  HandlerOutcome = "timed_out"
	HandlerCanceled  HandlerOutcome = "canceled"
)

type HandlerResult struct {
	Outcome    HandlerOutcome `json:"outcome"`
	ExitCode   *int           `json:"exit_code,omitempty"`
	DurationMS int64          `json:"duration_ms"`
}

type HandlerReport struct {
	Receipt
	Result HandlerResult `json:"result"`
}

type Delivery struct {
	Receipt
	Envelope        Envelope  `json:"envelope"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
	AckMode         string    `json:"ack_mode"`
	ReportSupported bool      `json:"report_supported,omitempty"`
}

// Member 只包含公开稳定规则引用和 occurrence 哈希，不暴露标签全集或链接。
type Member struct {
	Ref       string          `json:"ref"`
	RuleID    string          `json:"rule_id,omitempty"`
	Alertname string          `json:"alertname,omitempty"`
	Status    domain.Status   `json:"status"`
	Severity  domain.Severity `json:"severity"`
	StartsAt  time.Time       `json:"startsAt"`
	EndsAt    *time.Time      `json:"endsAt,omitempty"`
}
type GroupPayload struct {
	Origin          string   `json:"origin,omitempty"`
	ReceiverRef     string   `json:"receiver_ref"`
	GroupRef        string   `json:"group_ref"`
	TruncatedAlerts int      `json:"truncatedAlerts"`
	Alerts          []Member `json:"alerts"`
}

// OccurrenceRef 与存储时间精度一致，Grafana快照恢复从PG读取starts_at时不能
// 因纳秒被舍去而产生新的member身份。Alertmanager沿用原有纳秒契约。
func OccurrenceRef(source, sourceID, fingerprint string, startsAt time.Time) string {
	if source == domain.SourceKindGrafana {
		startsAt = startsAt.Truncate(time.Microsecond)
	}
	b, _ := json.Marshal([]string{sourceID, fingerprint, startsAt.UTC().Format(time.RFC3339Nano)})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
