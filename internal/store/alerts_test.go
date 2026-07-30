//go:build integration

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// 本文件是集成测试，复用 events_test.go 中定义的 setupTestStore/applyMigrations/
// migrationsDir（不重复声明），需要真实 PostgreSQL：
//   go test -tags=integration ./internal/store/... -run 'Alert|Timeline' -v

func testAlertEvent(fingerprint string) domain.AlertEvent {
	return domain.AlertEvent{
		Fingerprint:   fingerprint,
		RuleID:        "rule-x",
		SignalKind:    domain.SignalKindMetric,
		Severity:      domain.SeverityWarning,
		Status:        domain.StatusFiring,
		Title:         "test alert",
		Labels:        map[string]string{"env": "prod"},
		Source:        "test",
		ResourceRef:   map[string]string{"host": "node-1"},
		ResolvePolicy: domain.ResolvePolicyManual,
		StartsAt:      time.Now(),
	}
}

// TestAlertRepo_UpsertByFingerprint_ConcurrentDedup 验证 R3.3：10 个并发写入同一
// fingerprint 的事件，最终只落 1 行，dedup_count 累加到 10，且 created 在全部
// 10 次调用中只出现 1 次 true（其余 9 次都是合并到已有行）。
func TestAlertRepo_UpsertByFingerprint_ConcurrentDedup(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()

	const n = 10
	fingerprint := "concurrent-fp"

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		createdTrue int
		firstID     string
		idMismatch  bool
	)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			err := st.WithTx(ctx, func(tx pgx.Tx) error {
				alert, created, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if created {
					createdTrue++
				}
				if firstID == "" {
					firstID = alert.ID
				} else if alert.ID != firstID {
					idMismatch = true
				}
				return nil
			})
			if err != nil {
				t.Errorf("upsert 事务失败: %v", err)
			}
		}()
	}
	wg.Wait()

	if createdTrue != 1 {
		t.Fatalf("created=true 出现次数 = %d, want 1", createdTrue)
	}
	if idMismatch {
		t.Fatalf("并发 upsert 应始终落在同一行，但观察到不同的 alert ID")
	}

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var count, dedupCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE fingerprint = $1`, fingerprint).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("alerts 表中 fingerprint=%s 的行数 = %d, want 1", fingerprint, count)
		}
		if err := tx.QueryRow(ctx, `SELECT dedup_count FROM alerts WHERE fingerprint = $1`, fingerprint).Scan(&dedupCount); err != nil {
			return err
		}
		if dedupCount != n {
			return fmt.Errorf("dedup_count = %d, want %d", dedupCount, n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验最终状态失败: %v", err)
	}
}

// TestAlertRepo_UpsertByFingerprint_ResolvedThenNew 验证 R3.4：已 resolved 的
// 告警不会被新事件复活——同 fingerprint 的新事件必须新建一行，且旧的 resolved
// 行必须原封不动地保留。
func TestAlertRepo_UpsertByFingerprint_ResolvedThenNew(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()
	fingerprint := "resolved-then-new-fp"

	var firstID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, created, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("首次 upsert 应为 created=true")
		}
		firstID = alert.ID
		_, err = tx.Exec(ctx, `UPDATE alerts SET status = 'resolved', ends_at = now() WHERE id = $1`, firstID)
		return err
	})
	if err != nil {
		t.Fatalf("准备已 resolved 告警失败: %v", err)
	}

	var secondID string
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, created, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("resolved 后同 fingerprint 的新事件应 created=true（新建行），got false")
		}
		secondID = alert.ID
		return nil
	})
	if err != nil {
		t.Fatalf("resolved 后新事件 upsert 失败: %v", err)
	}

	if secondID == firstID {
		t.Fatalf("新事件不应复用已 resolved 的旧行 ID")
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE fingerprint = $1`, fingerprint).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			return fmt.Errorf("fingerprint=%s 的行数 = %d, want 2（一行 resolved 旧行 + 一行新行）", fingerprint, count)
		}
		var oldStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM alerts WHERE id = $1`, firstID).Scan(&oldStatus); err != nil {
			return err
		}
		if oldStatus != "resolved" {
			return fmt.Errorf("旧行状态被意外改动，status = %s, want resolved", oldStatus)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验新旧行共存失败: %v", err)
	}
}

// TestAlertRepo_List_FilterAndPagination 覆盖 severity+status+label 组合过滤、
// team 过滤，以及分页边界（limit/offset 与 total 计数不受分页影响）。
func TestAlertRepo_List_FilterAndPagination(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()

	var teamID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('team-a') RETURNING id`).Scan(&teamID)
	})
	if err != nil {
		t.Fatalf("创建测试团队失败: %v", err)
	}

	seed := func(fingerprint string, severity domain.Severity, labels map[string]string, withTeam bool) {
		event := testAlertEvent(fingerprint)
		event.Severity = severity
		event.Labels = labels
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			alert, _, err := repo.UpsertByFingerprint(ctx, tx, event, "")
			if err != nil {
				return err
			}
			if withTeam {
				_, err = tx.Exec(ctx, `UPDATE alerts SET team_id = $1 WHERE id = $2`, teamID, alert.ID)
			}
			return err
		})
		if err != nil {
			t.Fatalf("seed 告警 %s 失败: %v", fingerprint, err)
		}
	}

	seed("list-fp-1", domain.SeverityCritical, map[string]string{"env": "prod", "service": "api"}, true)
	seed("list-fp-2", domain.SeverityWarning, map[string]string{"env": "prod", "service": "worker"}, false)
	seed("list-fp-3", domain.SeverityCritical, map[string]string{"env": "staging", "service": "api"}, false)

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		page, err := repo.List(ctx, tx, AlertFilter{
			Severity: domain.SeverityCritical,
			Status:   domain.StatusFiring,
			Labels:   map[string]string{"env": "prod"},
		})
		if err != nil {
			return err
		}
		if page.Total != 1 || len(page.Alerts) != 1 || page.Alerts[0].Fingerprint != "list-fp-1" {
			return fmt.Errorf("severity+status+label 过滤结果 = %+v, want 仅 list-fp-1", page)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("severity+status+label 过滤失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		page, err := repo.List(ctx, tx, AlertFilter{TeamID: teamID})
		if err != nil {
			return err
		}
		if page.Total != 1 || len(page.Alerts) != 1 || page.Alerts[0].Fingerprint != "list-fp-1" {
			return fmt.Errorf("team 过滤结果 = %+v, want 仅 list-fp-1", page)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("team 过滤失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		page, err := repo.List(ctx, tx, AlertFilter{Limit: 2, Offset: 0})
		if err != nil {
			return err
		}
		if page.Total != 3 {
			return fmt.Errorf("不加过滤条件时 total = %d, want 3", page.Total)
		}
		if len(page.Alerts) != 2 {
			return fmt.Errorf("limit=2 时返回 %d 条, want 2", len(page.Alerts))
		}

		page2, err := repo.List(ctx, tx, AlertFilter{Limit: 2, Offset: 2})
		if err != nil {
			return err
		}
		if page2.Total != 3 {
			return fmt.Errorf("第二页 total = %d, want 3", page2.Total)
		}
		if len(page2.Alerts) != 1 {
			return fmt.Errorf("offset=2,limit=2 时应剩 1 条, got %d", len(page2.Alerts))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("分页边界校验失败: %v", err)
	}
}

// TestTimelineRepo_AppendAndList 验证 Append/ListByAlert：无论写入顺序如何，
// ListByAlert 必须按时间正序（created_at 升序）返回。
func TestTimelineRepo_AppendAndList(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alertRepo := NewAlertRepo()
	timelineRepo := NewTimelineRepo()

	var alertID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, _, err := alertRepo.UpsertByFingerprint(ctx, tx, testAlertEvent("timeline-fp"), "")
		if err != nil {
			return err
		}
		alertID = alert.ID
		return nil
	})
	if err != nil {
		t.Fatalf("准备告警失败: %v", err)
	}

	base := time.Now().Add(-time.Hour)
	entries := []domain.TimelineEntry{
		{AlertID: alertID, Actor: "system", Action: domain.TimelineActionAck, Detail: map[string]any{"seq": float64(2)}, At: base.Add(2 * time.Minute)},
		{AlertID: alertID, Actor: "system", Action: domain.TimelineActionResolve, Detail: map[string]any{"seq": float64(3)}, At: base.Add(3 * time.Minute)},
		{AlertID: alertID, Actor: "system", Action: domain.TimelineActionHandoff, Detail: map[string]any{"seq": float64(1)}, At: base.Add(1 * time.Minute)},
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		for _, e := range entries {
			if _, err := timelineRepo.Append(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append 时间线失败: %v", err)
	}

	var got []domain.TimelineEntry
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = timelineRepo.ListByAlert(ctx, tx, alertID)
		return err
	})
	if err != nil {
		t.Fatalf("查询时间线失败: %v", err)
	}

	if len(got) != len(entries) {
		t.Fatalf("时间线条目数 = %d, want %d", len(got), len(entries))
	}
	wantSeqOrder := []float64{1, 2, 3}
	for i, e := range got {
		seq, _ := e.Detail["seq"].(float64)
		if seq != wantSeqOrder[i] {
			t.Fatalf("第 %d 条时间线 seq = %v, want %v（应按时间正序排列）", i, seq, wantSeqOrder[i])
		}
	}
}
