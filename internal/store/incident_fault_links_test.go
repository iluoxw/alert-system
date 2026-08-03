//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

func TestIncidentFaultLinksPreserveSinglePrimaryAndDispositionAudit(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewIncidentRepo()
	faults := NewFaultRepo()
	actor := mustUser(t, ctx, st, "incident-link-reviewer")
	now := time.Now().UTC().Truncate(time.Microsecond)

	var incident, other domain.Incident
	var primary, supporting domain.Fault
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		primary, _, err = faults.Upsert(ctx, tx, testIncidentFault("incident-link-primary", domain.FaultStatusActive, now))
		if err != nil {
			return err
		}
		supporting, _, err = faults.Upsert(ctx, tx, testIncidentFault("incident-link-supporting", domain.FaultStatusActive, now))
		if err != nil {
			return err
		}
		incident, err = repo.Create(ctx, tx, testIncident(actor.ID, "多 Fault 响应", now))
		if err != nil {
			return err
		}
		other, err = repo.Create(ctx, tx, testIncident(actor.ID, "另一个响应", now.Add(time.Second)))
		if err != nil {
			return err
		}
		if _, err := repo.LinkFault(ctx, tx, incident.ID, primary.ID, domain.IncidentFaultPrimary, now); err != nil {
			return err
		}
		if _, err := repo.LinkFault(ctx, tx, incident.ID, supporting.ID, domain.IncidentFaultSupporting, now); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		excluded, err := repo.UpdateFaultDisposition(ctx, tx, incident.ID, supporting.ID,
			domain.IncidentFaultExcluded, "独立因果链", actor.ID, now.Add(time.Minute))
		if err != nil {
			return err
		}
		if excluded.Disposition != domain.IncidentFaultExcluded || excluded.DispositionRationale != "独立因果链" ||
			excluded.DecidedBy != actor.ID || excluded.DecidedAt.IsZero() {
			return fmt.Errorf("Fault 裁决审计不完整: %+v", excluded)
		}
		included, err := repo.UpdateFaultDisposition(ctx, tx, incident.ID, supporting.ID,
			domain.IncidentFaultIncluded, "根因证据已确认", actor.ID, now.Add(2*time.Minute))
		if err != nil {
			return err
		}
		if included.Disposition != domain.IncidentFaultIncluded {
			return fmt.Errorf("Fault 未重新纳入: %+v", included)
		}
		updated, previous, err := repo.SetPrimaryFault(ctx, tx, incident.ID, supporting.ID)
		if err != nil {
			return err
		}
		if previous != primary.ID || updated.Relation != domain.IncidentFaultPrimary {
			return fmt.Errorf("primary 切换错误: previous=%s updated=%+v", previous, updated)
		}
		links, err := repo.ListFaultLinks(ctx, tx, incident.ID)
		if err != nil {
			return err
		}
		primaryCount := 0
		for _, link := range links {
			if link.Relation == domain.IncidentFaultPrimary {
				primaryCount++
			}
		}
		if primaryCount != 1 {
			return fmt.Errorf("primary 数量=%d want=1: %+v", primaryCount, links)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, linkErr := repo.LinkFault(ctx, tx, other.ID, supporting.ID, domain.IncidentFaultSupporting, now)
		return linkErr
	})
	if !errors.Is(err, ErrIncidentForFaultExists) {
		t.Fatalf("跨 Incident 复用 Fault 应冲突, got %v", err)
	}
}

func testIncidentFault(key string, status domain.FaultStatus, now time.Time) domain.Fault {
	return domain.Fault{
		CorrelationKey: key,
		Scope:          domain.FaultScopeCapability,
		Status:         status,
		Severity:       domain.SeverityWarning,
		Kind:           "integration_test_fault",
		Title:          key,
		EntityRef:      key,
		Confidence:     0.9,
		FirstDetected:  now,
		LastEvaluated:  now,
	}
}

func testIncident(actorID, title string, now time.Time) domain.Incident {
	return domain.Incident{
		Status:                 domain.IncidentStatusDeclared,
		Severity:               domain.SeverityWarning,
		Title:                  title,
		DeclaredBy:             actorID,
		CommunicationChannel:   "lark_webhook",
		CommunicationTargetRef: "https://open.feishu.cn",
		DeclaredAt:             now,
	}
}
