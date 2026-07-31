//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// 本文件是集成测试，复用 events_test.go 中定义的 setupTestStore/applyMigrations
// 与 config_repos_test.go 中定义的 testAlertEvent（不重复声明），需要真实
// PostgreSQL：
//   go test -tags=integration ./internal/store/... -run 'Ops' -v

// createTestTeam 建一个测试团队，返回 team_id。
func createTestTeam(t *testing.T, ctx context.Context, st *Store, name string) string {
	t.Helper()
	var teamID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO teams (name) VALUES ($1) RETURNING id`, name).Scan(&teamID)
	})
	if err != nil {
		t.Fatalf("创建测试团队 %s 失败: %v", name, err)
	}
	return teamID
}

// seedOpsAlert 建一条告警并按需覆盖 severity/starts_at/team_id/status/acked_at/
// ends_at——统计 SQL 关心的全部维度都需要脱离 domain 状态机直接摆到目标状态，
// 与 alerts_test.go/config_repos_test.go 里"先 Upsert 再用裸 SQL 改字段"的既有
// 手法一致，避免在测试里重新驱动一遍状态机。
func seedOpsAlert(t *testing.T, ctx context.Context, st *Store, repo *AlertRepo, fingerprint, teamID string, severity domain.Severity, startsAt time.Time, ackedAt, endsAt *time.Time, status domain.Status) string {
	t.Helper()
	event := testAlertEvent(fingerprint)
	event.Severity = severity
	event.StartsAt = startsAt

	var alertID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alert, _, err := repo.UpsertByFingerprint(ctx, tx, event, "")
		if err != nil {
			return err
		}
		alertID = alert.ID
		_, err = tx.Exec(ctx, `
			UPDATE alerts SET team_id = $2::uuid, status = $3, starts_at = $4, acked_at = $5, ends_at = $6
			WHERE id = $1
		`, alertID, teamID, status, startsAt, ackedAt, endsAt)
		return err
	})
	if err != nil {
		t.Fatalf("seed 告警 %s 失败: %v", fingerprint, err)
	}
	return alertID
}

func timePtr(t time.Time) *time.Time { return &t }

// TestOpsRepo_SettingsCRUD_And_ChannelsRoundTrip 验证 GetSettings 未命中返回
// ErrNotFound、UpsertSettings 幂等（同 team_id 多次写只留一行）、channels
// JSONB 与 []OpsChannelTarget 的往返序列化（kind/target 小写 key）。
func TestOpsRepo_SettingsCRUD_And_ChannelsRoundTrip(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewOpsRepo()
	teamID := createTestTeam(t, ctx, st, "team-ops-crud")

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := repo.GetSettings(ctx, tx, teamID)
		return err
	})
	if err != ErrNotFound {
		t.Fatalf("未配置团队的 GetSettings err = %v, want ErrNotFound", err)
	}

	first := OpsSettings{
		TeamID:                    teamID,
		Timezone:                  "Asia/Shanghai",
		DailyReportEnabled:        true,
		DailyReportTime:           "09:00",
		StaleSweepEnabled:         false,
		StaleSweepIntervalSeconds: 1800,
		OncallBroadcastEnabled:    false,
		OncallBroadcastTime:       "09:30",
		Channels:                  []OpsChannelTarget{{Kind: "lark", Target: "ou_first"}},
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error { return repo.UpsertSettings(ctx, tx, first) })
	if err != nil {
		t.Fatalf("首次 UpsertSettings 失败: %v", err)
	}

	second := first
	second.DailyReportEnabled = false
	second.StaleSweepEnabled = true
	second.StaleSweepIntervalSeconds = 60
	second.Channels = []OpsChannelTarget{{Kind: "webhook", Target: "https://example.com/hook"}}
	err = st.WithTx(ctx, func(tx pgx.Tx) error { return repo.UpsertSettings(ctx, tx, second) })
	if err != nil {
		t.Fatalf("二次 UpsertSettings 失败: %v", err)
	}

	var got *OpsSettings
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = repo.GetSettings(ctx, tx, teamID)
		return err
	})
	if err != nil {
		t.Fatalf("GetSettings 失败: %v", err)
	}
	if got.DailyReportEnabled || !got.StaleSweepEnabled || got.StaleSweepIntervalSeconds != 60 {
		t.Fatalf("GetSettings 结果应反映最后一次 Upsert，got %+v", got)
	}
	if len(got.Channels) != 1 || got.Channels[0].Kind != "webhook" || got.Channels[0].Target != "https://example.com/hook" {
		t.Fatalf("channels 往返结果 = %+v, want [{webhook https://example.com/hook}]", got.Channels)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_ops_settings WHERE team_id = $1`, teamID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("team_ops_settings 行数 = %d, want 1（Upsert 应幂等）", count)
		}
		var channelsRaw string
		if err := tx.QueryRow(ctx, `SELECT channels::text FROM team_ops_settings WHERE team_id = $1`, teamID).Scan(&channelsRaw); err != nil {
			return err
		}
		want := `[{"kind": "webhook", "target": "https://example.com/hook"}]`
		if channelsRaw != want {
			return fmt.Errorf("channels 原始 JSONB = %s, want %s（key 必须小写）", channelsRaw, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("校验落库结果失败: %v", err)
	}
}

// TestOpsRepo_ListEnabledSettings 验证只返回三个开关中任一为 true 的团队配置。
func TestOpsRepo_ListEnabledSettings(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewOpsRepo()

	enabledTeam := createTestTeam(t, ctx, st, "team-ops-enabled")
	disabledTeam := createTestTeam(t, ctx, st, "team-ops-disabled")

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := repo.UpsertSettings(ctx, tx, OpsSettings{
			TeamID: enabledTeam, Timezone: "UTC", DailyReportTime: "09:00",
			OncallBroadcastTime: "09:30", StaleSweepIntervalSeconds: 1800,
			OncallBroadcastEnabled: true,
		}); err != nil {
			return err
		}
		return repo.UpsertSettings(ctx, tx, OpsSettings{
			TeamID: disabledTeam, Timezone: "UTC", DailyReportTime: "09:00",
			OncallBroadcastTime: "09:30", StaleSweepIntervalSeconds: 1800,
		})
	})
	if err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		list, err := repo.ListEnabledSettings(ctx, tx)
		if err != nil {
			return err
		}
		for _, s := range list {
			if s.TeamID == disabledTeam {
				return fmt.Errorf("全部开关关闭的团队不应出现在 ListEnabledSettings 结果中")
			}
		}
		found := false
		for _, s := range list {
			if s.TeamID == enabledTeam {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("开启了 oncall_broadcast 的团队应出现在 ListEnabledSettings 结果中")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListEnabledSettings 校验失败: %v", err)
	}
}

// TestOpsRepo_DailyStats 验证新增/恢复计数、severity 分布、MTTA 均值与中位数、
// 环比新增数，以及时间窗边界（dayStart 含、dayEnd 不含）。
func TestOpsRepo_DailyStats(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alertRepo := NewAlertRepo()
	opsRepo := NewOpsRepo()
	teamID := createTestTeam(t, ctx, st, "team-daily-stats")

	dayStart := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	// 窗内新增：3 条 acked（10min/30min/100min）+ 1 条未 acked。
	seedOpsAlert(t, ctx, st, alertRepo, "ds-in-1", teamID, domain.SeverityCritical,
		dayStart.Add(1*time.Hour), timePtr(dayStart.Add(1*time.Hour+10*time.Minute)), nil, domain.StatusAcknowledged)
	seedOpsAlert(t, ctx, st, alertRepo, "ds-in-2", teamID, domain.SeverityCritical,
		dayStart.Add(2*time.Hour), timePtr(dayStart.Add(2*time.Hour+30*time.Minute)), nil, domain.StatusAcknowledged)
	seedOpsAlert(t, ctx, st, alertRepo, "ds-in-3", teamID, domain.SeverityWarning,
		dayStart.Add(3*time.Hour), timePtr(dayStart.Add(3*time.Hour+100*time.Minute)), nil, domain.StatusAcknowledged)
	seedOpsAlert(t, ctx, st, alertRepo, "ds-in-unacked", teamID, domain.SeverityInfo,
		dayStart.Add(4*time.Hour), nil, nil, domain.StatusFiring)

	// 边界：dayStart 之前（不应计入）、恰为 dayEnd（不应计入，终点不含）。
	seedOpsAlert(t, ctx, st, alertRepo, "ds-before", teamID, domain.SeverityCritical,
		dayStart.Add(-1*time.Hour), nil, nil, domain.StatusFiring)
	seedOpsAlert(t, ctx, st, alertRepo, "ds-at-end", teamID, domain.SeverityCritical,
		dayEnd, nil, nil, domain.StatusFiring)

	// 恢复数：用 ends_at 落窗判定，即使 starts_at 在窗口之外。
	seedOpsAlert(t, ctx, st, alertRepo, "ds-resolved-in", teamID, domain.SeverityWarning,
		dayStart.Add(-2*time.Hour), nil, timePtr(dayStart.Add(5*time.Hour)), domain.StatusResolved)
	seedOpsAlert(t, ctx, st, alertRepo, "ds-resolved-out", teamID, domain.SeverityWarning,
		dayStart.Add(-3*time.Hour), nil, timePtr(dayStart.Add(-1*time.Hour)), domain.StatusResolved)

	// 环比：窗口前 7 天内新增 3 条。
	for i, offset := range []time.Duration{-1 * 24 * time.Hour, -3 * 24 * time.Hour, -6 * 24 * time.Hour} {
		seedOpsAlert(t, ctx, st, alertRepo, fmt.Sprintf("ds-prev-%d", i), teamID, domain.SeverityInfo,
			dayStart.Add(offset), nil, nil, domain.StatusFiring)
	}

	var stats OpsDailyStats
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		stats, err = opsRepo.DailyStats(ctx, tx, teamID, dayStart, dayEnd)
		return err
	})
	if err != nil {
		t.Fatalf("DailyStats 失败: %v", err)
	}

	if stats.NewCount != 4 {
		t.Fatalf("NewCount = %d, want 4（边界外的 2 条不应计入）", stats.NewCount)
	}
	if stats.BySeverity["critical"] != 2 || stats.BySeverity["warning"] != 1 || stats.BySeverity["info"] != 1 {
		t.Fatalf("BySeverity = %+v, want critical=2 warning=1 info=1", stats.BySeverity)
	}
	if stats.ResolvedCount != 1 {
		t.Fatalf("ResolvedCount = %d, want 1（应按 ends_at 落窗，不是 starts_at）", stats.ResolvedCount)
	}
	if !stats.MTTAAvgValid || !stats.MTTAMedianValid {
		t.Fatalf("存在 acked 数据时 MTTAAvgValid/MTTAMedianValid 应为 true，got avg=%v median=%v", stats.MTTAAvgValid, stats.MTTAMedianValid)
	}
	wantAvg := (10 + 30 + 100) * time.Minute / 3
	if diff := stats.MTTAAvg - wantAvg; diff < -time.Second || diff > time.Second {
		t.Fatalf("MTTAAvg = %v, want ~%v", stats.MTTAAvg, wantAvg)
	}
	wantMedian := 30 * time.Minute
	if diff := stats.MTTAMedian - wantMedian; diff < -time.Second || diff > time.Second {
		t.Fatalf("MTTAMedian = %v, want ~%v（三个样本排序后取中位 30min，而非均值 46.7min）", stats.MTTAMedian, wantMedian)
	}
	// 窗口前 7 天 [dayStart-7d, dayStart) 内除 3 条 ds-prev-* 外，ds-before/
	// ds-resolved-in/ds-resolved-out 的 starts_at 也落在这个区间（它们是为测试
	// "新增窗口边界"/"恢复数用 ends_at 而非 starts_at 判定"而造的数据，starts_at
	// 本身恰好在 dayStart 前几小时），共计 6 条。
	wantPrevAvg := 6.0 / 7
	if diff := stats.Prev7DayAvgNew - wantPrevAvg; diff < -0.001 || diff > 0.001 {
		t.Fatalf("Prev7DayAvgNew = %v, want %v（窗口前 7 天新增 6 条 / 7）", stats.Prev7DayAvgNew, wantPrevAvg)
	}
}

// TestOpsRepo_DailyStats_NoAck_MTTAInvalid 验证窗内无 Ack 数据时 MTTAAvgValid/
// MTTAMedianValid 均为 false（不能用 0 冒充"均值为 0"）。
func TestOpsRepo_DailyStats_NoAck_MTTAInvalid(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alertRepo := NewAlertRepo()
	opsRepo := NewOpsRepo()
	teamID := createTestTeam(t, ctx, st, "team-no-ack")

	dayStart := time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	seedOpsAlert(t, ctx, st, alertRepo, "no-ack-1", teamID, domain.SeverityWarning,
		dayStart.Add(time.Hour), nil, nil, domain.StatusFiring)

	var stats OpsDailyStats
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		stats, err = opsRepo.DailyStats(ctx, tx, teamID, dayStart, dayEnd)
		return err
	})
	if err != nil {
		t.Fatalf("DailyStats 失败: %v", err)
	}
	if stats.NewCount != 1 {
		t.Fatalf("NewCount = %d, want 1", stats.NewCount)
	}
	if stats.MTTAAvgValid || stats.MTTAMedianValid {
		t.Fatalf("无 Ack 数据时 MTTAAvgValid/MTTAMedianValid 应为 false，got avg=%v median=%v", stats.MTTAAvgValid, stats.MTTAMedianValid)
	}
	if stats.Prev7DayAvgNew != 0 {
		t.Fatalf("窗口前 7 天无历史数据时 Prev7DayAvgNew = %v, want 0", stats.Prev7DayAvgNew)
	}
}

// TestOpsRepo_ListUnackedCritical 验证只返回 firing 状态的 critical 告警，
// acknowledged/其他 severity/其他团队均被排除。
func TestOpsRepo_ListUnackedCritical(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alertRepo := NewAlertRepo()
	opsRepo := NewOpsRepo()
	teamID := createTestTeam(t, ctx, st, "team-unacked-critical")
	otherTeamID := createTestTeam(t, ctx, st, "team-unacked-critical-other")

	now := time.Now()
	wantID := seedOpsAlert(t, ctx, st, alertRepo, "uc-firing-critical", teamID, domain.SeverityCritical,
		now.Add(-time.Hour), nil, nil, domain.StatusFiring)
	seedOpsAlert(t, ctx, st, alertRepo, "uc-acked-critical", teamID, domain.SeverityCritical,
		now.Add(-time.Hour), timePtr(now), nil, domain.StatusAcknowledged)
	seedOpsAlert(t, ctx, st, alertRepo, "uc-firing-warning", teamID, domain.SeverityWarning,
		now.Add(-time.Hour), nil, nil, domain.StatusFiring)
	seedOpsAlert(t, ctx, st, alertRepo, "uc-other-team-critical", otherTeamID, domain.SeverityCritical,
		now.Add(-time.Hour), nil, nil, domain.StatusFiring)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alerts, err := opsRepo.ListUnackedCritical(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if len(alerts) != 1 || alerts[0].ID != wantID {
			return fmt.Errorf("ListUnackedCritical 结果 = %+v, want 仅 %s", alerts, wantID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListUnackedCritical 校验失败: %v", err)
	}
}

// TestOpsRepo_ListOpenLongerThan 验证只返回 starts_at 早于 olderThan 且未
// resolved 的告警。
func TestOpsRepo_ListOpenLongerThan(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	alertRepo := NewAlertRepo()
	opsRepo := NewOpsRepo()
	teamID := createTestTeam(t, ctx, st, "team-open-longer-than")

	now := time.Now()
	olderThan := now.Add(-24 * time.Hour)

	wantID := seedOpsAlert(t, ctx, st, alertRepo, "ol-old-firing", teamID, domain.SeverityCritical,
		now.Add(-48*time.Hour), nil, nil, domain.StatusFiring)
	seedOpsAlert(t, ctx, st, alertRepo, "ol-recent-firing", teamID, domain.SeverityCritical,
		now.Add(-time.Hour), nil, nil, domain.StatusFiring)
	seedOpsAlert(t, ctx, st, alertRepo, "ol-old-resolved", teamID, domain.SeverityCritical,
		now.Add(-48*time.Hour), nil, timePtr(now.Add(-30*time.Hour)), domain.StatusResolved)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		alerts, err := opsRepo.ListOpenLongerThan(ctx, tx, teamID, olderThan)
		if err != nil {
			return err
		}
		if len(alerts) != 1 || alerts[0].ID != wantID {
			return fmt.Errorf("ListOpenLongerThan 结果 = %+v, want 仅 %s", alerts, wantID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListOpenLongerThan 校验失败: %v", err)
	}
}
