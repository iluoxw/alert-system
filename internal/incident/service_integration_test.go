//go:build integration

package incident

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
// 用 `integration` build tag 隔离；容器启动/迁移应用方式照搬
// internal/scheduler/scheduler_integration_test.go 与 internal/store/events_test.go
// 的既有模式（同一套约定在多个包各自复制一份，本仓库没有共享 testutil 包）。实测运行：
//   go test -tags=integration ./internal/incident/... -v
//
// incident.Service（533 行）在本文件之前没有任何服务层测试——domain 层只测纯函数
// 门槛，api 层只测脱敏；真实 Service 的事务边界、Fault 状态联动、primary 切换审计
// 从未被驱动过。以下测试覆盖 CLAUDE.md 不变量 11/18 描述的双重关闭门槛与多 Fault
// 容器语义。

const migrationsDir = "../../migrations"

const testDSNEnv = "ALERT_SYSTEM_TEST_DSN"

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

func setupTestStoreFromDSN(t *testing.T, adminDSN string) *store.Store {
	t.Helper()
	ctx := context.Background()

	admin, err := store.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("连接 %s 指向的数据库失败: %v", testDSNEnv, err)
	}

	dbName := fmt.Sprintf("incident_test_%s_%d", sanitizeDBName(t.Name()), time.Now().UnixNano())
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
		if _, err := admin.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+dbName+`" WITH (FORCE)`); err != nil {
			t.Logf("删除测试数据库 %s 失败: %v", dbName, err)
		}
		admin.Close()
	})

	applyMigrations(t, ctx, st)
	return st
}

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

func mustUser(t *testing.T, ctx context.Context, st *store.Store, username string) *store.User {
	t.Helper()
	users := store.NewUserRepo()
	var u *store.User
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		u, err = users.Create(ctx, tx, store.User{Username: username, PasswordHash: "x", DisplayName: username})
		return err
	})
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", username, err)
	}
	return u
}

// newFault 直接以 active 状态 upsert 一个 Fault，跳过自动关联评估流程——服务层
// 测试只关心 Service 拿到既定 Fault 状态后的行为，不重复覆盖 correlation/evidence
// 投影逻辑（那是 internal/mapping 与 internal/impact 的职责）。
func newFault(t *testing.T, ctx context.Context, st *store.Store, key string, status domain.FaultStatus) domain.Fault {
	t.Helper()
	faults := store.NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var fault domain.Fault
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		fault, _, err = faults.Upsert(ctx, tx, domain.Fault{
			CorrelationKey: key, Scope: domain.FaultScopeCapability, Status: status,
			Severity: domain.SeverityCritical, Kind: "integration_test_fault",
			Title: key, EntityRef: key, Confidence: 0.9,
			FirstDetected: now, LastEvaluated: now,
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建 Fault %s 失败: %v", key, err)
	}
	return fault
}

// confirmBusinessImpact 给 faultID 造一条 confirmed 的 business_system 影响：先导入
// 一个 business_system 拓扑节点，再依次落 automation potential -> human confirmed，
// 复用 FaultRepo 现有两段式写入（human 裁决只能 UPDATE 既有行，见 UpsertHumanImpact
// 文档），不重复 store 层已经覆盖过的 confirm 语义本身。
func confirmBusinessImpact(t *testing.T, ctx context.Context, st *store.Store, faultID, actorID string) {
	t.Helper()
	topology := store.NewTopologyRepo()
	faults := store.NewFaultRepo()
	now := time.Now().UTC().Truncate(time.Microsecond)
	externalRef := "svc-" + faultID
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := topology.ImportSnapshot(ctx, tx, domain.TopologySnapshot{
			SchemaVersion: 1, Source: "incident-svc-test", ObservedAt: now,
			Nodes: []domain.ResourceNode{
				{ExternalRef: externalRef, Name: externalRef, Kind: domain.ResourceNodeKindBusinessSystem},
			},
		}); err != nil {
			return err
		}
		node, err := topology.GetByExternalRef(ctx, tx, "incident-svc-test", externalRef)
		if err != nil {
			return err
		}
		if err := faults.UpsertAutomatedImpact(ctx, tx, faultID, domain.FaultImpact{
			NodeID: node.ID, State: domain.ImpactStatePotential, Rationale: "test", Confidence: 0.8,
		}); err != nil {
			return err
		}
		_, err = faults.UpsertHumanImpact(ctx, tx, domain.FaultImpact{
			FaultID: faultID, NodeID: node.ID, State: domain.ImpactStateConfirmed,
			Rationale: "确认业务影响", Confidence: 1, DecisionOrigin: domain.DecisionOriginHuman,
			DecidedBy: actorID, DecidedAt: now,
		})
		return err
	})
	if err != nil {
		t.Fatalf("确认业务影响失败(fault=%s): %v", faultID, err)
	}
}

// fakeOutbox 记录 QueueIncidentNotification 调用，验证 Service 只在事务内 Schedule
// 通报意图、不做同步外部 IO（不变量 4）；测试无需真的接一个 scheduler。
type fakeOutbox struct {
	mu    sync.Mutex
	calls []domain.IncidentNotification
}

func (f *fakeOutbox) QueueIncidentNotification(_ context.Context, _ pgx.Tx, n domain.IncidentNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, n)
	return nil
}

// ScheduleRecoveryNudge/CancelRecoveryNudge 在这些测试里是无关紧要的旁路意图
// （任务 B 的调度细节由 internal/app 的集成测试覆盖），这里只需要满足接口、不
// 影响 Declare/Transition 的核心断言。
func (f *fakeOutbox) ScheduleRecoveryNudge(_ context.Context, _ pgx.Tx, _ string) error { return nil }
func (f *fakeOutbox) CancelRecoveryNudge(_ context.Context, _ pgx.Tx, _ string) error   { return nil }

func (f *fakeOutbox) eventCount(event domain.IncidentNotificationEvent) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Event == event {
			n++
		}
	}
	return n
}

func newTestService(st *store.Store, outbox *fakeOutbox) *Service {
	return NewService(st, store.NewFaultRepo(), store.NewIncidentRepo(), outbox)
}

func faultStatus(t *testing.T, ctx context.Context, st *store.Store, faultID string) domain.FaultStatus {
	t.Helper()
	faults := store.NewFaultRepo()
	var status domain.FaultStatus
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		f, err := faults.GetByID(ctx, tx, faultID)
		status = f.Status
		return err
	})
	if err != nil {
		t.Fatalf("查询 Fault 状态失败: %v", err)
	}
	return status
}

// TestService_Declare 覆盖场景 1：门槛拒绝、成功宣布的同事务副作用、重复宣布幂等、
// 跨 Incident 复用同一 Fault 被拒绝。
func TestService_Declare(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := mustUser(t, ctx, st, "declare-actor")
	outbox := &fakeOutbox{}
	svc := newTestService(st, outbox)
	who := Actor{UserID: actor.ID, Display: actor.DisplayName}

	// 1a：Fault 尚未 active（仍是 candidate）——拒绝。
	candidateFault := newFault(t, ctx, st, "declare-candidate", domain.FaultStatusCandidate)
	_, _, err := svc.Declare(ctx, DeclareCommand{
		FaultID: candidateFault.ID, Title: "候选期宣布", Severity: domain.SeverityCritical,
		CommunicationChannel: "lark_webhook", CommunicationTargetRef: "https://open.feishu.cn/a",
	}, who)
	if !errors.Is(err, ErrIncidentNotReady) {
		t.Fatalf("candidate Fault 应拒绝宣布, got %v", err)
	}

	// 1b：Fault active 但没有 confirmed 业务影响——拒绝。
	activeNoImpact := newFault(t, ctx, st, "declare-no-impact", domain.FaultStatusActive)
	_, _, err = svc.Declare(ctx, DeclareCommand{
		FaultID: activeNoImpact.ID, Title: "无确认影响宣布", Severity: domain.SeverityCritical,
		CommunicationChannel: "lark_webhook", CommunicationTargetRef: "https://open.feishu.cn/a",
	}, who)
	if !errors.Is(err, ErrIncidentNotReady) {
		t.Fatalf("无 confirmed 影响应拒绝宣布, got %v", err)
	}

	// 1c：门槛满足——宣布成功，primary link、通报意图与 Incident 同事务落地。
	faultA := newFault(t, ctx, st, "declare-ready", domain.FaultStatusActive)
	confirmBusinessImpact(t, ctx, st, faultA.ID, actor.ID)
	created, isNew, err := svc.Declare(ctx, DeclareCommand{
		FaultID: faultA.ID, Title: "共享缓存故障", Summary: "缓存节点大面积超时",
		Severity: domain.SeverityCritical, CommunicationChannel: "lark_webhook",
		CommunicationTargetRef: "https://open.feishu.cn/a",
	}, who)
	if err != nil || !isNew {
		t.Fatalf("满足门槛后应宣布成功: created=%v err=%v", isNew, err)
	}
	if created.Status != domain.IncidentStatusDeclared {
		t.Fatalf("新建 Incident 状态错误: %+v", created)
	}

	var link domain.IncidentFaultLink
	var criteria []domain.RecoveryCriterion
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		link, err = store.NewIncidentRepo().GetFaultLink(ctx, tx, created.ID, faultA.ID)
		if err != nil {
			return err
		}
		criteria, err = store.NewIncidentRepo().ListRecoveryCriteria(ctx, tx, created.ID)
		return err
	})
	if err != nil {
		t.Fatalf("查询 Declare 落库结果失败: %v", err)
	}
	if link.Relation != domain.IncidentFaultPrimary || link.Disposition != domain.IncidentFaultIncluded {
		t.Fatalf("primary link 未按预期建立: %+v", link)
	}
	if len(criteria) == 0 {
		t.Fatalf("恢复判据未初始化")
	}
	if outbox.eventCount(domain.IncidentNotificationDeclared) != 1 {
		t.Fatalf("Declared 通报意图应恰好入队一次, got %d", outbox.eventCount(domain.IncidentNotificationDeclared))
	}

	// 1d：重复宣布同一个 Fault——幂等返回既有 Incident，不产生新的通报意图。
	again, isNewAgain, err := svc.Declare(ctx, DeclareCommand{
		FaultID: faultA.ID, Title: "重复宣布", Severity: domain.SeverityCritical,
		CommunicationChannel: "lark_webhook", CommunicationTargetRef: "https://open.feishu.cn/a",
	}, who)
	if err != nil || isNewAgain || again.ID != created.ID {
		t.Fatalf("重复宣布应返回既有 Incident: again=%+v isNew=%v err=%v", again, isNewAgain, err)
	}
	if outbox.eventCount(domain.IncidentNotificationDeclared) != 1 {
		t.Fatalf("重复宣布不应产生新的通报意图, got %d", outbox.eventCount(domain.IncidentNotificationDeclared))
	}

	// 1e：同一个 Fault 不能被第二个 Incident 绑定——即便是通过 LinkFault 关联。
	faultB := newFault(t, ctx, st, "declare-second", domain.FaultStatusActive)
	confirmBusinessImpact(t, ctx, st, faultB.ID, actor.ID)
	incident2, _, err := svc.Declare(ctx, DeclareCommand{
		FaultID: faultB.ID, Title: "第二个 Incident", Severity: domain.SeverityWarning,
		CommunicationChannel: "lark_webhook", CommunicationTargetRef: "https://open.feishu.cn/b",
	}, who)
	if err != nil {
		t.Fatalf("宣布第二个 Incident 失败: %v", err)
	}
	_, _, err = svc.LinkFault(ctx, incident2.ID, faultA.ID, who)
	if !errors.Is(err, store.ErrIncidentForFaultExists) {
		t.Fatalf("跨 Incident 复用 Fault 应拒绝, got %v", err)
	}
}

// TestService_TransitionLifecycle 覆盖场景 2/3/4/5/6：以一个 Incident 的完整生命周期
// 驱动 monitoring/resolved 两道门槛、Fault 联动、primary 切换审计与非法迁移拒绝。
func TestService_TransitionLifecycle(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	actor := mustUser(t, ctx, st, "lifecycle-actor")
	outbox := &fakeOutbox{}
	svc := newTestService(st, outbox)
	who := Actor{UserID: actor.ID, Display: actor.DisplayName}
	incidents := store.NewIncidentRepo()

	// 宣布 Incident，primary Fault A（active）。
	faultA := newFault(t, ctx, st, "lifecycle-primary", domain.FaultStatusActive)
	confirmBusinessImpact(t, ctx, st, faultA.ID, actor.ID)
	inc, _, err := svc.Declare(ctx, DeclareCommand{
		FaultID: faultA.ID, Title: "多 Fault 生命周期", Severity: domain.SeverityCritical,
		CommunicationChannel: "lark_webhook", CommunicationTargetRef: "https://open.feishu.cn/c",
	}, who)
	if err != nil {
		t.Fatalf("宣布 Incident 失败: %v", err)
	}

	// 关联 supporting Fault B（candidate，保持默认 included disposition）与
	// Fault C（active，随后手动裁决为 excluded）。
	faultB := newFault(t, ctx, st, "lifecycle-candidate", domain.FaultStatusCandidate)
	if _, _, err := svc.LinkFault(ctx, inc.ID, faultB.ID, who); err != nil {
		t.Fatalf("关联 candidate Fault 失败: %v", err)
	}
	faultC := newFault(t, ctx, st, "lifecycle-excluded", domain.FaultStatusActive)
	if _, _, err := svc.LinkFault(ctx, inc.ID, faultC.ID, who); err != nil {
		t.Fatalf("关联 excluded Fault 失败: %v", err)
	}
	if _, err := svc.SetFaultDisposition(ctx, inc.ID, faultC.ID, domain.IncidentFaultExcluded, "独立因果链，不阻塞门槛", who); err != nil {
		t.Fatalf("裁决 Fault C 为 excluded 失败: %v", err)
	}

	// 场景 2：mitigation 判据未满足时拒绝推进 monitoring。
	if _, err := svc.Transition(ctx, inc.ID, domain.IncidentStatusMonitoring, who); !errors.Is(err, domain.ErrRecoveryGateNotSatisfied) {
		t.Fatalf("mitigation 判据未满足应拒绝, got %v", err)
	}
	now := time.Now().UTC()
	if _, err := svc.UpdateRecoveryCriterion(ctx, inc.ID, "application_impact_recovered", domain.RecoveryCriterionPassed, map[string]any{"note": "已恢复"}, now, who); err != nil {
		t.Fatalf("更新 mitigation 判据失败: %v", err)
	}
	if _, err := svc.UpdateRecoveryCriterion(ctx, inc.ID, "linked_faults_stabilized", domain.RecoveryCriterionPassed, map[string]any{"note": "已稳定"}, now, who); err != nil {
		t.Fatalf("更新 mitigation 判据失败: %v", err)
	}
	if _, err := svc.Transition(ctx, inc.ID, domain.IncidentStatusMonitoring, who); err != nil {
		t.Fatalf("满足 mitigation 判据后应能推进 monitoring: %v", err)
	}
	if outbox.eventCount(domain.IncidentNotificationMitigated) != 1 {
		t.Fatalf("Mitigated 通报意图应恰好入队一次, got %d", outbox.eventCount(domain.IncidentNotificationMitigated))
	}
	if status := faultStatus(t, ctx, st, faultA.ID); status != domain.FaultStatusMonitoring {
		t.Fatalf("included+active 的 primary Fault 应推进为 monitoring, got %s", status)
	}
	if status := faultStatus(t, ctx, st, faultB.ID); status != domain.FaultStatusCandidate {
		t.Fatalf("supporting+candidate 的 Fault 状态不应被改动, got %s", status)
	}
	if status := faultStatus(t, ctx, st, faultC.ID); status != domain.FaultStatusActive {
		t.Fatalf("excluded 的 Fault 状态不应被改动, got %s", status)
	}

	// 场景 4：切换 primary（此时 Incident 仍是 open 的 monitoring 状态）。faultB 当前
	// disposition 是 included（默认），满足 SetPrimaryFault 前置条件。
	if _, err := svc.SetPrimaryFault(ctx, inc.ID, faultB.ID, who); err != nil {
		t.Fatalf("切换 primary Fault 失败: %v", err)
	}
	var links []domain.IncidentFaultLink
	var timeline []domain.IncidentTimelineEntry
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		links, err = incidents.ListFaultLinks(ctx, tx, inc.ID)
		if err != nil {
			return err
		}
		timeline, err = incidents.ListTimeline(ctx, tx, inc.ID)
		return err
	}); err != nil {
		t.Fatalf("查询 primary 切换后状态失败: %v", err)
	}
	primaryCount, keepsA, keepsB := 0, false, false
	for _, l := range links {
		if l.FaultID == faultA.ID {
			keepsA = true
		}
		if l.FaultID == faultB.ID {
			keepsB = true
		}
		if l.Relation == domain.IncidentFaultPrimary {
			primaryCount++
			if l.FaultID != faultB.ID {
				t.Fatalf("primary 应切换为 faultB, got %s", l.FaultID)
			}
		}
	}
	if primaryCount != 1 || !keepsA || !keepsB {
		t.Fatalf("旧 Fault 的关联不应被删除: links=%+v", links)
	}
	foundSwitchEntry := false
	for _, e := range timeline {
		if e.Kind == domain.IncidentTimelineDecision && strings.Contains(e.Summary, "切换 Primary Fault") {
			foundSwitchEntry = true
		}
	}
	if !foundSwitchEntry {
		t.Fatalf("切换 primary 应追加时间线: %+v", timeline)
	}
	// 换回 faultA 作为 primary，便于后续按最初设计继续推进（primary 切换本身已验证）。
	if _, err := svc.SetPrimaryFault(ctx, inc.ID, faultA.ID, who); err != nil {
		t.Fatalf("换回 primary Fault 失败: %v", err)
	}

	// 场景 3a：存在非 done/canceled 任务时拒绝 resolved。先满足 resolution 判据，
	// 隔离出"仅任务未收口"这一种拒绝原因。
	if _, err := svc.UpdateRecoveryCriterion(ctx, inc.ID, "observation_window_complete", domain.RecoveryCriterionPassed, map[string]any{"note": "观察窗口已过"}, now, who); err != nil {
		t.Fatalf("更新 resolution 判据失败: %v", err)
	}
	task, err := svc.CreateTask(ctx, inc.ID, CreateTaskCommand{Title: "复盘会议纪要"}, who)
	if err != nil {
		t.Fatalf("创建响应任务失败: %v", err)
	}
	if _, err := svc.Transition(ctx, inc.ID, domain.IncidentStatusResolved, who); !errors.Is(err, domain.ErrIncidentTasksNotClosed) {
		t.Fatalf("存在未收口任务应拒绝 resolved, got %v", err)
	}
	if _, err := svc.UpdateTask(ctx, inc.ID, task.ID, domain.IncidentTaskDone, "", who); err != nil {
		t.Fatalf("收口任务失败: %v", err)
	}

	// 场景 3b：存在 included+active Fault 时拒绝 resolved。faultC 当前是 excluded，
	// 额外关联一个 included+active 的 faultD 制造这个拒绝条件。
	faultD := newFault(t, ctx, st, "lifecycle-blocks-resolve", domain.FaultStatusActive)
	if _, _, err := svc.LinkFault(ctx, inc.ID, faultD.ID, who); err != nil {
		t.Fatalf("关联 faultD 失败: %v", err)
	}
	if _, err := svc.Transition(ctx, inc.ID, domain.IncidentStatusResolved, who); !errors.Is(err, domain.ErrIncidentFaultsNotReady) {
		t.Fatalf("存在 included+active Fault 应拒绝 resolved, got %v", err)
	}

	// 场景 5：把 faultD 裁决为 excluded 后，它不再阻塞 resolved。
	if _, err := svc.SetFaultDisposition(ctx, inc.ID, faultD.ID, domain.IncidentFaultExcluded, "误关联，非本次事故根因", who); err != nil {
		t.Fatalf("裁决 faultD 为 excluded 失败: %v", err)
	}
	resolved, err := svc.Transition(ctx, inc.ID, domain.IncidentStatusResolved, who)
	if err != nil {
		t.Fatalf("满足全部门槛后应能 resolved: %v", err)
	}
	if resolved.Status != domain.IncidentStatusResolved {
		t.Fatalf("Incident 状态应为 resolved, got %s", resolved.Status)
	}
	if outbox.eventCount(domain.IncidentNotificationResolved) != 1 {
		t.Fatalf("Resolved 通报意图应恰好入队一次, got %d", outbox.eventCount(domain.IncidentNotificationResolved))
	}
	if status := faultStatus(t, ctx, st, faultA.ID); status != domain.FaultStatusResolved {
		t.Fatalf("included+monitoring 的 Fault 应推进为 resolved, got %s", status)
	}
	if status := faultStatus(t, ctx, st, faultB.ID); status != domain.FaultStatusCandidate {
		t.Fatalf("faultB 仍是 candidate，不应被 resolved 转移影响, got %s", status)
	}
	if status := faultStatus(t, ctx, st, faultC.ID); status != domain.FaultStatusActive {
		t.Fatalf("excluded 的 faultC 不应被推进, got %s", status)
	}
	if status := faultStatus(t, ctx, st, faultD.ID); status != domain.FaultStatusActive {
		t.Fatalf("excluded 的 faultD 不应被推进, got %s", status)
	}

	// 场景 6a：resolved 之后再迁移——非法迁移，拒绝。
	if _, err := svc.Transition(ctx, inc.ID, domain.IncidentStatusMonitoring, who); !errors.Is(err, domain.ErrInvalidIncidentTransition) {
		t.Fatalf("resolved 之后再迁移应拒绝, got %v", err)
	}

	// 场景 6b：对已关闭 Incident 关联 Fault——拒绝。
	faultE := newFault(t, ctx, st, "lifecycle-after-close", domain.FaultStatusActive)
	if _, _, err := svc.LinkFault(ctx, inc.ID, faultE.ID, who); !errors.Is(err, ErrIncidentClosed) {
		t.Fatalf("对已关闭 Incident 关联 Fault 应拒绝, got %v", err)
	}
}
