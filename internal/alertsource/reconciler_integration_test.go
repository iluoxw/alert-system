//go:build integration

package alertsource

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"alert-system/internal/store"
)

// 本文件是集成测试，需要真实 PostgreSQL（通过 testcontainers-go 拉起临时容器），
// 用 `integration` build tag 隔离，与 internal/scheduler/scheduler_integration_test.go
// 完全同一套模式（容器启动、迁移应用方式均照搬，setupTestStore 是本包的独立副本，
// 与其他包各自持有一份是既有约定，不共享）。实测运行：
//
//	go test -tags=integration ./internal/alertsource/... -run TestReconciler -v
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

// fakeAlertProjector 记录每次 Project 调用，固定返回预设结果，不做任何真实映射
// （本测试只关心 Reconciler 是否在正确的两处调用点调用了 Project 与
// faultActivation，映射逻辑本身由 internal/mapping 包的单测覆盖）。
type fakeAlertProjector struct {
	mu     sync.Mutex
	result domain.AlertMappingResult
	calls  []domain.Alert
}

func (f *fakeAlertProjector) Project(ctx context.Context, tx pgx.Tx, alert domain.Alert) (domain.AlertMappingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, alert)
	return f.result, nil
}

func (f *fakeAlertProjector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeFaultActivation 记录每次被调用时收到的 AlertMappingResult。
type fakeFaultActivation struct {
	mu    sync.Mutex
	calls []domain.AlertMappingResult
}

func (f *fakeFaultActivation) trigger(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, result)
	return nil
}

func (f *fakeFaultActivation) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// alertmanagerPayload 组装最小合法的 Alertmanager v2 /api/v2/alerts 响应体，
// firing=true 时不带 endsAt（对齐 client_test.go 的既有测试数据格式）。
func alertmanagerPayload(fingerprint string, firing bool, startsAt time.Time) string {
	state := "active"
	if !firing {
		state = "resolved"
	}
	return fmt.Sprintf(`[{"annotations":{"summary":"test"},"receivers":[{"name":"ops"}],`+
		`"fingerprint":%q,"startsAt":%q,"updatedAt":%q,"generatorURL":"https://prom.example/graph",`+
		`"labels":{"alertname":"TestAlert"},"status":{"state":%q,"silencedBy":[],"inhibitedBy":[],"mutedBy":[]}}]`,
		fingerprint, startsAt.UTC().Format(time.RFC3339), startsAt.UTC().Format(time.RFC3339), state)
}

// TestReconciler_FaultActivationTriggeredOnBothPaths 覆盖 R5.10：Reconciler 的两处
// Project 调用点（unknown->firing 恢复、快照缺失）都必须触发 faultActivation 钩子。
//
// 三轮对账驱动状态机：
//  1. 首次出现 fp1 firing -> 快照新建 occurrence，触发一次（webhook 丢失时对账是唯一
//     入口；曾经不投影不触发，等同漏报）。
//  2. 服务端不再返回 fp1 -> MarkMissingAfterSnapshot 判定 miss，MissingConfirmations
//     强制 >=2，第一次 miss 只到 unknown，进入"快照缺失"调用点，触发一次。
//  3. fp1 重新出现 firing -> UpsertAuthoritativeOccurrence 识别 previous=unknown ->
//     进入"unknown->firing 恢复"调用点，再触发一次。
func TestReconciler_FaultActivationTriggeredOnBothPaths(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sources := store.NewAlertSourceRepo()
	alerts := store.NewAlertRepo()
	timeline := store.NewTimelineRepo()
	integrations := store.NewIntegrationRepo()

	startsAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var round int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch round {
		case 0:
			_, _ = w.Write([]byte(alertmanagerPayload("fp1", true, startsAt)))
		case 1:
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(alertmanagerPayload("fp1", true, startsAt)))
		}
	}))
	defer server.Close()

	sourceConfig, err := json.Marshal(store.AlertmanagerSourceConfig{
		APIURL: server.URL, ReconcileIntervalSeconds: 30, StaleAfterSeconds: 3600, MissingConfirmations: 2,
	})
	if err != nil {
		t.Fatalf("序列化 source 配置失败: %v", err)
	}
	var source *store.Integration
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		source, err = integrations.Create(ctx, tx, store.Integration{
			Kind: "alertmanager", Name: "test-source-fault-trigger", HookIDHash: "hash-fault-trigger",
			DefaultSeverity: "warning", Enabled: true, Config: sourceConfig,
		})
		return err
	}); err != nil {
		t.Fatalf("创建 integration 失败: %v", err)
	}

	projector := &fakeAlertProjector{result: domain.AlertMappingResult{FaultID: "fault-x", Transition: domain.FaultTransitionEnteredActive}}
	trigger := &fakeFaultActivation{}
	reconciler := NewReconciler(st, sources, alerts, timeline, NewClient(server.Client()), slog.Default()).
		WithProjector(projector).WithFaultActivation(trigger.trigger)

	// 第 1 轮：创建。
	if _, err := reconciler.ReconcileSource(ctx, source.ID); err != nil {
		t.Fatalf("第 1 轮对账失败: %v", err)
	}
	if trigger.callCount() != 1 {
		t.Fatalf("快照首次出现的告警必须投影并触发 Fault 通知，got %d", trigger.callCount())
	}

	// 第 2 轮：快照缺失路径。
	mu.Lock()
	round = 1
	mu.Unlock()
	if _, err := reconciler.ReconcileSource(ctx, source.ID); err != nil {
		t.Fatalf("第 2 轮对账失败: %v", err)
	}
	if trigger.callCount() != 2 {
		t.Fatalf("快照缺失路径应再触发一次（累计 2 次），got %d", trigger.callCount())
	}

	// 第 3 轮：unknown -> firing 恢复路径。
	mu.Lock()
	round = 2
	mu.Unlock()
	if _, err := reconciler.ReconcileSource(ctx, source.ID); err != nil {
		t.Fatalf("第 3 轮对账失败: %v", err)
	}
	if trigger.callCount() != 3 {
		t.Fatalf("恢复路径应再触发一次（累计 3 次），got %d", trigger.callCount())
	}
}

// TestReconciler_FaultActivationNotConfigured 覆盖未注入钩子（WithFaultActivation
// 未调用）时的默认行为：不 panic，对账正常完成。
func TestReconciler_FaultActivationNotConfigured(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	sources := store.NewAlertSourceRepo()
	alerts := store.NewAlertRepo()
	timeline := store.NewTimelineRepo()
	integrations := store.NewIntegrationRepo()

	startsAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(alertmanagerPayload("fp-no-hook", true, startsAt)))
	}))
	defer server.Close()

	sourceConfig, err := json.Marshal(store.AlertmanagerSourceConfig{
		APIURL: server.URL, ReconcileIntervalSeconds: 30, StaleAfterSeconds: 3600, MissingConfirmations: 2,
	})
	if err != nil {
		t.Fatalf("序列化 source 配置失败: %v", err)
	}
	var source *store.Integration
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		source, err = integrations.Create(ctx, tx, store.Integration{
			Kind: "alertmanager", Name: "test-source-no-hook", HookIDHash: "hash-no-hook",
			DefaultSeverity: "warning", Enabled: true, Config: sourceConfig,
		})
		return err
	}); err != nil {
		t.Fatalf("创建 integration 失败: %v", err)
	}

	projector := &fakeAlertProjector{result: domain.AlertMappingResult{FaultID: "fault-y", Transition: domain.FaultTransitionCreatedActive}}
	reconciler := NewReconciler(st, sources, alerts, timeline, NewClient(server.Client()), slog.Default()).
		WithProjector(projector)

	if _, err := reconciler.ReconcileSource(ctx, source.ID); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
}
