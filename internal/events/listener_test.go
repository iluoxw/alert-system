//go:build integration

package events

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// 本文件是集成测试，需要真实 PostgreSQL（通过 testcontainers-go 拉起临时容器），
// 用 `integration` build tag 隔离，默认 `go test ./...` 不会编译/运行本文件。
// 手法与 internal/store/events_test.go 的 setupTestStore 一致：起容器、按文件名
// 顺序执行 migrations/*.up.sql（含本任务新增的 000011 触发器）。实测运行：
//   go test -tags=integration ./internal/events/... -run TestRunListener -v

const migrationsDir = "../../migrations"

// setupTestDSN 起一个临时 postgres:15-alpine 容器、跑完全部 migrations，返回可
// 直接连接的 DSN；测试结束时自动清理容器。
func setupTestDSN(t *testing.T) string {
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

	var conn *pgx.Conn
	for i := 0; i < 10; i++ {
		conn, err = pgx.Connect(ctx, dsn)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer conn.Close(ctx)

	applyMigrations(t, ctx, conn)
	return dsn
}

func applyMigrations(t *testing.T, ctx context.Context, conn *pgx.Conn) {
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
		if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("执行迁移 %s 失败: %v", f, err)
		}
	}
}

// TestRunListener_InsertAndUpdate_PublishesAlertID 验证 000011 的两个触发器
// （alerts_notify_changed）经 RunListener 到达 Bus 订阅者：先 INSERT 一条 alert，
// 断言收到其 UUID；再 UPDATE 该行，断言再收到一次同一 UUID。
func TestRunListener_InsertAndUpdate_PublishesAlertID(t *testing.T) {
	dsn := setupTestDSN(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewBus()
	ch, unsub := bus.Subscribe()
	defer unsub()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	go RunListener(ctx, dsn, bus, logger)

	// RunListener 建立 LISTEN 连接是异步的，给它一点时间再写入，避免第一条通知
	// 因为连接还没建好而错过（LISTEN 之前的 pg_notify 收不到，属预期行为，测试
	// 只需保证连接已就绪）。
	time.Sleep(500 * time.Millisecond)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer conn.Close(ctx)

	var alertID string
	err = conn.QueryRow(ctx, `
		INSERT INTO alerts (fingerprint, status, severity, title, source, resolve_policy, starts_at, last_seen_at)
		VALUES ('fp-listener-test', 'firing', 'warning', 'test alert', 'test', 'manual', now(), now())
		RETURNING id::text
	`).Scan(&alertID)
	if err != nil {
		t.Fatalf("插入 alert 失败: %v", err)
	}

	select {
	case got := <-ch:
		if got != alertID {
			t.Fatalf("INSERT 后收到 %q，期望 %q", got, alertID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("INSERT 后 5s 内未收到通知")
	}

	if _, err := conn.Exec(ctx, `UPDATE alerts SET status = 'acknowledged' WHERE id = $1`, alertID); err != nil {
		t.Fatalf("更新 alert 失败: %v", err)
	}

	select {
	case got := <-ch:
		if got != alertID {
			t.Fatalf("UPDATE 后收到 %q，期望 %q", got, alertID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UPDATE 后 5s 内未收到通知")
	}
}
