//go:build integration

package store

import (
	"context"
	"errors"
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

// TestAlertRepo_GetLatestResolvedByFingerprint_ReturnsMostRecent 验证 dedup stage
// refire 跨行延续 flap 基线（aiops-foundation R4/C4）依赖的查询语义：同一 fingerprint
// 存在多条历史 resolved 行时，取 created_at 最新的一条；不存在时返回 ErrAlertNotFound。
func TestAlertRepo_GetLatestResolvedByFingerprint_ReturnsMostRecent(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()
	fingerprint := "flap-history-fp"

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := repo.GetLatestResolvedByFingerprint(ctx, tx, fingerprint)
		if !errors.Is(err, ErrAlertNotFound) {
			return fmt.Errorf("首次出现应返回 ErrAlertNotFound, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var olderID, newerID string
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
		if err != nil {
			return err
		}
		olderID = alert.ID
		_, err = tx.Exec(ctx, `UPDATE alerts SET status = 'resolved', ends_at = now(), created_at = now() - interval '1 hour', flap_count = 2 WHERE id = $1`, olderID)
		return err
	})
	if err != nil {
		t.Fatalf("准备较旧的 resolved 行失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
		if err != nil {
			return err
		}
		newerID = alert.ID
		_, err = tx.Exec(ctx, `UPDATE alerts SET status = 'resolved', ends_at = now(), flap_count = 5 WHERE id = $1`, newerID)
		return err
	})
	if err != nil {
		t.Fatalf("准备较新的 resolved 行失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		latest, err := repo.GetLatestResolvedByFingerprint(ctx, tx, fingerprint)
		if err != nil {
			return err
		}
		if latest.ID != newerID {
			return fmt.Errorf("GetLatestResolvedByFingerprint 返回 ID = %s, want 最新行 %s（旧行 %s）", latest.ID, newerID, olderID)
		}
		if latest.FlapCount != 5 {
			return fmt.Errorf("latest.FlapCount = %d, want 5（应取最新行而非旧行的 2）", latest.FlapCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验最近 resolved 行失败: %v", err)
	}
}

// TestAlertRepo_UpdateFlapState 验证 flap 三字段的落库与读回，覆盖 flapWindowStart
// 为零值时写 NULL 的路径。
func TestAlertRepo_UpdateFlapState(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()
	fingerprint := "flap-update-fp"

	var id string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
		if err != nil {
			return err
		}
		id = alert.ID
		return nil
	})
	if err != nil {
		t.Fatalf("准备告警失败: %v", err)
	}

	windowStart := time.Now().Add(-30 * time.Second)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.UpdateFlapState(ctx, tx, id, 3, windowStart, true)
	})
	if err != nil {
		t.Fatalf("UpdateFlapState 失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, err := repo.GetByFingerprint(ctx, tx, fingerprint)
		if err != nil {
			return err
		}
		if alert.FlapCount != 3 || !alert.Flapping {
			return fmt.Errorf("读回的 flap 状态 = {FlapCount:%d Flapping:%v}, want {3 true}", alert.FlapCount, alert.Flapping)
		}
		if alert.FlapWindowStart.Unix() != windowStart.Unix() {
			return fmt.Errorf("FlapWindowStart = %v, want %v", alert.FlapWindowStart, windowStart)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验落库结果失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		uerr := repo.UpdateFlapState(ctx, tx, "00000000-0000-0000-0000-000000000000", 1, time.Time{}, false)
		if !errors.Is(uerr, ErrAlertNotFound) {
			return fmt.Errorf("不存在的 id 应返回 ErrAlertNotFound, got %v", uerr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

// TestAlertRepo_CountByRouteSince 验证风暴熔断计数通路（design.md C3）：只统计
// 目标 routeID 下、created_at 落在 since 之后的行，且不与其他路由/更早时间的行混计。
func TestAlertRepo_CountByRouteSince(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()

	seed := func(fingerprint, routeID string, createdAt time.Time) {
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE alerts SET route_id = $1, created_at = $2 WHERE id = $3`, routeID, createdAt, alert.ID)
			return err
		})
		if err != nil {
			t.Fatalf("seed 告警 %s 失败: %v", fingerprint, err)
		}
	}

	// route_id 外键指向 routes，routes.target_team_id 又非空外键指向 teams；
	// 准备两条路由（priority 各不相同，避免唯一约束冲突）供隔离性断言。
	var teamID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('count-by-route-team') RETURNING id`).Scan(&teamID)
	})
	if err != nil {
		t.Fatalf("准备测试团队失败: %v", err)
	}
	var routeA, routeB string
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO routes (priority, matchers, target_team_id, group_by) VALUES (1, '[]'::jsonb, $1, '[]'::jsonb) RETURNING id
		`, teamID).Scan(&routeA); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO routes (priority, matchers, target_team_id, group_by) VALUES (2, '[]'::jsonb, $1, '[]'::jsonb) RETURNING id
		`, teamID).Scan(&routeB)
	})
	if err != nil {
		t.Fatalf("准备测试路由失败: %v", err)
	}

	now := time.Now()
	seed("storm-fp-1", routeA, now.Add(-30*time.Second)) // 窗口内
	seed("storm-fp-2", routeA, now.Add(-10*time.Second)) // 窗口内
	seed("storm-fp-3", routeA, now.Add(-90*time.Second)) // 窗口外（早于 since）
	seed("storm-fp-4", routeB, now.Add(-10*time.Second)) // 窗口内但路由不同

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		count, err := repo.CountByRouteSince(ctx, tx, routeA, now.Add(-60*time.Second))
		if err != nil {
			return err
		}
		if count != 2 {
			return fmt.Errorf("routeA 近窗口计数 = %d, want 2", count)
		}
		count, err = repo.CountByRouteSince(ctx, tx, routeB, now.Add(-60*time.Second))
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("routeB 近窗口计数 = %d, want 1", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("CountByRouteSince 校验失败: %v", err)
	}
}

// TestAlertRepo_ListActiveByRoute 验证 ListActiveByRoute（供 storm_summary handler，
// design.md C3）：只返回本路由下非 resolved 的告警，按 last_seen_at 倒序（最新在前，
// 供取"代表样本标题"）；resolved 成员与其他路由的成员都不应出现。
func TestAlertRepo_ListActiveByRoute(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()

	seed := func(fingerprint, routeID, title, status string, lastSeenAt time.Time) string {
		var id string
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
			if err != nil {
				return err
			}
			id = alert.ID
			_, err = tx.Exec(ctx, `
				UPDATE alerts SET route_id = $1, title = $2, status = $3, last_seen_at = $4 WHERE id = $5
			`, routeID, title, status, lastSeenAt, alert.ID)
			return err
		})
		if err != nil {
			t.Fatalf("seed 告警 %s 失败: %v", fingerprint, err)
		}
		return id
	}

	var teamID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('list-active-by-route-team') RETURNING id`).Scan(&teamID)
	})
	if err != nil {
		t.Fatalf("准备测试团队失败: %v", err)
	}
	var routeA, routeB string
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO routes (priority, matchers, target_team_id, group_by) VALUES (1, '[]'::jsonb, $1, '[]'::jsonb) RETURNING id
		`, teamID).Scan(&routeA); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO routes (priority, matchers, target_team_id, group_by) VALUES (2, '[]'::jsonb, $1, '[]'::jsonb) RETURNING id
		`, teamID).Scan(&routeB)
	})
	if err != nil {
		t.Fatalf("准备测试路由失败: %v", err)
	}

	now := time.Now()
	seed("active-fp-older", routeA, "older active", "firing", now.Add(-time.Minute))
	newestID := seed("active-fp-newer", routeA, "newest active", "firing", now)
	seed("active-fp-resolved", routeA, "resolved, 不应出现", "resolved", now.Add(30*time.Second))
	seed("active-fp-other-route", routeB, "other route, 不应出现", "firing", now)

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		alerts, err := repo.ListActiveByRoute(ctx, tx, routeA)
		if err != nil {
			return err
		}
		if len(alerts) != 2 {
			return fmt.Errorf("routeA 活跃告警数 = %d, want 2", len(alerts))
		}
		if alerts[0].ID != newestID {
			return fmt.Errorf("首条应是 last_seen_at 最新的一条，got id=%s title=%q", alerts[0].ID, alerts[0].Title)
		}
		if alerts[0].Title != "newest active" || alerts[1].Title != "older active" {
			return fmt.Errorf("标题/顺序不符预期: %+v", []string{alerts[0].Title, alerts[1].Title})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListActiveByRoute 校验失败: %v", err)
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

// TestAlertRepo_FingerprintStats_CountAndAvg 验证 FingerprintStats：count 覆盖
// since 起该 fingerprint 下全部状态行，avgSeconds 只对 resolved 且 ends_at 非
// NULL 的样本取平均，无这类样本时为 nil（而非 0）。
func TestAlertRepo_FingerprintStats_CountAndAvg(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()
	fingerprint := "fp-stats-fp"

	now := time.Now()
	since := now.Add(-time.Minute)

	seedResolved := func(suffix string, startsAt, endsAt time.Time) {
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE alerts SET status = 'resolved', starts_at = $1, ends_at = $2 WHERE id = $3`, startsAt, endsAt, alert.ID)
			return err
		})
		if err != nil {
			t.Fatalf("准备 resolved 行 %s 失败: %v", suffix, err)
		}
	}

	starts1, ends1 := now, now.Add(30*time.Second)
	starts2, ends2 := now.Add(-10*time.Second), now.Add(50*time.Second)
	seedResolved("-r1", starts1, ends1)
	seedResolved("-r2", starts2, ends2)

	var firingID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
		if err != nil {
			return err
		}
		firingID = alert.ID
		return nil
	})
	if err != nil {
		t.Fatalf("准备 firing 行失败: %v", err)
	}

	wantAvg := ((ends1.Sub(starts1)).Seconds() + (ends2.Sub(starts2)).Seconds()) / 2

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		count, avgSeconds, err := repo.FingerprintStats(ctx, tx, fingerprint, since)
		if err != nil {
			return err
		}
		if count != 3 {
			return fmt.Errorf("count = %d, want 3", count)
		}
		if avgSeconds == nil {
			return fmt.Errorf("avgSeconds = nil, want 非 nil")
		}
		if diff := *avgSeconds - wantAvg; diff > 1 || diff < -1 {
			return fmt.Errorf("avgSeconds = %v, want 约等于 %v（容差 1s）", *avgSeconds, wantAvg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验 count/avg 失败: %v", err)
	}

	// 窗口外样本：把 firing 行的 starts_at 改到 since 之前很久，不应计入 count。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alerts SET starts_at = $1 WHERE id = $2`, now.Add(-time.Hour), firingID)
		return err
	})
	if err != nil {
		t.Fatalf("改动 firing 行 starts_at 失败: %v", err)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		count, _, err := repo.FingerprintStats(ctx, tx, fingerprint, since)
		if err != nil {
			return err
		}
		if count != 2 {
			return fmt.Errorf("窗口外样本被移出后 count = %d, want 2", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验窗口外样本被排除失败: %v", err)
	}

	// 只有 firing 状态、无 resolved 样本时，avgSeconds 应为 nil。
	onlyFiringFP := "fp-stats-only-firing-fp"
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(onlyFiringFP), "")
		return err
	})
	if err != nil {
		t.Fatalf("准备 only-firing 告警失败: %v", err)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		count, avgSeconds, err := repo.FingerprintStats(ctx, tx, onlyFiringFP, since)
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("only-firing count = %d, want 1", count)
		}
		if avgSeconds != nil {
			return fmt.Errorf("only-firing avgSeconds = %v, want nil（无 resolved 样本）", *avgSeconds)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验 only-firing avgSeconds 为 nil 失败: %v", err)
	}
}

// TestAlertRepo_ListRecentResolvedByFingerprint 验证 ListRecentResolvedByFingerprint：
// 只返回 resolved 且 ends_at 非 NULL 的行，按 ends_at DESC 排序，excludeID 传空串
// 时不排除任何行，limit 生效，note/reason 从 timeline_entries（action=resolve）
// 最新一条 detail 中取（无对应条目时两者均为空串）。
func TestAlertRepo_ListRecentResolvedByFingerprint(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewAlertRepo()
	timelineRepo := NewTimelineRepo()
	fingerprint := "fp-recent-resolved-fp"

	now := time.Now()

	seedResolved := func(suffix string, endsAt time.Time) string {
		var id string
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			alert, _, err := repo.UpsertByFingerprint(ctx, tx, testAlertEvent(fingerprint), "")
			if err != nil {
				return err
			}
			id = alert.ID
			_, err = tx.Exec(ctx, `UPDATE alerts SET status = 'resolved', ends_at = $1 WHERE id = $2`, endsAt, id)
			return err
		})
		if err != nil {
			t.Fatalf("准备 resolved 行 %s 失败: %v", suffix, err)
		}
		return id
	}

	idWithNote := seedResolved("-note", now)
	idWithReason := seedResolved("-reason", now.Add(-1*time.Minute))
	idNoTimeline := seedResolved("-no-timeline", now.Add(-2*time.Minute))
	idExcluded := seedResolved("-excluded", now.Add(-3*time.Minute))
	idNullEnds := seedResolved("-null-ends", now.Add(-4*time.Minute))

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := timelineRepo.Append(ctx, tx, domain.TimelineEntry{
			AlertID: idWithNote, Actor: "system", Action: domain.TimelineActionResolve,
			Detail: map[string]any{"note": "手动处置说明"}, At: now,
		}); err != nil {
			return err
		}
		if _, err := timelineRepo.Append(ctx, tx, domain.TimelineEntry{
			AlertID: idWithReason, Actor: "system", Action: domain.TimelineActionResolve,
			Detail: map[string]any{"reason": "auto-resolved"}, At: now,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("写入 resolve 时间线条目失败: %v", err)
	}

	// idNullEnds 手动把 ends_at 改回 NULL，不应出现在任何查询结果中。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alerts SET ends_at = NULL WHERE id = $1`, idNullEnds)
		return err
	})
	if err != nil {
		t.Fatalf("清空 idNullEnds 的 ends_at 失败: %v", err)
	}

	// excludeID 传空串：不报错，不排除任何本该出现的行（4 条：note/reason/no-timeline/excluded）。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		refs, err := repo.ListRecentResolvedByFingerprint(ctx, tx, fingerprint, "", 10)
		if err != nil {
			return err
		}
		if len(refs) != 4 {
			return fmt.Errorf("excludeID 为空串时返回 %d 条, want 4", len(refs))
		}
		byID := map[string]ResolvedRef{}
		for _, ref := range refs {
			byID[ref.AlertID] = ref
		}
		noteRef, ok := byID[idWithNote]
		if !ok {
			return fmt.Errorf("缺少 idWithNote 对应的记录")
		}
		if noteRef.Note != "手动处置说明" || noteRef.Reason != "" {
			return fmt.Errorf("idWithNote 的 Note/Reason = %q/%q, want %q/%q", noteRef.Note, noteRef.Reason, "手动处置说明", "")
		}
		reasonRef, ok := byID[idWithReason]
		if !ok {
			return fmt.Errorf("缺少 idWithReason 对应的记录")
		}
		if reasonRef.Reason != "auto-resolved" || reasonRef.Note != "" {
			return fmt.Errorf("idWithReason 的 Note/Reason = %q/%q, want %q/%q", reasonRef.Note, reasonRef.Reason, "", "auto-resolved")
		}
		noTimelineRef, ok := byID[idNoTimeline]
		if !ok {
			return fmt.Errorf("缺少 idNoTimeline 对应的记录")
		}
		if noTimelineRef.Note != "" || noTimelineRef.Reason != "" {
			return fmt.Errorf("idNoTimeline 的 Note/Reason 应均为空串, got %q/%q", noTimelineRef.Note, noTimelineRef.Reason)
		}
		if _, ok := byID[idNullEnds]; ok {
			return fmt.Errorf("ends_at 为 NULL 的行不应出现在结果中")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("excludeID 为空串场景校验失败: %v", err)
	}

	// excludeID 生效：排除 idExcluded 后应少 1 条，且该 ID 不在结果里。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		refs, err := repo.ListRecentResolvedByFingerprint(ctx, tx, fingerprint, idExcluded, 10)
		if err != nil {
			return err
		}
		if len(refs) != 3 {
			return fmt.Errorf("排除 idExcluded 后返回 %d 条, want 3", len(refs))
		}
		for _, ref := range refs {
			if ref.AlertID == idExcluded {
				return fmt.Errorf("excludeID 应被排除，但仍出现在结果中")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("excludeID 生效场景校验失败: %v", err)
	}

	// limit 截断 + DESC 排序：limit=2 时应只返回 ends_at 最大的前两条（idWithNote, idWithReason）。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		refs, err := repo.ListRecentResolvedByFingerprint(ctx, tx, fingerprint, "", 2)
		if err != nil {
			return err
		}
		if len(refs) != 2 {
			return fmt.Errorf("limit=2 时返回 %d 条, want 2", len(refs))
		}
		if refs[0].AlertID != idWithNote || refs[1].AlertID != idWithReason {
			return fmt.Errorf("limit=2 时顺序 = [%s, %s], want [idWithNote, idWithReason]（按 ends_at DESC）", refs[0].AlertID, refs[1].AlertID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("limit 截断/排序校验失败: %v", err)
	}
}
