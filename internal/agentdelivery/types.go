// Package agentdelivery implements the optional role-scoped analysis channel.
package agentdelivery

import (
	"errors"
	"time"

	"alert-system/internal/agentprotocol"
)

var ErrLeaseConflict = errors.New("delivery receipt is no longer valid")

type Envelope = agentprotocol.Envelope
type Receipt = agentprotocol.Receipt
type NegativeReceipt = agentprotocol.NegativeReceipt
type Delivery = agentprotocol.Delivery

type Policy struct {
	EnqueuedAt                   time.Time
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
