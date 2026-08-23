//go:build integration

// Service Observability 只读查询层在真实 PostgreSQL 上的语义。
//
// 这些行为必须用真库验证，无法用 fake 覆盖：递归 CTE 的边界与环路守卫、JSONB 参数化
// 匹配、UUID 数组参数、REPEATABLE READ + ReadOnly 事务，以及"所有查询都是只读、数据库
// 行数不变"这条硬约束。
//
//	go test -tags=integration ./internal/store -run ServiceObservability -count=1
package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// observabilityFixture 是一份最小但完整的两服务场景：
//
//   - primary：platform/infrastructure_capability 登记，declared 依赖一个基础设施
//     provider，另有一个 Profile root（part_of 子能力）；
//   - secondary：application/business_system 登记，有自己的 Alert 与 Fault；
//   - primary 上有一个 active Fault（带 confirmed + potential Impact、关联 Incident）
//     与一个 candidate Fault；
//   - 两个 Alert：一个带 focus 标签，一个不带。
type observabilityFixture struct {
	primaryServiceID   string
	primaryEntityID    string
	rootEntityID       string
	providerEntityID   string
	secondaryServiceID string
	secondaryEntityID  string
	activeFaultID      string
	candidateFaultID   string
	secondaryFaultID   string
	focusedAlertID     string
	plainAlertID       string
	secondaryAlertID   string
	incidentID         string
	impactedEntityID   string
	seededAt           time.Time
}

const observabilityFocusKey = "sandbox_id"
const observabilityFocusValue = "sbx_hp16c3"

func seedObservabilityFixture(t *testing.T, ctx context.Context, st *Store) observabilityFixture {
	t.Helper()
	team := mustTeam(t, ctx, st, "observability-owner")
	user := mustUser(t, ctx, st, "observability-commander")
	catalog := NewServiceCatalogRepo()
	topology := NewTopologyRepo()
	fixture := observabilityFixture{seededAt: time.Now().UTC()}

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		// provider 与 root 是普通 canonical 实体（root 是受管子能力，provider 是资源）。
		provider, err := topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "infrastructure/observability/postgresql", Layer: domain.EntityLayerInfrastructure,
			Kind: domain.ResourceNodeKindResource, Name: "PostgreSQL", Environment: "prod",
			OwnerTeamID: team.ID, Managed: true,
		})
		if err != nil {
			return err
		}
		fixture.providerEntityID = provider.ID

		root, err := topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/observability/orchestrator", Layer: domain.EntityLayerPlatform,
			Kind: domain.ResourceNodeKindInfrastructureCapability, Name: "Orchestrator",
			Environment: "prod", OwnerTeamID: team.ID, Managed: true,
		})
		if err != nil {
			return err
		}
		fixture.rootEntityID = root.ID

		primary, err := catalog.Create(ctx, tx, domain.ServiceRegistration{
			Entity: domain.ResourceNode{
				EntityKey: "platform/observability/primary", Layer: domain.EntityLayerPlatform,
				Kind: domain.ResourceNodeKindInfrastructureCapability, Name: "Primary Capability",
				Environment: "prod", OwnerTeamID: team.ID, Managed: true,
			},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier1, Enabled: true,
			Description: "共享基础设施能力",
			Dependencies: []domain.DependencyEdge{{
				ToNodeID: provider.ID, Relation: domain.DependencyRelationDependsOn,
				Criticality: domain.DependencyCriticalityCritical,
			}},
		})
		if err != nil {
			return err
		}
		fixture.primaryServiceID = primary.ID
		fixture.primaryEntityID = primary.Entity.ID

		// root 通过 declared part_of 挂在 service entity 下，模拟 Profile root 的可达性。
		if _, err := tx.Exec(ctx, `
			INSERT INTO dependency_edges (source, from_node_id, to_node_id, relation,
				criticality, evidence, observed_at, sync_run_id, origin)
			VALUES ('service_registry', $1, $2, 'part_of', 'high', '{}'::jsonb, now(), NULL, 'declared')
		`, primary.Entity.ID, root.ID); err != nil {
			return err
		}

		secondary, err := catalog.Create(ctx, tx, domain.ServiceRegistration{
			Entity: domain.ResourceNode{
				EntityKey: "application/observability/secondary", Layer: domain.EntityLayerApplication,
				Kind: domain.ResourceNodeKindBusinessSystem, Name: "Secondary System",
				Environment: "prod", OwnerTeamID: team.ID, Managed: true,
			},
			OwnerTeamID: team.ID, Tier: domain.ServiceTier2, Enabled: true,
		})
		if err != nil {
			return err
		}
		fixture.secondaryServiceID = secondary.ID
		fixture.secondaryEntityID = secondary.Entity.ID
		fixture.impactedEntityID = secondary.Entity.ID

		// 三条 Alert：primary 的聚焦 Alert、primary 的普通 Alert、secondary 的 Alert。
		fixture.focusedAlertID, err = insertObservabilityAlert(ctx, tx,
			"focused-fp", "PLACE 资源不足", "critical",
			map[string]string{observabilityFocusKey: observabilityFocusValue, "alertname": "PlacementFailed"})
		if err != nil {
			return err
		}
		fixture.plainAlertID, err = insertObservabilityAlert(ctx, tx,
			"plain-fp", "HugePages 使用率高", "warning",
			map[string]string{"alertname": "HugePagesHigh"})
		if err != nil {
			return err
		}
		fixture.secondaryAlertID, err = insertObservabilityAlert(ctx, tx,
			"secondary-fp", "结账错误率升高", "critical",
			map[string]string{observabilityFocusKey: observabilityFocusValue, "alertname": "CheckoutErrors"})
		if err != nil {
			return err
		}

		// primary 的 active Fault：两条证据都关联上，一条 confirmed Impact、一条 potential。
		fixture.activeFaultID, err = insertObservabilityFault(ctx, tx,
			"observability-active", "active", "critical", "capability", primary.Entity.ID, "容量失守")
		if err != nil {
			return err
		}
		if err := linkObservabilityAlert(ctx, tx, fixture.activeFaultID, fixture.focusedAlertID, "direct_impact"); err != nil {
			return err
		}
		if err := linkObservabilityAlert(ctx, tx, fixture.activeFaultID, fixture.plainAlertID, "supporting"); err != nil {
			return err
		}
		if err := insertObservabilityImpact(ctx, tx, fixture.activeFaultID, secondary.Entity.ID, "confirmed"); err != nil {
			return err
		}
		if err := insertObservabilityImpact(ctx, tx, fixture.activeFaultID, provider.ID, "potential"); err != nil {
			return err
		}

		// primary 的 candidate Fault：它在列表里可见，但不计入 active_faults。
		fixture.candidateFaultID, err = insertObservabilityFault(ctx, tx,
			"observability-candidate", "candidate", "warning", "instance", root.ID, "单实例异常")
		if err != nil {
			return err
		}

		// secondary 的 Fault 与证据：绝不能串进 primary 的边界。
		fixture.secondaryFaultID, err = insertObservabilityFault(ctx, tx,
			"observability-secondary", "active", "critical", "capability", secondary.Entity.ID, "结账故障")
		if err != nil {
			return err
		}
		if err := linkObservabilityAlert(ctx, tx, fixture.secondaryFaultID, fixture.secondaryAlertID, "direct_impact"); err != nil {
			return err
		}

		fixture.incidentID, err = insertObservabilityIncident(ctx, tx, user.ID, fixture.activeFaultID)
		return err
	})
	if err != nil {
		t.Fatalf("seed Service Observability fixture 失败: %v", err)
	}
	return fixture
}

func insertObservabilityAlert(ctx context.Context, tx pgx.Tx, fingerprint, title, severity string, labels map[string]string) (string, error) {
	labelsJSON, err := marshalJSONObject(labels)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO alerts (fingerprint, status, severity, title, source, labels,
			resource_ref, resolve_policy, upstream_fingerprint, source_state)
		VALUES ($1, 'firing', $2, $3, 'alertmanager', $4, '{}'::jsonb, 'source', $1, 'firing')
		RETURNING id::text
	`, fingerprint, severity, title, labelsJSON).Scan(&id)
	return id, err
}

func insertObservabilityFault(ctx context.Context, tx pgx.Tx, correlationKey, status, severity, scope, nodeID, title string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO faults (correlation_key, scope, status, severity, kind, title,
			resource_node_id, entity_ref, confidence, metadata,
			first_detected_at, last_evaluated_at)
		VALUES ($1, $2, $3, $4, 'capacity', $5, $6, '', 0.8, '{}'::jsonb, now(), now())
		RETURNING id::text
	`, correlationKey, scope, status, severity, title, nodeID).Scan(&id)
	return id, err
}

func linkObservabilityAlert(ctx context.Context, tx pgx.Tx, faultID, alertID, role string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fault_alert_links (fault_id, alert_id, role, rationale, confidence, decision_origin)
		VALUES ($1, $2, $3, '', 0.8, 'automation')
	`, faultID, alertID, role)
	return err
}

func insertObservabilityImpact(ctx context.Context, tx pgx.Tx, faultID, nodeID, state string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fault_impacts (fault_id, node_id, state, path, rationale, confidence, decision_origin)
		VALUES ($1, $2, $3, '[]'::jsonb, '', 0.7, 'automation')
	`, faultID, nodeID, state)
	return err
}

func insertObservabilityIncident(ctx context.Context, tx pgx.Tx, userID, faultID string) (string, error) {
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO incidents (status, severity, title, summary, declared_by,
			communication_channel, communication_target_ref, declared_at)
		VALUES ('mitigating', 'critical', '共享能力事故', '', $1, 'feishu', 'oc_fixture', now())
		RETURNING id::text
	`, userID).Scan(&id); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO incident_fault_links (incident_id, fault_id, relation, disposition)
		VALUES ($1, $2, 'primary', 'included')
	`, id, faultID)
	return id, err
}

// countObservabilityRows 汇总被测表的行数，用来证明只读查询确实没有副作用。
func countObservabilityRows(t *testing.T, ctx context.Context, st *Store) map[string]int {
	t.Helper()
	tables := []string{"resource_nodes", "dependency_edges", "service_registrations", "entity_aliases",
		"alerts", "faults", "fault_alert_links", "fault_impacts", "incidents", "incident_fault_links"}
	counts := map[string]int{}
	for _, table := range tables {
		var count int
		// 表名来自本函数内的固定字面量清单，不是外部输入。
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("统计 %s 行数失败: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func TestServiceObservabilityRegistrationListIsEnabledOnlyAndFiltered(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	// 禁用 secondary：它必须从目录消失，但直接 Get 仍然能读到（可见性口径与
	// GET /api/v1/services 一致）。
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE service_registrations SET enabled = false WHERE id = $1`, fixture.secondaryServiceID)
		return err
	}); err != nil {
		t.Fatalf("禁用 secondary 失败: %v", err)
	}

	var rows []ServiceIdentityRow
	var truncated bool
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, truncated, err = repo.ListServiceIdentities(ctx, tx, ServiceIdentityFilter{})
		return err
	}); err != nil {
		t.Fatalf("ListServiceIdentities 失败: %v", err)
	}
	if truncated {
		t.Fatal("两条以内不应触发截断")
	}
	if len(rows) != 1 || rows[0].ServiceID != fixture.primaryServiceID {
		t.Fatalf("目录应只含 enabled registration: %+v", rows)
	}
	if rows[0].Layer != domain.EntityLayerPlatform ||
		rows[0].Kind != domain.ResourceNodeKindInfrastructureCapability {
		t.Fatalf("platform/infrastructure_capability 分类必须原样读回: %+v", rows[0])
	}
	if rows[0].OwnerTeamName == "" || rows[0].Tier != domain.ServiceTier1 {
		t.Fatalf("目录行缺少责任与 tier: %+v", rows[0])
	}

	// 禁用的服务仍可直接读取。
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		row, err := repo.GetServiceIdentity(ctx, tx, fixture.secondaryServiceID)
		if err != nil {
			return err
		}
		if row.Enabled {
			t.Fatal("secondary 已被禁用")
		}
		return nil
	}); err != nil {
		t.Fatalf("GetServiceIdentity 应能读到禁用登记: %v", err)
	}

	// metadata 过滤与 LIKE 通配符转义：`%` 必须被当成字面量。
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		byEnv, _, err := repo.ListServiceIdentities(ctx, tx, ServiceIdentityFilter{Environment: "staging"})
		if err != nil {
			return err
		}
		if len(byEnv) != 0 {
			t.Fatalf("environment 过滤未生效: %+v", byEnv)
		}
		byQuery, _, err := repo.ListServiceIdentities(ctx, tx, ServiceIdentityFilter{Query: "%"})
		if err != nil {
			return err
		}
		if len(byQuery) != 0 {
			t.Fatal("`%` 必须被转义成字面量，不能匹配全部服务")
		}
		byTier, _, err := repo.ListServiceIdentities(ctx, tx, ServiceIdentityFilter{Tier: "tier-3"})
		if err != nil {
			return err
		}
		if len(byTier) != 0 {
			t.Fatalf("tier 过滤未生效: %+v", byTier)
		}
		return nil
	}); err != nil {
		t.Fatalf("过滤查询失败: %v", err)
	}
}

// keyset 分批遍历必须与单条 ListServiceIdentities 落在同一个候选口径上，并且逐批走完
// 不漏行不重复：目录摘要的分母就是这个完整集合。
func TestServiceObservabilityScanWalksCompleteCandidateSetByKeyset(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	var walked []string
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		var after ServiceIdentityScanCursor
		for {
			// 单批 1 行：强制走完整 keyset 链路，而不是一批取回全部。
			rows, err := repo.ScanServiceIdentities(ctx, tx, ServiceIdentityFilter{}, after, 1)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				walked = append(walked, row.ServiceID)
				after = ServiceIdentityScanCursor{
					Tier: string(row.Tier), Name: row.Name, ServiceID: row.ServiceID,
				}
			}
		}
	}); err != nil {
		t.Fatalf("ScanServiceIdentities 分批遍历失败: %v", err)
	}

	if len(walked) != 2 {
		t.Fatalf("完整候选集合应为 2 个 enabled 登记，实际 %d: %v", len(walked), walked)
	}
	if !containsID(walked, fixture.primaryServiceID) || !containsID(walked, fixture.secondaryServiceID) {
		t.Fatalf("keyset 遍历漏行: %v", walked)
	}
	seen := map[string]int{}
	for _, id := range walked {
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("keyset 遍历重复返回 %s", id)
		}
	}

	// 过滤口径与 List 完全一致：staging 环境没有登记，keyset 遍历也必须为空。
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		rows, err := repo.ScanServiceIdentities(ctx, tx,
			ServiceIdentityFilter{Environment: "staging"}, ServiceIdentityScanCursor{}, 10)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Fatalf("environment 过滤未生效: %+v", rows)
		}
		// 非法锚点不下传数据库：否则 uuid 转换失败会污染整个只读事务。
		_, err = repo.ScanServiceIdentities(ctx, tx, ServiceIdentityFilter{},
			ServiceIdentityScanCursor{ServiceID: "not-a-uuid"}, 10)
		if !errors.Is(err, ErrServiceObservabilityInvalidID) {
			t.Fatalf("非法锚点应返回 typed 错误，实际 %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("过滤/锚点校验失败: %v", err)
	}
}

func TestServiceObservabilityBoundaryTruncationIsExplicit(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	var boundary []string
	var truncated bool
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		var err error
		boundary, truncated, err = repo.ServiceBoundaryEntityIDs(ctx, tx, fixture.primaryEntityID,
			[]string{fixture.rootEntityID}, 1, 2)
		return err
	}); err != nil {
		t.Fatalf("读取实体边界失败: %v", err)
	}
	if !truncated {
		t.Fatal("fixture 边界超过 maxEntities=2 时必须标 truncated")
	}
	if len(boundary) != 2 {
		t.Fatalf("截断后应保留前 2 个实体，实际 %d: %v", len(boundary), boundary)
	}
}

func TestServiceObservabilityBoundaryIsolatesSecondService(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	var boundary []string
	var facts DomainFactsRows
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		var err error
		boundary, _, err = repo.ServiceBoundaryEntityIDs(ctx, tx, fixture.primaryEntityID,
			[]string{fixture.rootEntityID}, 1, 100)
		if err != nil {
			return err
		}
		facts, err = repo.ReadDomainFacts(ctx, tx, DomainFactsQuery{EntityIDs: boundary})
		return err
	}); err != nil {
		t.Fatalf("读取领域事实失败: %v", err)
	}

	// 边界 = service entity + Profile root + declared provider。
	for _, want := range []string{fixture.primaryEntityID, fixture.rootEntityID, fixture.providerEntityID} {
		if !containsID(boundary, want) {
			t.Fatalf("边界缺少 %s: %v", want, boundary)
		}
	}
	if containsID(boundary, fixture.secondaryEntityID) {
		t.Fatalf("第二个服务的实体不得进入 primary 边界: %v", boundary)
	}

	// 第二个服务的 Fault/Alert 绝不串入。
	for _, fault := range facts.Faults {
		if fault.FaultID == fixture.secondaryFaultID {
			t.Fatal("第二个服务的 Fault 串入 primary 边界")
		}
	}
	for _, alert := range facts.Alerts {
		if alert.AlertID == fixture.secondaryAlertID {
			t.Fatal("第二个服务的 Alert 串入 primary 边界")
		}
	}
	if len(facts.Faults) != 2 {
		t.Fatalf("primary 边界应有 active + candidate 两个未关闭 Fault: %+v", facts.Faults)
	}
	if len(facts.Alerts) != 2 {
		t.Fatalf("primary 边界应有两条 active Alert: %+v", facts.Alerts)
	}

	// 计数按领域词表的严格含义：candidate 不是 active。
	if facts.Counts.ActiveFaults != 1 {
		t.Fatalf("active_faults 只数 status='active'，实际 %d", facts.Counts.ActiveFaults)
	}
	if facts.Counts.ConfirmedImpacts != 1 {
		t.Fatalf("confirmed_impacts 应为 1（爆炸半径口径），实际 %d", facts.Counts.ConfirmedImpacts)
	}
	if facts.Counts.ActiveIncidents != 1 {
		t.Fatalf("active_incidents 应为 1，实际 %d", facts.Counts.ActiveIncidents)
	}

	// confirmed 与 potential 都要保留，不能只留一种。
	states := map[string]int{}
	for _, impact := range facts.Impacts {
		states[impact.State]++
	}
	if states["confirmed"] != 1 || states["potential"] != 1 {
		t.Fatalf("Impact 三态投影不完整: %+v", states)
	}
	if len(facts.Incidents) != 1 || facts.Incidents[0].IncidentID != fixture.incidentID ||
		facts.Incidents[0].Relation != "primary" {
		t.Fatalf("关联 Incident 投影不符: %+v", facts.Incidents)
	}
	if facts.LatestFactAt.IsZero() {
		t.Fatal("LatestFactAt 必须来自真实事实时间")
	}
}

func TestServiceObservabilityFocusOnlyNarrowsDomainRefs(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	var unfocused, focused, missFocus DomainFactsRows
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		boundary, _, err := repo.ServiceBoundaryEntityIDs(ctx, tx, fixture.primaryEntityID,
			[]string{fixture.rootEntityID}, 1, 100)
		if err != nil {
			return err
		}
		if unfocused, err = repo.ReadDomainFacts(ctx, tx, DomainFactsQuery{EntityIDs: boundary}); err != nil {
			return err
		}
		if focused, err = repo.ReadDomainFacts(ctx, tx, DomainFactsQuery{
			EntityIDs: boundary, FocusKey: observabilityFocusKey, FocusValue: observabilityFocusValue,
		}); err != nil {
			return err
		}
		missFocus, err = repo.ReadDomainFacts(ctx, tx, DomainFactsQuery{
			EntityIDs: boundary, FocusKey: observabilityFocusKey, FocusValue: "sbx_absent",
		})
		return err
	}); err != nil {
		t.Fatalf("focus 查询失败: %v", err)
	}

	if len(unfocused.Alerts) != 2 {
		t.Fatalf("无 focus 时应看到整个边界的 Alert: %+v", unfocused.Alerts)
	}
	if !focused.FocusApplied {
		t.Fatal("focus 生效必须显式可见")
	}
	if len(focused.Alerts) != 1 || focused.Alerts[0].AlertID != fixture.focusedAlertID {
		t.Fatalf("focus 应只保留带匹配标签的 Alert: %+v", focused.Alerts)
	}
	// focus 只收窄 domain refs：candidate Fault 没有匹配证据，因此被收窄掉；
	// 但计数仍按整个服务边界统计，不受 focus 影响。
	if len(focused.Faults) != 1 || focused.Faults[0].FaultID != fixture.activeFaultID {
		t.Fatalf("focus 应只保留有匹配证据的 Fault: %+v", focused.Faults)
	}
	if focused.Counts.ActiveFaults != unfocused.Counts.ActiveFaults {
		t.Fatalf("focus 不得改写服务级计数: %d vs %d",
			focused.Counts.ActiveFaults, unfocused.Counts.ActiveFaults)
	}
	// 查询成功但无结果是 Empty，不是失败，也绝不放宽回整个边界。
	if len(missFocus.Alerts) != 0 || len(missFocus.Faults) != 0 {
		t.Fatalf("focus 未命中必须返回空，不得回退到整个边界: %+v", missFocus)
	}
	if len(missFocus.Impacts) != 0 || len(missFocus.Incidents) != 0 {
		t.Fatalf("focus 未命中时不应有 Impact/Incident: %+v", missFocus)
	}
}

func TestServiceObservabilityDomainBoundaryIndependentOfTopology(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	// 模拟 topology 查询失败：用一个已取消的 context 让邻域查询报错，再在健康的
	// 事务里读 domain，证明 domain 的边界不依赖 topology 的返回或成功。
	failed, cancel := context.WithCancel(ctx)
	cancel()
	topologyErr := st.WithReadTx(failed, func(tx pgx.Tx) error {
		_, err := repo.ListCanonicalNeighborhood(failed, tx, NeighborhoodQuery{
			SeedEntityIDs: []string{fixture.primaryEntityID},
		})
		return err
	})
	if topologyErr == nil {
		t.Fatal("已取消的 context 应让 topology 查询失败")
	}

	var boundary []string
	var facts DomainFactsRows
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		var err error
		boundary, _, err = repo.ServiceBoundaryEntityIDs(ctx, tx, fixture.primaryEntityID,
			[]string{fixture.rootEntityID}, 1, 100)
		if err != nil {
			return err
		}
		facts, err = repo.ReadDomainFacts(ctx, tx, DomainFactsQuery{EntityIDs: boundary})
		return err
	}); err != nil {
		t.Fatalf("topology 失败后 domain 仍必须可读: %v", err)
	}
	if len(facts.Faults) == 0 {
		t.Fatal("topology 失败不得让 domain 变空")
	}
	if containsID(boundary, fixture.secondaryEntityID) {
		t.Fatal("topology 失败不得让 domain 边界放宽到第二个服务")
	}
}

func TestServiceObservabilityNeighborhoodBoundsAndDirection(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	var full, capped NeighborhoodRows
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		var err error
		full, err = repo.ListCanonicalNeighborhood(ctx, tx, NeighborhoodQuery{
			SeedEntityIDs: []string{fixture.primaryEntityID, fixture.rootEntityID},
			MaxDepth:      2, MaxNodes: 100, MaxRelations: 200,
		})
		if err != nil {
			return err
		}
		capped, err = repo.ListCanonicalNeighborhood(ctx, tx, NeighborhoodQuery{
			SeedEntityIDs: []string{fixture.primaryEntityID, fixture.rootEntityID},
			MaxDepth:      2, MaxNodes: 1, MaxRelations: 1,
		})
		return err
	}); err != nil {
		t.Fatalf("邻域查询失败: %v", err)
	}

	if len(full.Nodes) < 3 {
		t.Fatalf("邻域应至少含 service entity、root 与 provider: %+v", full.Nodes)
	}
	// 方向固定 consumer -> provider。
	foundProviderEdge := false
	for _, relation := range full.Relations {
		if relation.SourceEntityID == fixture.primaryEntityID &&
			relation.TargetEntityID == fixture.providerEntityID {
			foundProviderEdge = true
			if relation.Origin != "declared" {
				t.Fatalf("登记依赖必须是 declared，实际 %q", relation.Origin)
			}
			if relation.Criticality != string(domain.DependencyCriticalityCritical) {
				t.Fatalf("criticality 必须原样保留，实际 %q", relation.Criticality)
			}
			if relation.Stale {
				t.Fatal("declared 关系不因时间过期")
			}
			if relation.ObservedAt.IsZero() {
				t.Fatal("declared 关系必须保留 observed_at")
			}
		}
	}
	if !foundProviderEdge {
		t.Fatalf("缺少 service -> provider 的 declared 边: %+v", full.Relations)
	}
	// 邻域不得越过 declared 边界把第二个服务拉进来。
	for _, node := range full.Nodes {
		if node.EntityID == fixture.secondaryEntityID {
			t.Fatal("第二个服务不得出现在 primary 邻域")
		}
	}

	// 达到上限必须显式截断标记。
	if !capped.NodesTruncated {
		t.Fatal("MaxNodes=1 必须显式标记截断，不能静默丢弃")
	}
	if len(capped.Nodes) != 1 {
		t.Fatalf("截断后应恰好保留 1 个节点: %+v", capped.Nodes)
	}

	// 没有 seed 时返回空邻域，绝不退化成扫全库。
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		empty, err := repo.ListCanonicalNeighborhood(ctx, tx, NeighborhoodQuery{})
		if err != nil {
			return err
		}
		if len(empty.Nodes) != 0 || len(empty.Relations) != 0 {
			t.Fatalf("空 seed 必须返回空邻域: %+v", empty)
		}
		return nil
	}); err != nil {
		t.Fatalf("空 seed 查询失败: %v", err)
	}
}

func TestServiceObservabilityResolverSeparatesNotFoundFromInfrastructure(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		row, err := repo.ResolveServiceByID(ctx, tx, fixture.primaryServiceID)
		if err != nil {
			return err
		}
		if row.EntityID != fixture.primaryEntityID || row.Environment != "prod" || !row.Managed {
			t.Fatalf("service 解析结果不符: %+v", row)
		}
		// 真实存在但不属于该服务的 ID 必须 typed not found。
		if _, err := repo.ResolveServiceByID(ctx, tx, "99999999-9999-4999-8999-999999999999"); !errors.Is(err, ErrServiceObservabilityNotFound) {
			t.Fatalf("不存在的 service ID 应 typed not found，实际 %v", err)
		}
		// 符号 fixture ID 绝不被当成 entity_key 猜测。
		if _, err := repo.ResolveServiceByID(ctx, tx, "svc_e2b_fixture"); !errors.Is(err, ErrServiceObservabilityInvalidID) {
			t.Fatalf("符号 ID 应 typed 拒绝而不是 entity_key fallback，实际 %v", err)
		}
		if _, err := repo.ResolveEntityByID(ctx, tx, "platform/e2b/api"); !errors.Is(err, ErrServiceObservabilityInvalidID) {
			t.Fatalf("entity_key 形状的输入应 typed 拒绝，实际 %v", err)
		}
		// root 必须能从 service entity 经 declared 关系可达。
		reachable, err := repo.DeclaredReachableEntityIDs(ctx, tx, fixture.primaryEntityID, 8)
		if err != nil {
			return err
		}
		if _, ok := reachable[fixture.rootEntityID]; !ok {
			t.Fatalf("root 应经 declared part_of 可达: %v", reachable)
		}
		if _, ok := reachable[fixture.secondaryEntityID]; ok {
			t.Fatal("第二个服务不得出现在 declared 可达集合")
		}
		return nil
	}); err != nil {
		t.Fatalf("resolver 查询失败: %v", err)
	}

	// context 取消是基础设施错误，绝不能被折叠成 not found。
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := st.WithReadTx(canceled, func(tx pgx.Tx) error {
		_, resolveErr := repo.ResolveServiceByID(canceled, tx, fixture.primaryServiceID)
		return resolveErr
	})
	if err == nil {
		t.Fatal("已取消的 context 必须返回错误")
	}
	if errors.Is(err, ErrServiceObservabilityNotFound) {
		t.Fatalf("取消不得被折叠成 not found: %v", err)
	}
}

func TestServiceObservabilityQueriesAreReadOnly(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()
	before := countObservabilityRows(t, ctx, st)

	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := repo.ListServiceIdentities(ctx, tx, ServiceIdentityFilter{}); err != nil {
			return err
		}
		if _, err := repo.GetServiceIdentity(ctx, tx, fixture.primaryServiceID); err != nil {
			return err
		}
		if _, err := repo.ListDeclaredDependencies(ctx, tx, fixture.primaryEntityID, 100); err != nil {
			return err
		}
		if _, err := repo.ListCanonicalNeighborhood(ctx, tx, NeighborhoodQuery{
			SeedEntityIDs: []string{fixture.primaryEntityID}, MaxDepth: 2,
		}); err != nil {
			return err
		}
		boundary, _, err := repo.ServiceBoundaryEntityIDs(ctx, tx, fixture.primaryEntityID,
			[]string{fixture.rootEntityID}, 1, 100)
		if err != nil {
			return err
		}
		if _, err := repo.ReadDomainFacts(ctx, tx, DomainFactsQuery{
			EntityIDs: boundary, FocusKey: observabilityFocusKey, FocusValue: observabilityFocusValue,
		}); err != nil {
			return err
		}
		if _, err := repo.DeclaredReachableEntityIDs(ctx, tx, fixture.primaryEntityID, 8); err != nil {
			return err
		}
		if _, err := repo.ResolveEntityByID(ctx, tx, fixture.rootEntityID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("只读查询链失败: %v", err)
	}

	after := countObservabilityRows(t, ctx, st)
	for table, count := range before {
		if after[table] != count {
			t.Fatalf("%s 行数被改写: %d -> %d", table, count, after[table])
		}
	}

	// WithReadTx 必须真的是只读事务：写语句由数据库直接拒绝。
	writeErr := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE service_registrations SET description = 'tampered' WHERE id = $1`,
			fixture.primaryServiceID)
		return err
	})
	if writeErr == nil {
		t.Fatal("只读事务必须拒绝写语句")
	}
}

func containsID(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

type aliasQueryCounter struct {
	pgx.Tx
	queries *int
}

func (t *aliasQueryCounter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	*t.queries++
	return t.Tx.Query(ctx, sql, args...)
}

func insertObservabilityAlias(ctx context.Context, tx pgx.Tx, namespace, value, entityID string, enabled bool) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO entity_aliases (namespace, alias_value, entity_id, enabled)
		VALUES ($1, $2, $3::uuid, $4)
	`, namespace, value, entityID, enabled)
	return err
}

func TestServiceObservabilityAliasBatchLookupStates(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()
	topology := NewTopologyRepo()

	var conflictEntityID string
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		conflict, err := topology.CreateManagedEntity(ctx, tx, domain.ResourceNode{
			EntityKey: "platform/observability/conflict", Layer: domain.EntityLayerPlatform,
			Kind: domain.ResourceNodeKindInfrastructureCapability, Name: "Conflict Owner",
			Environment: "prod", OwnerTeamID: mustTeam(t, ctx, st, "alias-conflict-owner").ID, Managed: true,
		})
		if err != nil {
			return err
		}
		conflictEntityID = conflict.ID
		if err := insertObservabilityAlias(ctx, tx, "service", "resolved-api", fixture.primaryEntityID, true); err != nil {
			return err
		}
		if err := insertObservabilityAlias(ctx, tx, "service", "conflict-host", fixture.providerEntityID, true); err != nil {
			return err
		}
		return insertObservabilityAlias(ctx, tx, "service", "conflict-host", conflictEntityID, true)
	}); err != nil {
		t.Fatalf("seed alias fixture 失败: %v", err)
	}

	type wantState struct {
		count    int
		entityID string
	}
	cases := map[string]struct {
		namespace string
		value     string
		want      wantState
	}{
		"not-found": {namespace: "service", value: "missing-host", want: wantState{count: 0}},
		"resolved":  {namespace: "service", value: "resolved-api", want: wantState{count: 1, entityID: fixture.primaryEntityID}},
		"conflict":  {namespace: "service", value: "conflict-host", want: wantState{count: 2}},
	}

	var results []AliasLookupResult
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		keys := []AliasIdentityKey{
			{Namespace: "service", Value: "missing-host"},
			{Namespace: "service", Value: "resolved-api"},
			{Namespace: "service", Value: "conflict-host"},
			{Namespace: "service", Value: "resolved-api"},
		}
		var err error
		results, err = repo.LookupAliasesBatch(ctx, &aliasQueryCounter{Tx: tx, queries: new(int)}, keys)
		return err
	}); err != nil {
		t.Fatalf("LookupAliasesBatch 失败: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("duplicate input 应只返回 3 个稳定 entry，实际 %d: %+v", len(results), results)
	}
	byValue := map[string]AliasLookupResult{}
	for _, result := range results {
		byValue[result.Value] = result
	}
	for name, tc := range cases {
		result, ok := byValue[tc.value]
		if !ok {
			t.Fatalf("缺少 %s 结果", name)
		}
		if result.MatchCount != tc.want.count {
			t.Fatalf("%s MatchCount=%d want %d", name, result.MatchCount, tc.want.count)
		}
		if tc.want.count == 1 {
			if result.Match == nil || result.Match.EntityID != tc.want.entityID {
				t.Fatalf("%s 应返回 canonical metadata: %+v", name, result.Match)
			}
			if result.Match.EntityKey == "" || result.Match.Name == "" {
				t.Fatalf("%s metadata 不完整: %+v", name, result.Match)
			}
		}
		if tc.want.count != 1 && result.Match != nil {
			t.Fatalf("%s 不应返回 Match: %+v", name, result.Match)
		}
	}
}

func TestServiceObservabilityAliasBatchLookupEmptyAndBounds(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewServiceObservabilityRepo()

	var empty []AliasLookupResult
	queryCount := 0
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		counter := &aliasQueryCounter{Tx: tx, queries: &queryCount}
		var err error
		empty, err = repo.LookupAliasesBatch(ctx, counter, nil)
		return err
	}); err != nil {
		t.Fatalf("空输入查询失败: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空输入应返回空结果: %+v", empty)
	}
	if queryCount != 0 {
		t.Fatalf("空输入不应查库，实际 query count=%d", queryCount)
	}

	keys := make([]AliasIdentityKey, MaxAliasBatchIdentities+1)
	for i := range keys {
		keys[i] = AliasIdentityKey{Namespace: "service", Value: "too-many-" + string(rune('a'+i%26))}
	}
	err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		_, lookupErr := repo.LookupAliasesBatch(ctx, tx, keys)
		return lookupErr
	})
	if !errors.Is(err, ErrAliasBatchTooLarge) {
		t.Fatalf("超界应返回 ErrAliasBatchTooLarge，实际 %v", err)
	}
}

func TestServiceObservabilityAliasBatchLookupUsesSingleQuery(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return insertObservabilityAlias(ctx, tx, "service", "batch-a", fixture.rootEntityID, true)
	}); err != nil {
		t.Fatalf("seed alias 失败: %v", err)
	}

	before := countObservabilityRows(t, ctx, st)
	queryCount := 0
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		counter := &aliasQueryCounter{Tx: tx, queries: &queryCount}
		keys := []AliasIdentityKey{
			{Namespace: "service", Value: "batch-a"},
			{Namespace: "service", Value: "missing-b"},
			{Namespace: "service", Value: "missing-c"},
		}
		results, err := repo.LookupAliasesBatch(ctx, counter, keys)
		if err != nil {
			return err
		}
		if len(results) != 3 {
			t.Fatalf("结果数=%d want 3", len(results))
		}
		return nil
	}); err != nil {
		t.Fatalf("批量 alias 查询失败: %v", err)
	}
	if queryCount != 1 {
		t.Fatalf("批量 alias 查询应使用单条 SQL，实际 query count=%d", queryCount)
	}
	after := countObservabilityRows(t, ctx, st)
	for table, count := range before {
		if after[table] != count {
			t.Fatalf("%s 行数被改写: %d -> %d", table, count, after[table])
		}
	}
	if _, ok := before["entity_aliases"]; !ok {
		t.Fatal("entity_aliases 应纳入只读断言")
	}
}

func TestServiceObservabilityAliasBatchLookupDisabledClaimIgnored(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	fixture := seedObservabilityFixture(t, ctx, st)
	repo := NewServiceObservabilityRepo()

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := insertObservabilityAlias(ctx, tx, "service", "disabled-only", fixture.secondaryEntityID, false); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed disabled alias 失败: %v", err)
	}

	var result AliasLookupResult
	if err := st.WithReadTx(ctx, func(tx pgx.Tx) error {
		results, err := repo.LookupAliasesBatch(ctx, tx, []AliasIdentityKey{
			{Namespace: "service", Value: "disabled-only"},
		})
		if err != nil {
			return err
		}
		result = results[0]
		return nil
	}); err != nil {
		t.Fatalf("LookupAliasesBatch 失败: %v", err)
	}
	if result.MatchCount != 0 {
		t.Fatalf("disabled claim 不应计入，实际 MatchCount=%d", result.MatchCount)
	}
}
