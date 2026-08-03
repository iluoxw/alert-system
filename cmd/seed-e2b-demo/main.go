// Command seed-e2b-demo 把脱敏的 E2B stage B fixture 导入隔离的本地数据库：
// topology -> Alert -> explainable Fault -> blast radius。它不会创建 Incident、不会
// 调用真实 E2B API，也不会删除既有数据；人工宣布与后续协作留给 UI E2E 完成。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/correlation"
	"alert-system/internal/domain"
	"alert-system/internal/e2b"
	"alert-system/internal/store"
)

const defaultDSN = "postgres://alert_system:alert_system@localhost:15432/alert_system?sslmode=disable"

func main() {
	dsnDefault := os.Getenv("DB_DSN")
	if dsnDefault == "" {
		dsnDefault = defaultDSN
	}
	dsn := flag.String("dsn", dsnDefault, "隔离的本地 E2E Postgres DSN")
	flag.Parse()

	if err := run(context.Background(), *dsn); err != nil {
		fmt.Fprintf(os.Stderr, "seed-e2b-demo 失败: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string) error {
	st, err := store.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer st.Close()

	topology, err := e2b.LoadTopologyFixture()
	if err != nil {
		return err
	}
	fixture, err := e2b.LoadReplayFixture("stage_b.json")
	if err != nil {
		return err
	}
	events, err := fixture.AlertEvents()
	if err != nil {
		return err
	}

	topologyRepo := store.NewTopologyRepo()
	alertRepo := store.NewAlertRepo()
	faultRepo := store.NewFaultRepo()
	evaluator := correlation.NewEvaluator(topologyRepo, faultRepo)
	var capability domain.Fault
	var nodeCount, alertCount, faultCount int

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		imported, importErr := topologyRepo.ImportSnapshot(ctx, tx, topology)
		if importErr != nil {
			return importErr
		}
		nodeCount = len(imported.NodeIDs)

		alerts := make([]domain.Alert, 0, len(events))
		for _, event := range events {
			if event.Status != domain.StatusFiring {
				return fmt.Errorf("stage B 包含非 firing 事件 %s", event.Title)
			}
			stored, _, upsertErr := alertRepo.UpsertByFingerprint(ctx, tx, event, "")
			if upsertErr != nil {
				return upsertErr
			}
			alerts = append(alerts, stored)
		}
		alertCount = len(alerts)

		assessment := correlation.AssessE2BFailedToPlace(alerts, fixture.ObservedAt)
		faults, persistErr := evaluator.Persist(ctx, tx, topology.Source, assessment)
		if persistErr != nil {
			return persistErr
		}
		faultCount = len(faults)
		for _, fault := range faults {
			if fault.CorrelationKey == correlation.E2BCapabilityCorrelationKey {
				capability = fault
				break
			}
		}
		if capability.ID == "" || capability.Status != domain.FaultStatusActive {
			return fmt.Errorf("未生成 active capability Fault")
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Printf("完成: nodes=%d alerts=%d faults=%d capability_fault_id=%s status=%s external_mutation=false\n",
		nodeCount, alertCount, faultCount, capability.ID, capability.Status)
	return nil
}
