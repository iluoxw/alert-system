package agentdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/agentprotocol"
)

var (
	ErrReportConflict  = errors.New("delivery already has a different handler report")
	ErrMessageNotFound = errors.New("delivery message not found")
)

// Report records the first outcome for an acknowledged receipt. The message lock
// serializes competing submissions and retention. Reporting never changes delivery state.
func (q *Queue) Report(ctx context.Context, who Principal, report agentprotocol.HandlerReport) error {
	detail, err := json.Marshal(report.Result)
	if err != nil {
		return err
	}
	return q.st.WithTx(ctx, func(tx pgx.Tx) error {
		status, err := lockReceipt(ctx, tx, who, report.Receipt)
		if err != nil {
			return err
		}
		if status != "acked" {
			return ErrLeaseConflict
		}

		var identical bool
		err = tx.QueryRow(ctx, `SELECT detail=$2::jsonb FROM agent_message_events
			WHERE message_id=$1 AND kind='handler_report' ORDER BY id LIMIT 1`, report.MessageID, detail).Scan(&identical)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO agent_message_events(message_id,kind,from_status,to_status,actor,detail)
				VALUES($1,'handler_report','acked','acked',$2,$3::jsonb)`, report.MessageID, who.ClientID, detail)
			return err
		case err != nil:
			return err
		case identical:
			return nil
		default:
			return ErrReportConflict
		}
	})
}

type MessageDetail struct {
	MessageID      string                       `json:"message_id"`
	Role           string                       `json:"role"`
	DeliveryStatus string                       `json:"delivery_status"`
	Attempts       int                          `json:"attempts"`
	CreatedAt      time.Time                    `json:"created_at"`
	SettledAt      *time.Time                   `json:"settled_at,omitempty"`
	ReportedAt     *time.Time                   `json:"reported_at,omitempty"`
	HandlerResult  *agentprotocol.HandlerResult `json:"handler_result,omitempty"`
}

func (q *Queue) MessageDetail(ctx context.Context, tx pgx.Tx, id string) (MessageDetail, error) {
	d := MessageDetail{MessageID: id}
	var detail []byte
	err := tx.QueryRow(ctx, `SELECT m.role,m.status,m.attempts,m.created_at,m.settled_at,r.at,r.detail
		FROM agent_messages m LEFT JOIN LATERAL (
			SELECT at,detail FROM agent_message_events
			WHERE message_id=m.id AND kind='handler_report' ORDER BY id LIMIT 1
		) r ON true WHERE m.id=$1`, id).
		Scan(&d.Role, &d.DeliveryStatus, &d.Attempts, &d.CreatedAt, &d.SettledAt, &d.ReportedAt, &detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageDetail{}, ErrMessageNotFound
	}
	if err != nil {
		return MessageDetail{}, err
	}
	if d.ReportedAt != nil {
		if err := json.Unmarshal(detail, &d.HandlerResult); err != nil {
			return MessageDetail{}, err
		}
	}
	return d, nil
}
