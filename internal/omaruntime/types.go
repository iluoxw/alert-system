// Package omaruntime is the durable, optional analysis consumer. It never writes domain lifecycle state.
package omaruntime

import (
	"alert-system/internal/agentdelivery"
	"alert-system/internal/agentprotocol"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

var (
	ErrFence          = errors.New("oma runtime: stale lease")
	ErrNotFound       = errors.New("oma runtime: not found")
	ErrReportConflict = errors.New("oma runtime: report content conflict")
	ErrRoundDone      = errors.New("oma runtime: round ended")
)

var ErrModelTerminated = errors.New("oma runtime: model round terminated")

const IntentSchema = "sre.analysis-intent/v1"
const ResultSchema = "sre.analysis-result/v1"

type Member = agentprotocol.Member
type GroupPayload = agentprotocol.GroupPayload

type Intent struct {
	Schema    string   `json:"schema"`
	QueryIDs  []string `json:"query_ids"`
	Rationale string   `json:"rationale"`
}
type Fact struct {
	EvidenceID string `json:"evidence_id"`
	Statement  string `json:"statement"`
}
type Report struct {
	Schema          string   `json:"schema"`
	Status          string   `json:"status"`
	Verdict         string   `json:"verdict"`
	Summary         string   `json:"summary"`
	Facts           []Fact   `json:"facts"`
	Inferences      []string `json:"inferences"`
	EvidenceGaps    []string `json:"evidence_gaps"`
	Recommendations []string `json:"recommendations"`
	ReasonCode      string   `json:"reason_code,omitempty"`
}
type Evidence struct {
	ID         string          `json:"evidence_id"`
	QueryID    string          `json:"query_id"`
	Status     string          `json:"status"`
	ObservedAt time.Time       `json:"observed_at"`
	Payload    json.RawMessage `json:"payload"`
}
type EvidenceBundle struct {
	CoverageGaps []string   `json:"coverage_gaps,omitempty"`
	Schema       string     `json:"schema"`
	Items        []Evidence `json:"items"`
	Partial      bool       `json:"partial"`
}
type EvidenceCollector func(context.Context, []string) (json.RawMessage, error)
type Event struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Text        string    `json:"text,omitempty"`
	StopReason  string    `json:"stop_reason,omitempty"`
	ProcessedAt time.Time `json:"processed_at"`
}
type SessionRequest struct {
	RunID        string          `json:"run_id"`
	SubmissionID string          `json:"submission_id"`
	Input        json.RawMessage `json:"input"`
}
type Message struct {
	SubmissionID string          `json:"submission_id"`
	Input        json.RawMessage `json:"input"`
}
type Model interface {
	Create(context.Context, SessionRequest) (string, error)
	Find(context.Context, string, time.Time) (string, bool, error)
	Send(context.Context, string, Message) error
	Events(context.Context, string) ([]Event, error)
	Subscribe(context.Context, string, string, func(Event) error) error
}
type Queue interface {
	Claim(context.Context, agentdelivery.Principal, string, int) ([]agentdelivery.Delivery, error)
	Ack(context.Context, agentdelivery.Principal, agentdelivery.Receipt) error
	AckTx(context.Context, pgx.Tx, agentdelivery.Principal, agentdelivery.Receipt) error
}
type Run struct {
	StreamConnections int                    `json:"stream_connections"`
	LastStreamAt      *time.Time             `json:"last_stream_at"`
	ID                string                 `json:"id"`
	MessageID         string                 `json:"message_id"`
	Role              string                 `json:"role"`
	ClientID          string                 `json:"client_id"`
	SourceID          string                 `json:"source_id"`
	EventType         string                 `json:"event_type"`
	State             string                 `json:"state"`
	SessionID         string                 `json:"session_id,omitempty"`
	Cursor            string                 `json:"cursor,omitempty"`
	ReasonCode        string                 `json:"reason_code,omitempty"`
	AlertIDs          []string               `json:"alert_ids"`
	FaultIDs          []string               `json:"fault_ids"`
	BindingStatus     string                 `json:"binding_status"`
	Attempts          int                    `json:"attempts"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	Receipt           agentdelivery.Receipt  `json:"-"`
	Envelope          agentprotocol.Envelope `json:"-"`
	Intent            *Intent                `json:"-"`
	Evidence          *EvidenceBundle        `json:"-"`
	LeaseToken        int64                  `json:"-"`
}
type Action struct {
	Stage, SubmissionID, Status string
	Request                     json.RawMessage
	EventAfter                  int64
}
type Result struct {
	Run      Run            `json:"run"`
	Report   *Report        `json:"report"`
	Evidence []Evidence     `json:"evidence"`
	Events   []EventSummary `json:"events"`
}
type EventSummary struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	ProcessedAt time.Time `json:"processed_at"`
}
type Filter struct {
	MessageID, AlertID, FaultID, SourceID, Role string
	Limit                                       int
}
