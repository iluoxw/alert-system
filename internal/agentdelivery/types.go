// Package agentdelivery implements the optional role-scoped analysis channel.
package agentdelivery

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrLeaseConflict = errors.New("delivery receipt is no longer valid")

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

type Policy struct {
	Role, RouteID, ConfigVersion string
	TTL                          time.Duration
	Priority, MaxAttempts        int
}

type Rule struct {
	Name        string            `yaml:"name"`
	Source      string            `yaml:"source,omitempty"`
	SourceID    string            `yaml:"source_id,omitempty"`
	EventType   string            `yaml:"event_type,omitempty"`
	Severity    string            `yaml:"severity,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Role        string            `yaml:"role"`
	TTL         time.Duration     `yaml:"ttl"`
	Priority    int               `yaml:"priority,omitempty"`
	MaxAttempts int               `yaml:"max_attempts,omitempty"`
	AckMode     string            `yaml:"ack_mode,omitempty"`
}

type Routing struct {
	SourceIDs     []string `yaml:"source_ids"`
	Routes        []Rule   `yaml:"routes"`
	Fallback      Rule     `yaml:"fallback"`
	ConfigVersion string   `yaml:"-"`
}

type Principal struct{ ClientID, Role string }
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
