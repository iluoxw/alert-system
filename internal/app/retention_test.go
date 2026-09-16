//go:build integration

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"alert-system/internal/config"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
)

// 本文件是集成测试，需要真实 PostgreSQL（通过 testcontainers-go 拉起临时容器），
// 用 `integration` build tag 隔离，默认 `go test ./...`（不带 -tags=integration）
// 不会编译/运行本文件，无 docker 环境不受影响，与 internal/scheduler 包的集成测试
// 完全同一套模式（容器启动、迁移应用方式均照搬）。实测运行：
//   go test -tags=integration ./internal/app/... -v

const migrationsDir = "../../migrations"

// setupTestStore 起一个临时 postgres:15-alpine 容器、按文件名顺序执行
// migrations/*.up.sql，返回连好的 Store；测试结束时自动清理容器与连接池。
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
	pgContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("启动 postgres 容器失败: %v", err)
	}
	t.Cleanup(func() {
		if err := pgContainer.Terminate(context.Background()); err != nil {
			t.Logf("终止 postgres 容器失败: %v", err)
		}
	})

	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("获取容器 host 失败: %v", err)
	}
	port, err := pgContainer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("获取容器映射端口失败: %v", err)
	}
	dsn := fmt.Sprintf("postgres://postgres:test@%s:%s/alertsystem?sslmode=disable", host, port.Port())

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
		t.Fatalf("读取 migrations 目录失败: %v", err)
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
			t.Fatalf("读取迁移文件 %s 失败: %v", f, err)
		}
		if _, err := st.Pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("执行迁移 %s 失败: %v", f, err)
		}
	}
}

// insertTimer 直接插入一条 timers 行（绕过 Schedule），用于精确构造"哪些行该被清理、
// 哪些不该"的测试夹具：status/fire_at 组合任意指定。
func insertTimer(t *testing.T, ctx context.Context, st *store.Store, kind, refID, status string, fireAt time.Time) string {
	t.Helper()
	var id string
	err := st.Pool.QueryRow(ctx, `
		INSERT INTO timers (kind, ref_id, fire_at, payload, status)
		VALUES ($1, $2, $3, '{}'::jsonb, $4)
		RETURNING id
	`, kind, refID, fireAt, status).Scan(&id)
	if err != nil {
		t.Fatalf("插入 timers 行失败: %v", err)
	}
	return id
}

// insertNotificationLog 直接插入一条 notification_log 行，created_at 可指定为
// 历史时刻以模拟"早已过保留期"的记录。
func insertNotificationLog(t *testing.T, ctx context.Context, st *store.Store, groupKey string, createdAt time.Time) string {
	t.Helper()
	var id string
	err := st.Pool.QueryRow(ctx, `
		INSERT INTO notification_log (group_key, channel, target, idempotency_key, status, created_at, updated_at)
		VALUES ($1, 'webhook', 'http://x', $2, 'sent', $3, $3)
		RETURNING id
	`, groupKey, "key-"+groupKey, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入 notification_log 行失败: %v", err)
	}
	return id
}

func timerExists(t *testing.T, ctx context.Context, st *store.Store, id string) bool {
	t.Helper()
	var exists bool
	if err := st.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM timers WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("查询 timers 行是否存在失败: %v", err)
	}
	return exists
}

func notificationLogExists(t *testing.T, ctx context.Context, st *store.Store, id string) bool {
	t.Helper()
	var exists bool
	if err := st.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM notification_log WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("查询 notification_log 行是否存在失败: %v", err)
	}
	return exists
}

// pendingRetentionTimer 查询 retention_sweep/global 当前的 pending 记录（应当
// 恰好一条，见 timers_pending_kind_ref_uidx 唯一索引），返回其 fire_at。
func pendingRetentionTimer(t *testing.T, ctx context.Context, st *store.Store) (fireAt time.Time, found bool) {
	t.Helper()
	err := st.Pool.QueryRow(ctx, `
		SELECT fire_at FROM timers
		WHERE kind = $1 AND ref_id = $2 AND status = 'pending'
	`, TimerKindRetentionSweep, retentionSweepRefID).Scan(&fireAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return time.Time{}, false
		}
		t.Fatalf("查询 retention_sweep 续期记录失败: %v", err)
	}
	return fireAt, true
}

func testRetentionConfig() config.RetentionConfig {
	return config.RetentionConfig{
		TimersDays:          7,
		NotificationLogDays: 90,
		SweepInterval:       time.Hour,
		BatchSize:           1000,
	}
}

// runRetentionHandler 在一个新事务里调用 handler.Handle，模拟 scheduler
// processOneDue 里"handler 与业务副作用同一事务提交"的用法。
func runRetentionHandler(t *testing.T, ctx context.Context, st *store.Store, h *retentionHandler) {
	t.Helper()
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return h.Handle(ctx, tx, retentionSweepRefID, nil)
	}); err != nil {
		t.Fatalf("retentionHandler.Handle 失败: %v", err)
	}
}

// ① 只删终态 + 过期行；未过期行与 pending 行不动。
func TestRetentionHandler_OnlyDeletesTerminalAndExpired(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := testRetentionConfig()

	// timers：终态+早于 7 天保留期 -> 应删；终态但未过期 -> 不应删；pending 且早于
	// 保留期 -> 不应删（pending 是"还没触发"的将来任务，不受保留期约束）。
	expiredFired := insertTimer(t, ctx, st, "alert_ttl", "alert-old", "fired", now.AddDate(0, 0, -10))
	freshFired := insertTimer(t, ctx, st, "alert_ttl", "alert-fresh", "fired", now.AddDate(0, 0, -1))
	oldPending := insertTimer(t, ctx, st, "alert_ttl", "alert-pending-old", "pending", now.AddDate(0, 0, -10))
	expiredCanceled := insertTimer(t, ctx, st, "group_wait", "group-old", "canceled", now.AddDate(0, 0, -30))
	expiredFailed := insertTimer(t, ctx, st, "alert_repeat", "alert-failed-old", "failed", now.AddDate(0, 0, -8))

	// notification_log：早于 90 天保留期 -> 应删；未过期 -> 不应删。
	expiredLog := insertNotificationLog(t, ctx, st, "group-a", now.AddDate(0, 0, -100))
	freshLog := insertNotificationLog(t, ctx, st, "group-b", now.AddDate(0, 0, -1))

	h := newRetentionHandler(scheduler.New(st, time.Hour), cfg)
	h.now = func() time.Time { return now }

	runRetentionHandler(t, ctx, st, h)

	if timerExists(t, ctx, st, expiredFired) {
		t.Error("过期 fired timer 应被删除")
	}
	if timerExists(t, ctx, st, expiredCanceled) {
		t.Error("过期 canceled timer 应被删除")
	}
	if timerExists(t, ctx, st, expiredFailed) {
		t.Error("过期 failed timer 应被删除")
	}
	if !timerExists(t, ctx, st, freshFired) {
		t.Error("未过期 fired timer 不应被删除")
	}
	if !timerExists(t, ctx, st, oldPending) {
		t.Error("pending timer 不应被删除，无论 fire_at 多旧")
	}
	if !notificationLogExists(t, ctx, st, freshLog) {
		t.Error("未过期 notification_log 不应被删除")
	}
	if notificationLogExists(t, ctx, st, expiredLog) {
		t.Error("过期 notification_log 应被删除")
	}
}

// ② handler 跑完后存在一条新的 pending 续期定时器。
func TestRetentionHandler_AlwaysRenewsAfterRun(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := testRetentionConfig()

	h := newRetentionHandler(scheduler.New(st, time.Hour), cfg)
	h.now = func() time.Time { return now }

	runRetentionHandler(t, ctx, st, h)

	fireAt, found := pendingRetentionTimer(t, ctx, st)
	if !found {
		t.Fatal("handler 跑完后应存在 retention_sweep/global 的 pending 续期记录")
	}
	if !fireAt.Equal(now.Add(cfg.SweepInterval)) {
		t.Fatalf("无积压时续期间隔应为 SweepInterval，fireAt = %v, want %v", fireAt, now.Add(cfg.SweepInterval))
	}
}

// ③ 批满时续期时间是 1 分钟档而非 SweepInterval 档（积压快速消化）。
func TestRetentionHandler_BatchFull_RenewsWithCatchUpBackoff(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg := testRetentionConfig()
	cfg.BatchSize = 2 // 压小批量上限，方便用少量行触发"删满一批"

	for i := 0; i < 3; i++ {
		insertTimer(t, ctx, st, "alert_ttl", fmt.Sprintf("alert-old-%d", i), "fired", now.AddDate(0, 0, -10))
	}

	h := newRetentionHandler(scheduler.New(st, time.Hour), cfg)
	h.now = func() time.Time { return now }

	runRetentionHandler(t, ctx, st, h)

	fireAt, found := pendingRetentionTimer(t, ctx, st)
	if !found {
		t.Fatal("批满后仍应存在续期记录")
	}
	if !fireAt.Equal(now.Add(retentionCatchUpBackoff)) {
		t.Fatalf("批满时续期间隔应为 retentionCatchUpBackoff(1分钟)，fireAt = %v, want %v", fireAt, now.Add(retentionCatchUpBackoff))
	}

	// 3 条过期行、批量上限 2：第一轮应恰好删掉 2 条，剩 1 条留给下一轮。
	remaining := 0
	for i := 0; i < 3; i++ {
		var exists bool
		if err := st.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM timers WHERE kind='alert_ttl' AND ref_id=$1)`, fmt.Sprintf("alert-old-%d", i)).Scan(&exists); err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if exists {
			remaining++
		}
	}
	if remaining != 1 {
		t.Fatalf("批量上限为 2、共 3 条过期行，本轮后应剩 1 条，实际剩 %d 条", remaining)
	}
}

// ④ ReconcileRetentionTimer 首次对账会补建一条 pending 记录；已存在 pending 记录
// 时不打断其 fire_at（ScheduleIfAbsent 语义）。
func TestReconcileRetentionTimer_CreatesOnlyIfAbsent(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sched := scheduler.New(st, time.Hour)
	cfg := testRetentionConfig()

	if err := ReconcileRetentionTimer(ctx, st, sched, cfg); err != nil {
		t.Fatalf("首次对账失败: %v", err)
	}
	firstFireAt, found := pendingRetentionTimer(t, ctx, st)
	if !found {
		t.Fatal("首次对账后应存在 pending 续期记录")
	}

	if err := ReconcileRetentionTimer(ctx, st, sched, cfg); err != nil {
		t.Fatalf("二次对账失败: %v", err)
	}
	secondFireAt, found := pendingRetentionTimer(t, ctx, st)
	if !found {
		t.Fatal("二次对账后仍应存在 pending 续期记录")
	}
	if !secondFireAt.Equal(firstFireAt) {
		t.Fatalf("已存在 pending 记录时不应被打断/重排期，first=%v second=%v", firstFireAt, secondFireAt)
	}
}

// 零值配置（绕过 config.Load 的结构体字面量）必须被拒绝，且不注册任何定时器，
// 否则会立即触发清理并空转续期。
func TestReconcileRetentionTimer_RejectsInvalidConfig(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sched := scheduler.New(st, time.Hour)

	if err := ReconcileRetentionTimer(ctx, st, sched, config.RetentionConfig{}); err == nil {
		t.Fatal("零值 retention 配置应被拒绝")
	}
	if _, found := pendingRetentionTimer(t, ctx, st); found {
		t.Fatal("配置非法时不应注册 retention_sweep 定时器")
	}
}
