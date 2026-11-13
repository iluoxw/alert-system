// Package agentprotocol defines the versioned JSON exchanged by the server and agent client.
package agentprotocol

import (
	"encoding/json"
	"time"
)

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
type Delivery struct {
	Receipt
	Envelope       Envelope  `json:"envelope"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	AckMode        string    `json:"ack_mode"`
}
