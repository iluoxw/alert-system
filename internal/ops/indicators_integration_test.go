//go:build integration

package ops

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"alert-system/internal/store"
)

const migrationsDir = "../../migrations"

func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "postgres:15-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_PASSWORD": "test",
			"POSTGRES_DB":       "alertsystem",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}
	pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		t.Fatalf("启动 postgres 容器失败: %v", err)
	}
	t.Cleanup(func() {
		if err := pg.Terminate(context.Background()); err != nil {
			t.Logf("终止 postgres 容器失败: %v", err)
		}
	})
	host, err := pg.Host(ctx)
	if err != nil {
		t.Fatalf("获取容器 host 失败: %v", err)
	}
	port, err := pg.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("获取容器映射端口失败: %v", err)
	}
	dsn := "postgres://postgres:test@" + host + ":" + port.Port() + "/alertsystem?sslmode=disable"
	var st *store.Store
	for i := 0; i < 10; i++ {
		st, err = store.New(ctx, dsn)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(st.Close)
	applyMigrations(t, ctx, st)
	return st
}

func applyMigrations(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("读取 migrations 失败: %v", err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(filepath.Join(migrationsDir, f))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		if _, err := st.Pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("执行 %s 失败: %v", f, err)
		}
	}
}

// query 用一个足够宽的窗口取指标：多数用例不关心流量型指标的窗口边界，
// 边界本身由 TestQueryIndicators_NudgeFiredCountRespectsWindow 专门覆盖。
func query(t *testing.T, st *store.Store) Indicators {
	t.Helper()
	now := time.Now()
	return queryWindow(t, st, IndicatorWindow{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)})
}

func queryWindow(t *testing.T, st *store.Store, w IndicatorWindow) Indicators {
	t.Helper()
	var got Indicators
	if err := st.WithTx(context.Background(), func(tx pgx.Tx) error {
		var err error
		got, err = QueryIndicators(context.Background(), tx, w)
		return err
	}); err != nil {
		t.Fatalf("QueryIndicators: %v", err)
	}
	return got
}

func insertAlert(t *testing.T, st *store.Store, fingerprint, sourceState string) string {
	t.Helper()
	var id string
	err := st.Pool.QueryRow(context.Background(), `
		INSERT INTO alerts (fingerprint, status, severity, title, source, labels, resource_ref, resolve_policy, starts_at, last_seen_at, source_state)
		VALUES ($1, 'firing', 'warning', $1, 'test', '{}', '{}', 'source', now(), now(), $2)
		RETURNING id
	`, fingerprint, sourceState).Scan(&id)
	if err != nil {
		t.Fatalf("插入 alert %s 失败: %v", fingerprint, err)
	}
	return id
}

func insertFault(t *testing.T, st *store.Store, key, status string, ownerTeam *string, firstDetected, ackedAt *time.Time) string {
	t.Helper()
	now := time.Now().UTC()
	fd := now
	if firstDetected != nil {
		fd = *firstDetected
	}
	var id string
	err := st.Pool.QueryRow(context.Background(), `
		INSERT INTO faults (
			correlation_key, scope, status, severity, kind, title, confidence,
			first_detected_at, last_evaluated_at, owner_team_id, acked_at
		) VALUES ($1, 'capability', $2, 'warning', 'test', $1, 0.9, $3, $3, $4::uuid, $5)
		RETURNING id
	`, key, status, fd, ownerTeam, ackedAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入 fault %s 失败: %v", key, err)
	}
	return id
}

func TestQueryIndicators_EmptyDB_NullsNotZeroOrNaN(t *testing.T) {
	st := setupTestStore(t)
	got := query(t, st)
	if got.FiringMappingRate != nil {
		t.Fatalf("无 firing 告警时映射率应为 null，got %v", *got.FiringMappingRate)
	}
	if got.ActiveFaultsWithoutOwner != 0 {
		t.Fatalf("无 Fault 时无 owner 数应为 0，got %d", got.ActiveFaultsWithoutOwner)
	}
	if got.FaultAckMedianSeconds != nil {
		t.Fatalf("无认领样本时中位数应为 null，got %v", *got.FaultAckMedianSeconds)
	}
	if got.RecoveryNudgeFiredCount != 0 {
		t.Fatalf("无 fired nudge 应为 0，got %d", got.RecoveryNudgeFiredCount)
	}
	if got.UnresolvedIdentityCount != nil {
		t.Fatalf("无成功 ccloud 同步时未对齐数应为 null（uat 常态），got %v", *got.UnresolvedIdentityCount)
	}
}

func TestQueryIndicators_FiringMappingRate(t *testing.T) {
	st := setupTestStore(t)
	faultID := insertFault(t, st, "ck-map", "active", nil, nil, nil)
	a1 := insertAlert(t, st, "fp-mapped", "firing")
	a2 := insertAlert(t, st, "fp-unmapped", "firing")
	_ = insertAlert(t, st, "fp-resolved", "resolved") // 不进分母
	_, err := st.Pool.Exec(context.Background(), `
		INSERT INTO alert_mapping_results (alert_id, status, fault_id, reason_code, explanation, evaluated_at)
		VALUES ($1, 'mapped', $2, 'policy', 'ok', now())
	`, a1, faultID)
	if err != nil {
		t.Fatalf("写入 mapped 结果失败: %v", err)
	}
	_, err = st.Pool.Exec(context.Background(), `
		INSERT INTO alert_mapping_results (alert_id, status, reason_code, explanation, evaluated_at)
		VALUES ($1, 'unmapped', 'no_match', 'x', now())
	`, a2)
	if err != nil {
		t.Fatalf("写入 unmapped 结果失败: %v", err)
	}

	got := query(t, st)
	if got.FiringMappingRate == nil {
		t.Fatal("有 firing 告警时映射率不得为 null")
	}
	if *got.FiringMappingRate != 0.5 {
		t.Fatalf("映射率 = %v, want 0.5（2 条 firing，1 条已关联 Fault）", *got.FiringMappingRate)
	}
}

func TestQueryIndicators_FiringMappingRateZeroWhenNoneMapped(t *testing.T) {
	st := setupTestStore(t)
	_ = insertAlert(t, st, "fp-firing-unmapped", "firing")
	got := query(t, st)
	if got.FiringMappingRate == nil {
		t.Fatal("有 firing 但尚未映射时比率必须是 0，不能是 null")
	}
	if *got.FiringMappingRate != 0 {
		t.Fatalf("映射率 = %v, want 0", *got.FiringMappingRate)
	}
}

func TestQueryIndicators_OwnerlessAndAckMedian(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	var teamID string
	if err := st.Pool.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('sre') RETURNING id`).Scan(&teamID); err != nil {
		t.Fatalf("建团队失败: %v", err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	acked10 := base.Add(10 * time.Minute)
	acked30 := base.Add(30 * time.Minute)
	acked50 := base.Add(50 * time.Minute)
	insertFault(t, st, "ck-ownerless", "active", nil, &base, nil)
	insertFault(t, st, "ck-owned", "active", &teamID, &base, nil)
	insertFault(t, st, "ck-ack-10", "active", &teamID, &base, &acked10)
	insertFault(t, st, "ck-ack-30", "resolved", &teamID, &base, &acked30)
	insertFault(t, st, "ck-ack-50", "resolved", &teamID, &base, &acked50)

	got := query(t, st)
	if got.ActiveFaultsWithoutOwner != 1 {
		t.Fatalf("无 owner 的 active Fault = %d, want 1", got.ActiveFaultsWithoutOwner)
	}
	if got.FaultAckMedianSeconds == nil {
		t.Fatal("有认领样本时中位数不得为 null")
	}
	// 10 / 30 / 50 分钟，percentile_cont 中位 = 30 分钟 = 1800 秒
	if *got.FaultAckMedianSeconds < 1799 || *got.FaultAckMedianSeconds > 1801 {
		t.Fatalf("认领时长中位 = %v 秒, want ~1800", *got.FaultAckMedianSeconds)
	}
}

func TestQueryIndicators_NudgeFiredCount(t *testing.T) {
	st := setupTestStore(t)
	_, err := st.Pool.Exec(context.Background(), `
		INSERT INTO timers (kind, ref_id, fire_at, payload, status) VALUES
			('incident_recovery_nudge', 'inc-1', now(), '{}', 'fired'),
			('incident_recovery_nudge', 'inc-2', now(), '{}', 'pending'),
			('notify_send', 'x', now(), '{}', 'fired')
	`)
	if err != nil {
		t.Fatalf("插入 timers 失败: %v", err)
	}
	got := query(t, st)
	if got.RecoveryNudgeFiredCount != 1 {
		t.Fatalf("nudge fired 数 = %d, want 1（pending 与其他 kind 不计）", got.RecoveryNudgeFiredCount)
	}
}

func TestQueryIndicators_UnresolvedIgnoresStaleRowsAndNeedsSuccessfulSync(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	syncAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	_, err := st.Pool.Exec(ctx, `
		INSERT INTO unresolved_identities (namespace, value, first_seen, last_seen, sample_app) VALUES
			('ccloud.rely_node', 'still-open', $1, $2, 'app'),
			('ccloud.rely_node', 'already-aligned', $1, $3, 'app')
	`, syncAt.Add(-time.Hour), syncAt, syncAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("插入 unresolved 失败: %v", err)
	}

	// 只有失败同步：仍应 null，不能 count(*) 出 2。
	_, err = st.Pool.Exec(ctx, `
		INSERT INTO topology_sync_runs (source, status, observed_at, snapshot_hash, finished_at)
		VALUES ('ccloud', 'failed', $1, '', now())
	`, syncAt)
	if err != nil {
		t.Fatalf("插入失败同步失败: %v", err)
	}
	got := query(t, st)
	if got.UnresolvedIdentityCount != nil {
		t.Fatalf("仅有失败同步时未对齐数应为 null，got %v", *got.UnresolvedIdentityCount)
	}

	_, err = st.Pool.Exec(ctx, `
		INSERT INTO topology_sync_runs (source, status, observed_at, snapshot_hash, finished_at)
		VALUES ('ccloud', 'completed', $1, 'h', now())
	`, syncAt)
	if err != nil {
		t.Fatalf("插入成功同步失败: %v", err)
	}
	got = query(t, st)
	if got.UnresolvedIdentityCount == nil {
		t.Fatal("有成功同步后未对齐数不得为 null")
	}
	if *got.UnresolvedIdentityCount != 1 {
		t.Fatalf("未对齐数 = %d, want 1（已对齐的 last_seen 早于本次 observed_at 不计）", *got.UnresolvedIdentityCount)
	}

	later := syncAt.Add(time.Hour)
	_, err = st.Pool.Exec(ctx, `
		INSERT INTO topology_sync_runs (source, status, observed_at, snapshot_hash, finished_at)
		VALUES ('ccloud', 'completed', $1, 'h2', now())
	`, later)
	if err != nil {
		t.Fatalf("插入更新后的成功同步失败: %v", err)
	}
	got = query(t, st)
	if got.UnresolvedIdentityCount == nil {
		t.Fatal("有成功同步后即使当前为 0 也不得为 null")
	}
	if *got.UnresolvedIdentityCount != 0 {
		t.Fatalf("全部 last_seen 早于最新成功同步时应为 0，got %d", *got.UnresolvedIdentityCount)
	}
}

// 回归：恢复提醒发出次数是流量型指标，必须只统计窗口内 fire_at 的行。
// 没有窗口时这个数字既不是历史总量也不是某段时间的流量——timers 的 fired 行会被
// retention_sweep 按保留天数删除，全表计数会先涨后落，且随保留配置变化。
func TestQueryIndicators_NudgeFiredCountRespectsWindow(t *testing.T) {
	st := setupTestStore(t)
	_, err := st.Pool.Exec(context.Background(), `
		INSERT INTO timers (kind, ref_id, fire_at, payload, status) VALUES
			('incident_recovery_nudge', 'in-window',  now() - interval '2 hours', '{}', 'fired'),
			('incident_recovery_nudge', 'too-old',    now() - interval '40 hours', '{}', 'fired'),
			('incident_recovery_nudge', 'in-future',  now() + interval '2 hours', '{}', 'fired')
	`)
	if err != nil {
		t.Fatalf("插入 timers 失败: %v", err)
	}

	now := time.Now()
	got := queryWindow(t, st, IndicatorWindow{From: now.Add(-24 * time.Hour), To: now})
	if got.RecoveryNudgeFiredCount != 1 {
		t.Fatalf("窗口内 nudge 数 = %d, want 1（窗口外的历史行与未来行都不计）", got.RecoveryNudgeFiredCount)
	}
}
