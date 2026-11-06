//go:build integration

package scheduler

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"alert-system/internal/store"
)

// 本文件是集成测试，需要真实 PostgreSQL（通过 testcontainers-go 拉起临时容器），
// 用 `integration` build tag 隔离，默认 `go test ./...`（不带 -tags=integration）
// 不会编译/运行本文件，无 docker 环境不受影响，与 store 包 events_test.go 完全同一套
// 模式（容器启动、迁移应用方式均照搬）。实测运行：
//   go test -tags=integration ./internal/scheduler/... -v

const migrationsDir = "../../migrations"

// testDSNEnv 指向一个已经跑着的 PostgreSQL（URL 形式的管理连接串，库名任意）。
// 设置了它就跳过 testcontainers，改为在该实例上为每个测试单独建库/删库——给没有
// Docker 但有 PostgreSQL 的开发机与 CI 用，隔离级别与"每个测试一个容器"等价。
const testDSNEnv = "ALERT_SYSTEM_TEST_DSN"

// setupTestStore 起一个临时 postgres:15-alpine 容器、按文件名顺序执行
// migrations/*.up.sql，返回连好的 Store；测试结束时自动清理容器与连接池。
// 设置了 testDSNEnv 时走 setupTestStoreFromDSN，不需要 Docker。
func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()

	if dsn := os.Getenv(testDSNEnv); dsn != "" {
		return setupTestStoreFromDSN(t, dsn)
	}

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

// setupTestStoreFromDSN 在 adminDSN 指向的实例上新建一个本测试专属的数据库、跑完
// migrations，返回连到该库的 Store；测试结束时先关连接池再删库。
func setupTestStoreFromDSN(t *testing.T, adminDSN string) *store.Store {
	t.Helper()
	ctx := context.Background()

	admin, err := store.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("连接 %s 指向的数据库失败: %v", testDSNEnv, err)
	}

	// 库名取测试名 + 纳秒时间戳：同一个实例上并行跑多个测试也不会撞库。
	dbName := fmt.Sprintf("sched_test_%s_%d", sanitizeDBName(t.Name()), time.Now().UnixNano())
	if _, err := admin.Pool.Exec(ctx, `CREATE DATABASE "`+dbName+`"`); err != nil {
		admin.Close()
		t.Fatalf("创建测试数据库 %s 失败: %v", dbName, err)
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		admin.Close()
		t.Fatalf("解析 %s 失败（需要 URL 形式的 DSN）: %v", testDSNEnv, err)
	}
	u.Path = "/" + dbName

	st, err := store.New(ctx, u.String())
	if err != nil {
		admin.Close()
		t.Fatalf("连接测试数据库 %s 失败: %v", dbName, err)
	}
	t.Cleanup(func() {
		st.Close()
		// FORCE 断开残留连接：Run worker 的 goroutine 可能刚好还在收尾。
		if _, err := admin.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+dbName+`" WITH (FORCE)`); err != nil {
			t.Logf("删除测试数据库 %s 失败: %v", dbName, err)
		}
		admin.Close()
	})

	applyMigrations(t, ctx, st)
	return st
}

// sanitizeDBName 把测试名里 PostgreSQL 标识符不接受的字符换成下划线，并截断长度
// （标识符上限 63 字节，还要给时间戳后缀留位置）。
func sanitizeDBName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, name)
	if len(cleaned) > 24 {
		cleaned = cleaned[:24]
	}
	return cleaned
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

// testClock 是 Clock 的可控实现：测试通过 Set 精确推进时间，让「是否到期」的判断
// 不依赖真实 wall-clock 流逝，用 mutex 保护以支持 Run 所在 goroutine 与测试主
// goroutine 并发访问。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(t time.Time) *testClock {
	return &testClock{now: t}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func timerStatus(t *testing.T, ctx context.Context, st *store.Store, kind, refID string) string {
	t.Helper()
	var status string
	err := st.Pool.QueryRow(ctx, `SELECT status FROM timers WHERE kind = $1 AND ref_id = $2`, kind, refID).Scan(&status)
	if err != nil {
		t.Fatalf("查询定时器 kind=%s ref_id=%s 状态失败: %v", kind, refID, err)
	}
	return status
}

func timerCount(t *testing.T, ctx context.Context, st *store.Store, kind, refID string) int {
	t.Helper()
	var n int
	err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM timers WHERE kind = $1 AND ref_id = $2`, kind, refID).Scan(&n)
	if err != nil {
		t.Fatalf("统计定时器 kind=%s ref_id=%s 行数失败: %v", kind, refID, err)
	}
	return n
}

func waitForStatus(t *testing.T, ctx context.Context, st *store.Store, kind, refID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := timerStatus(t, ctx, st, kind, refID); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待定时器 kind=%s ref_id=%s 状态变为 %q 超时，当前状态 %q", kind, refID, want, timerStatus(t, ctx, st, kind, refID))
}

// insertPendingTimer 绕过 Schedule 直接插入一条 pending 记录，用于模拟「进程重启前
// 就已经写入数据库、且早在宕机期间就已到期」的历史定时器（场景④）。
func insertPendingTimer(t *testing.T, ctx context.Context, st *store.Store, kind, refID string, fireAt time.Time) {
	t.Helper()
	_, err := st.Pool.Exec(ctx, `
		INSERT INTO timers (kind, ref_id, fire_at, payload, status)
		VALUES ($1, $2, $3, '{}'::jsonb, 'pending')
	`, kind, refID, fireAt)
	if err != nil {
		t.Fatalf("直接插入 pending 定时器失败: %v", err)
	}
}

// ① Schedule 后到期触发，状态变 fired。
func TestScheduler_ScheduleThenDue_Fires(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sched := New(st, 20*time.Millisecond)
	clock := newTestClock(time.Now())
	sched.clock = clock

	var fired int32
	sched.RegisterHandler("ttl", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		atomic.AddInt32(&fired, 1)
		return nil
	})

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "ttl", "alert-1", clock.Now().Add(200*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	time.Sleep(60 * time.Millisecond)
	if got := timerStatus(t, ctx, st, "ttl", "alert-1"); got != "pending" {
		t.Fatalf("未到期前状态 = %q, want pending", got)
	}

	clock.Set(clock.Now().Add(300 * time.Millisecond))
	waitForStatus(t, ctx, st, "ttl", "alert-1", "fired", 2*time.Second)

	if atomic.LoadInt32(&fired) != 1 {
		t.Fatalf("handler 被调用 %d 次, want 1", fired)
	}
}

// ② Cancel 后不触发。
func TestScheduler_Cancel_PreventsFiring(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sched := New(st, 20*time.Millisecond)
	clock := newTestClock(time.Now())
	sched.clock = clock

	sched.RegisterHandler("ttl", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		return nil
	})

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "ttl", "alert-2", clock.Now().Add(50*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Cancel(ctx, tx, "ttl", "alert-2")
	})
	if err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(200 * time.Millisecond))
	time.Sleep(200 * time.Millisecond) // 给若干轮轮询留出时间

	if got := timerStatus(t, ctx, st, "ttl", "alert-2"); got != "canceled" {
		t.Fatalf("Cancel 后状态 = %q, want canceled（不应被触发）", got)
	}
}

// ③ 同 (kind, refID) 二次 Schedule 替换 fire_at：旧时间不触发，新时间触发。
func TestScheduler_ScheduleTwice_ReplacesFireAt(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sched := New(st, 20*time.Millisecond)
	base := time.Now()
	clock := newTestClock(base)
	sched.clock = clock

	sched.RegisterHandler("ttl", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		return nil
	})

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "ttl", "alert-3", base.Add(100*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("首次 Schedule 失败: %v", err)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "ttl", "alert-3", base.Add(300*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("二次 Schedule（替换）失败: %v", err)
	}

	if n := timerCount(t, ctx, st, "ttl", "alert-3"); n != 1 {
		t.Fatalf("同 (kind,refID) 二次 Schedule 后行数 = %d, want 1（应替换而非新增）", n)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	// 推进到旧 fire_at（100ms）之后、新 fire_at（300ms）之前：不应触发。
	clock.Set(base.Add(150 * time.Millisecond))
	time.Sleep(100 * time.Millisecond)
	if got := timerStatus(t, ctx, st, "ttl", "alert-3"); got != "pending" {
		t.Fatalf("推进到旧 fire_at 之后、新 fire_at 之前，状态 = %q, want pending（旧时间不应触发）", got)
	}

	// 推进到新 fire_at 之后：应当触发。
	clock.Set(base.Add(350 * time.Millisecond))
	waitForStatus(t, ctx, st, "ttl", "alert-3", "fired", 2*time.Second)
}

// ④「重启补触发」：进程重启前已经写入数据库、且早在宕机期间就已到期的 pending
// 定时器，启动 Run 后必须被补触发。
func TestScheduler_RestartRecovery_FiresAlreadyExpiredTimer(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	insertPendingTimer(t, ctx, st, "ttl", "alert-4", past)

	sched := New(st, 20*time.Millisecond)
	sched.clock = newTestClock(time.Now()) // "重启后"的当前时间，晚于 past

	var fired int32
	sched.RegisterHandler("ttl", func(_ context.Context, _ pgx.Tx, refID string, _ []byte) error {
		if refID != "alert-4" {
			return fmt.Errorf("unexpected refID %q", refID)
		}
		atomic.AddInt32(&fired, 1)
		return nil
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	waitForStatus(t, ctx, st, "ttl", "alert-4", "fired", 2*time.Second)
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatalf("handler 被调用 %d 次, want 1", fired)
	}
}

// ⑤ handler panic：该条定时器标记 failed，其余定时器（含之后新到期的）照常处理，
// worker 不能因为一次 panic 而死掉。
func TestScheduler_HandlerPanic_MarksFailedAndContinuesProcessing(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sched := New(st, 20*time.Millisecond)
	clock := newTestClock(time.Now())
	sched.clock = clock

	var okFired int32
	sched.RegisterHandler("boom", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		panic("模拟 handler 崩溃")
	})
	sched.RegisterHandler("ok", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
		atomic.AddInt32(&okFired, 1)
		return nil
	})

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := sched.Schedule(ctx, tx, "boom", "alert-5", clock.Now().Add(10*time.Millisecond), nil); err != nil {
			return err
		}
		return sched.Schedule(ctx, tx, "ok", "alert-6", clock.Now().Add(10*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sched.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))

	waitForStatus(t, ctx, st, "boom", "alert-5", "failed", 2*time.Second)
	waitForStatus(t, ctx, st, "ok", "alert-6", "fired", 2*time.Second)

	if atomic.LoadInt32(&okFired) != 1 {
		t.Fatalf("正常 handler 被调用 %d 次, want 1（worker 不应因另一个 handler panic 而死掉）", okFired)
	}

	// 再验证一次：panic 之后 worker 仍然存活，能继续处理后续新到期的定时器。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return sched.Schedule(ctx, tx, "ok", "alert-7", clock.Now().Add(10*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("panic 之后再次 Schedule 失败: %v", err)
	}
	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, "ok", "alert-7", "fired", 2*time.Second)
}

// ⑥ 两个 Run worker 并发轮询同一个 store：同一条定时器只被其中一个 worker 触发
// 恰好一次（FOR UPDATE SKIP LOCKED 防止重复触发）。
func TestScheduler_ConcurrentWorkers_FiresExactlyOnce(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	clock := newTestClock(time.Now())

	var fired int32
	newWorker := func() *Scheduler {
		s := New(st, 15*time.Millisecond)
		s.clock = clock
		s.RegisterHandler("ttl", func(_ context.Context, _ pgx.Tx, _ string, _ []byte) error {
			atomic.AddInt32(&fired, 1)
			return nil
		})
		return s
	}
	w1 := newWorker()
	w2 := newWorker()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return w1.Schedule(ctx, tx, "ttl", "alert-8", clock.Now().Add(10*time.Millisecond), nil)
	})
	if err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w1.Run(runCtx)
	go w2.Run(runCtx)

	clock.Set(clock.Now().Add(100 * time.Millisecond))
	waitForStatus(t, ctx, st, "ttl", "alert-8", "fired", 2*time.Second)

	// 再多等几轮轮询，确认没有被另一个 worker 再次触发。
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("handler 被调用 %d 次, want 恰好 1 次（SKIP LOCKED 应防止同一定时器被两个 worker 同时处理）", got)
	}
}
