//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"alert-system/internal/domain"
)

// 本文件是集成测试，需要真实 PostgreSQL（通过 testcontainers-go 拉起临时容器），
// 用 `integration` build tag 隔离，默认 `go test ./...`（不带 -tags=integration）
// 不会编译/运行本文件，无 docker 环境不受影响。实测运行：
//   go test -tags=integration ./internal/store/... -run TestEventQueue -v

const migrationsDir = "../../migrations"

// setupTestStore 起一个临时 postgres:15-alpine 容器、按文件名顺序执行
// migrations/*.up.sql，返回连好的 Store；测试结束时自动清理容器与连接池。
func setupTestStore(t *testing.T) *Store {
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

	var st *Store
	for i := 0; i < 10; i++ {
		st, err = New(ctx, dsn)
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

func applyMigrations(t *testing.T, ctx context.Context, st *Store) {
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

func seedEvents(t *testing.T, ctx context.Context, st *Store, queue *EventQueue, n int) {
	t.Helper()
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			event := domain.AlertEvent{
				Fingerprint:   fmt.Sprintf("fp-%d", i),
				RuleID:        "rule-x",
				SignalKind:    domain.SignalKindMetric,
				Severity:      domain.SeverityWarning,
				Status:        domain.StatusFiring,
				Title:         "test alert",
				Labels:        map[string]string{"seq": fmt.Sprintf("%d", i)},
				Source:        "test",
				ResolvePolicy: domain.ResolvePolicyManual,
				StartsAt:      time.Now(),
			}
			if _, err := queue.Enqueue(ctx, tx, "", event); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed 事件失败: %v", err)
	}
}

// TestEventQueue_ConcurrentDequeue_NoLossNoDuplication 验证 R1.6 at-least-once
// 语义中「不重不漏」的一半：两个并发 worker 用 SKIP LOCKED 消费同一批事件，
// 每条事件恰好被处理一次，且全部 1000 条都被处理。
func TestEventQueue_ConcurrentDequeue_NoLossNoDuplication(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	queue := NewEventQueue()

	const total = 1000
	seedEvents(t, ctx, st, queue, total)

	var (
		mu        sync.Mutex
		seen      = make(map[string]int)
		processed int
	)

	worker := func(wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			var batchIDs []string
			err := st.WithTx(ctx, func(tx pgx.Tx) error {
				batch, err := queue.Dequeue(ctx, tx, 10)
				if err != nil {
					return err
				}
				if len(batch) == 0 {
					return nil
				}
				mu.Lock()
				for _, e := range batch {
					seen[e.ID]++
					batchIDs = append(batchIDs, e.ID)
				}
				processed += len(batch)
				mu.Unlock()
				return queue.MarkProcessed(ctx, tx, batchIDs)
			})
			if err != nil {
				t.Errorf("worker 事务失败: %v", err)
				return
			}
			if len(batchIDs) == 0 {
				return // 没有更多待处理事件
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go worker(&wg)
	go worker(&wg)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if processed != total {
		t.Fatalf("processed = %d, want %d", processed, total)
	}
	if len(seen) != total {
		t.Fatalf("被处理过的不同事件数 = %d, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("事件 %s 被处理了 %d 次，期望恰好 1 次", id, n)
		}
	}
}

// TestEventQueue_CrashBeforeMark_EventStaysPending 模拟 worker 在 Dequeue 之后、
// MarkProcessed 与提交之前崩溃（这里用显式 Rollback 代替真实崩溃）：事件必须仍是
// pending，崩溃恢复后可被重新 Dequeue 到（R1.6 at-least-once 的另一半：不丢失）。
func TestEventQueue_CrashBeforeMark_EventStaysPending(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	queue := NewEventQueue()

	seedEvents(t, ctx, st, queue, 1)

	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	batch, err := queue.Dequeue(ctx, tx, 10)
	if err != nil {
		t.Fatalf("dequeue 失败: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("期望取到 1 条事件，got %d", len(batch))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		redo, err := queue.Dequeue(ctx, tx, 10)
		if err != nil {
			return err
		}
		if len(redo) != 1 || redo[0].ID != batch[0].ID {
			return fmt.Errorf("崩溃恢复后应重新取到同一条 pending 事件，got %+v", redo)
		}
		return queue.MarkProcessed(ctx, tx, []string{redo[0].ID})
	})
	if err != nil {
		t.Fatalf("崩溃恢复后的事务失败: %v", err)
	}
}

// TestEventQueue_MarkFailed_NotDequeuedAgain 验证 MarkFailed 后事件不再被
// Dequeue 取出（failed 状态需要独立的重试/死信路径，不会被当作 pending 重复消费）。
func TestEventQueue_MarkFailed_NotDequeuedAgain(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	queue := NewEventQueue()

	seedEvents(t, ctx, st, queue, 1)

	var failedID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		batch, err := queue.Dequeue(ctx, tx, 10)
		if err != nil {
			return err
		}
		if len(batch) != 1 {
			return fmt.Errorf("期望取到 1 条事件，got %d", len(batch))
		}
		failedID = batch[0].ID
		return queue.MarkFailed(ctx, tx, failedID, "模拟处理失败")
	})
	if err != nil {
		t.Fatalf("标记失败事务出错: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		again, err := queue.Dequeue(ctx, tx, 10)
		if err != nil {
			return err
		}
		if len(again) != 0 {
			return fmt.Errorf("failed 状态的事件不应再被 Dequeue 到，got %+v", again)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("二次 dequeue 事务出错: %v", err)
	}
}

// TestStore_WithTx_RollsBackOnError 验证 WithTx 的事务边界本身：fn 返回错误时
// 整个事务（包括其中的 Enqueue）都必须回滚，不留下部分写入。
func TestStore_WithTx_RollsBackOnError(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	queue := NewEventQueue()

	wantErr := errors.New("boom")
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := queue.Enqueue(ctx, tx, "", domain.AlertEvent{Fingerprint: "rollback-test"}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTx err = %v, want %v", err, wantErr)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		batch, err := queue.Dequeue(ctx, tx, 100)
		if err != nil {
			return err
		}
		for _, e := range batch {
			if e.Event.Fingerprint == "rollback-test" {
				return fmt.Errorf("回滚的事务不应把事件持久化")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("验证回滚结果的事务失败: %v", err)
	}
}
