package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

type TopologyRepo struct{}

func NewTopologyRepo() *TopologyRepo { return &TopologyRepo{} }

type TopologyImportResult struct {
	SyncRunID    string
	SnapshotHash string
	NodeIDs      map[string]string
	NodeCount    int
	EdgeCount    int
}

var (
	ErrResourceNodeNotFound           = errors.New("store: 资源节点不存在")
	ErrManagedEntityReadOnly          = errors.New("store: 自动发现实体不可通过登记接口修改")
	ErrManagedEntityIdentityImmutable = errors.New("store: canonical entity key 与 layer 创建后不可修改")
	ErrEntityLifecycleReasonRequired  = errors.New("store: 下线原因不能为空")
	ErrEntityLifecycleInvalid         = errors.New("store: 非法生命周期状态")
	ErrEntityLifecycleActorRequired   = errors.New("store: 下线操作人不能为空")
)

const resourceNodeColumns = `
		id, entity_key, layer, managed, source, external_ref, kind, name, provider, environment,
		COALESCE(owner_team_id::text, ''), labels, attributes,
	last_observed_at, created_at, updated_at,
	lifecycle_state, decommissioned_at, COALESCE(decommissioned_by::text, ''), COALESCE(decommission_reason, '')
`

// resourceNodeColumnsQualified 用于 JOIN 查询，避免 id/source 等列与 CTE 字段歧义。
const resourceNodeColumnsQualified = `
		n.id, n.entity_key, n.layer, n.managed, n.source, n.external_ref, n.kind, n.name, n.provider, n.environment,
	COALESCE(n.owner_team_id::text, ''), n.labels, n.attributes,
	n.last_observed_at, n.created_at, n.updated_at,
	n.lifecycle_state, n.decommissioned_at, COALESCE(n.decommissioned_by::text, ''), COALESCE(n.decommission_reason, '')
`

type topologyRowScanner interface {
	Scan(dest ...any) error
}

func scanResourceNode(row topologyRowScanner, extra ...any) (domain.ResourceNode, error) {
	var node domain.ResourceNode
	var labelsRaw, attributesRaw []byte
	dest := []any{
		&node.ID, &node.EntityKey, &node.Layer, &node.Managed,
		&node.Source, &node.ExternalRef, &node.Kind, &node.Name,
		&node.Provider, &node.Environment, &node.OwnerTeamID, &labelsRaw,
		&attributesRaw, &node.LastObservedAt, &node.CreatedAt, &node.UpdatedAt,
		&node.LifecycleState, &node.DecommissionedAt, &node.DecommissionedBy, &node.DecommissionReason,
	}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return domain.ResourceNode{}, err
	}
	if err := json.Unmarshal(labelsRaw, &node.Labels); err != nil {
		return domain.ResourceNode{}, fmt.Errorf("store: 反序列化资源 labels 失败: %w", err)
	}
	if err := json.Unmarshal(attributesRaw, &node.Attributes); err != nil {
		return domain.ResourceNode{}, fmt.Errorf("store: 反序列化资源 attributes 失败: %w", err)
	}
	return node, nil
}

// ImportSnapshot 在调用方事务内原子导入一次完整来源快照。只替换该来源的边集合；
// 未出现在新快照中的节点保留历史最后观测时间，便于 UI 显示拓扑过期而不是凭空消失。
func (r *TopologyRepo) ImportSnapshot(ctx context.Context, tx pgx.Tx, snapshot domain.TopologySnapshot) (TopologyImportResult, error) {
	if err := snapshot.Validate(); err != nil {
		return TopologyImportResult{}, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return TopologyImportResult{}, fmt.Errorf("store: 序列化拓扑快照失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	snapshotHash := hex.EncodeToString(sum[:])

	var runID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO topology_sync_runs (source, status, observed_at, snapshot_hash)
		VALUES ($1, 'running', $2, $3)
		RETURNING id
	`, snapshot.Source, snapshot.ObservedAt, snapshotHash).Scan(&runID); err != nil {
		return TopologyImportResult{}, fmt.Errorf("store: 创建拓扑同步记录失败: %w", err)
	}

	nodeIDs := make(map[string]string, len(snapshot.Nodes))
	for _, input := range snapshot.Nodes {
		labels, err := marshalJSONObject(input.Labels)
		if err != nil {
			return TopologyImportResult{}, fmt.Errorf("store: 序列化节点 %s labels 失败: %w", input.ExternalRef, err)
		}
		attributes, err := marshalJSONObject(input.Attributes)
		if err != nil {
			return TopologyImportResult{}, fmt.Errorf("store: 序列化节点 %s attributes 失败: %w", input.ExternalRef, err)
		}
		observedAt := input.LastObservedAt
		if observedAt.IsZero() {
			observedAt = snapshot.ObservedAt
		}
		environment := input.Environment
		if environment == "" {
			environment = snapshot.Environment
		}
		var id string
		if err := tx.QueryRow(ctx, `
				INSERT INTO resource_nodes (
					entity_key, layer, managed, source, external_ref, kind, name, provider, environment, owner_team_id,
					labels, attributes, last_observed_at, last_sync_run_id
				) VALUES ($1 || ':' || $2, CASE
					WHEN $3 = 'business_system' THEN 'application'
					WHEN $3 = 'infrastructure_capability' THEN 'platform'
					ELSE 'infrastructure' END, false,
					$1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8, $9, $10, $11)
			ON CONFLICT (source, external_ref) DO UPDATE SET
				kind = EXCLUDED.kind,
				name = EXCLUDED.name,
				provider = EXCLUDED.provider,
				environment = EXCLUDED.environment,
				owner_team_id = EXCLUDED.owner_team_id,
				labels = EXCLUDED.labels,
				attributes = EXCLUDED.attributes,
				last_observed_at = EXCLUDED.last_observed_at,
				last_sync_run_id = EXCLUDED.last_sync_run_id
			RETURNING id
		`, snapshot.Source, input.ExternalRef, input.Kind, input.Name, input.Provider,
			environment, input.OwnerTeamID, labels, attributes, observedAt, runID).Scan(&id); err != nil {
			return TopologyImportResult{}, fmt.Errorf("store: upsert 资源节点 %s 失败: %w", input.ExternalRef, err)
		}
		nodeIDs[input.ExternalRef] = id
	}

	for _, input := range snapshot.Edges {
		evidence, err := marshalJSONObject(input.Evidence)
		if err != nil {
			return TopologyImportResult{}, fmt.Errorf("store: 序列化拓扑边证据失败: %w", err)
		}
		observedAt := input.ObservedAt
		if observedAt.IsZero() {
			observedAt = snapshot.ObservedAt
		}
		criticality := input.Criticality
		if criticality == "" {
			criticality = domain.DependencyCriticalityMedium
		}
		// 跨源边：本次快照的 Nodes 里查不到 ExternalRef 时，回落调用方已给出的
		// NodeID（指向其他来源已登记的 canonical 实体，任务 8）。既有同源路径
		// nodeIDs 命中时行为不变。
		fromID, ok := nodeIDs[input.FromExternalRef]
		if !ok {
			fromID = input.FromNodeID
		}
		toID, ok := nodeIDs[input.ToExternalRef]
		if !ok {
			toID = input.ToNodeID
		}
		if fromID == "" || toID == "" {
			return TopologyImportResult{}, fmt.Errorf("store: 拓扑边 %s -> %s 缺少可解析节点", input.FromExternalRef, input.ToExternalRef)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dependency_edges (
				source, from_node_id, to_node_id, relation, criticality,
				evidence, observed_at, sync_run_id, no_recent_traffic
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (source, from_node_id, to_node_id, relation) DO UPDATE SET
				criticality = EXCLUDED.criticality,
				evidence = EXCLUDED.evidence,
				observed_at = EXCLUDED.observed_at,
				sync_run_id = EXCLUDED.sync_run_id,
				no_recent_traffic = EXCLUDED.no_recent_traffic
		`, snapshot.Source, fromID, toID,
			input.Relation, criticality, evidence, observedAt, runID, input.NoRecentTraffic); err != nil {
			return TopologyImportResult{}, fmt.Errorf("store: upsert 拓扑边 %s -> %s 失败: %w", input.FromExternalRef, input.ToExternalRef, err)
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM dependency_edges WHERE source = $1 AND sync_run_id <> $2`, snapshot.Source, runID); err != nil {
		return TopologyImportResult{}, fmt.Errorf("store: 清理旧拓扑边失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE topology_sync_runs
		SET status = 'completed', node_count = $2, edge_count = $3, finished_at = now()
		WHERE id = $1
	`, runID, len(snapshot.Nodes), len(snapshot.Edges)); err != nil {
		return TopologyImportResult{}, fmt.Errorf("store: 完成拓扑同步记录失败: %w", err)
	}

	return TopologyImportResult{
		SyncRunID:    runID,
		SnapshotHash: snapshotHash,
		NodeIDs:      nodeIDs,
		NodeCount:    len(snapshot.Nodes),
		EdgeCount:    len(snapshot.Edges),
	}, nil
}

func marshalJSONObject(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

func (r *TopologyRepo) GetByExternalRef(ctx context.Context, tx pgx.Tx, source, externalRef string) (domain.ResourceNode, error) {
	row := tx.QueryRow(ctx, `SELECT `+resourceNodeColumns+` FROM resource_nodes WHERE source = $1 AND external_ref = $2`, source, externalRef)
	node, err := scanResourceNode(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ResourceNode{}, ErrResourceNodeNotFound
	}
	if err != nil {
		return domain.ResourceNode{}, fmt.Errorf("store: 查询资源节点失败: %w", err)
	}
	return node, nil
}

func (r *TopologyRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.ResourceNode, error) {
	node, err := scanResourceNode(tx.QueryRow(ctx, `SELECT `+resourceNodeColumns+` FROM resource_nodes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return node, ErrResourceNodeNotFound
	}
	if err != nil {
		return node, fmt.Errorf("store: 按 ID 查询资源节点失败: %w", err)
	}
	return node, nil
}

func managedEntityKind(layer domain.EntityLayer) domain.ResourceNodeKind {
	if layer == domain.EntityLayerPlatform {
		return domain.ResourceNodeKindInfrastructureCapability
	}
	return domain.ResourceNodeKindResource
}

func validateManagedProvider(node domain.ResourceNode) error {
	if strings.TrimSpace(node.EntityKey) == "" || strings.TrimSpace(node.Name) == "" ||
		strings.TrimSpace(node.Environment) == "" || strings.TrimSpace(node.OwnerTeamID) == "" ||
		(node.Layer != domain.EntityLayerInfrastructure && node.Layer != domain.EntityLayerPlatform) {
		return fmt.Errorf("store: 受管实体字段非法")
	}
	return nil
}

// EntityRegistrySource 是受管基础设施/平台实体的唯一写入来源，用于隔离
// replaceEntityDependencies 只清理本实体自己声明的边（不变量 15）。导出给
// API 层在回显依赖时按同一 source 过滤（见 ListDependenciesForSource）。
const EntityRegistrySource = "entity_registry"

// CreateManagedEntity registers a canonical infrastructure/platform provider.
// Application entities must continue to use ServiceCatalogRepo so ownership,
// tier and aliases stay in one authority. aliases/dependencies 与
// ServiceCatalogRepo.Create 共用 replaceEntityAliases/replaceEntityDependencies，
// 只是依赖边的 source 换成 entity_registry，避免与应用服务的边互相清除。
func (r *TopologyRepo) CreateManagedEntity(ctx context.Context, tx pgx.Tx, node domain.ResourceNode, aliases []domain.EntityAlias, dependencies []domain.DependencyEdge) (domain.ResourceNode, error) {
	if err := validateManagedProvider(node); err != nil {
		return domain.ResourceNode{}, err
	}
	labels, err := marshalJSONObject(node.Labels)
	if err != nil {
		return domain.ResourceNode{}, err
	}
	attributes, err := marshalJSONObject(node.Attributes)
	if err != nil {
		return domain.ResourceNode{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO resource_nodes (
			entity_key, layer, managed, source, external_ref, kind, name, provider,
			environment, owner_team_id, labels, attributes, last_observed_at
		) VALUES ($1, $2, true, 'entity_registry', $1, $3, $4, $5, $6, $7, $8, $9, now())
		RETURNING id::text
	`, node.EntityKey, node.Layer, managedEntityKind(node.Layer), node.Name, node.Provider,
		node.Environment, node.OwnerTeamID, labels, attributes).Scan(&id)
	if err != nil {
		return domain.ResourceNode{}, fmt.Errorf("store: 创建受管实体失败: %w", err)
	}
	if err := replaceEntityAliases(ctx, tx, id, aliases); err != nil {
		return domain.ResourceNode{}, err
	}
	if err := replaceEntityDependencies(ctx, tx, EntityRegistrySource, id, node.Environment, dependencies); err != nil {
		return domain.ResourceNode{}, err
	}
	return r.GetByID(ctx, tx, id)
}

func (r *TopologyRepo) UpdateManagedEntity(ctx context.Context, tx pgx.Tx, id string, node domain.ResourceNode, aliases []domain.EntityAlias, dependencies []domain.DependencyEdge) (domain.ResourceNode, error) {
	if err := validateManagedProvider(node); err != nil {
		return domain.ResourceNode{}, err
	}
	current, err := r.GetByID(ctx, tx, id)
	if err != nil {
		return domain.ResourceNode{}, err
	}
	if !current.Managed || current.Source != "entity_registry" {
		return domain.ResourceNode{}, ErrManagedEntityReadOnly
	}
	if current.EntityKey != node.EntityKey || current.Layer != node.Layer {
		return domain.ResourceNode{}, ErrManagedEntityIdentityImmutable
	}
	labels, err := marshalJSONObject(node.Labels)
	if err != nil {
		return domain.ResourceNode{}, err
	}
	attributes, err := marshalJSONObject(node.Attributes)
	if err != nil {
		return domain.ResourceNode{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE resource_nodes SET name = $2, provider = $3, environment = $4,
			owner_team_id = $5, labels = $6, attributes = $7,
			last_observed_at = now()
		WHERE id = $1
	`, id, node.Name, node.Provider, node.Environment, node.OwnerTeamID, labels, attributes); err != nil {
		return domain.ResourceNode{}, fmt.Errorf("store: 更新受管实体失败: %w", err)
	}
	// nil 表示调用方未提供该集合，保持现状；非 nil（含空切片）才整体替换。
	if aliases != nil {
		if err := replaceEntityAliases(ctx, tx, id, aliases); err != nil {
			return domain.ResourceNode{}, err
		}
	}
	if dependencies != nil {
		if err := replaceEntityDependencies(ctx, tx, EntityRegistrySource, id, node.Environment, dependencies); err != nil {
			return domain.ResourceNode{}, err
		}
	}
	return r.GetByID(ctx, tx, id)
}

// SetEntityLifecycle 是下线 / 恢复的唯一写入入口（D2：不混进 UpdateManagedEntity）。
// 已处于目标状态则短路返回 changed=false，不重写审计列。下线写三列；恢复清空三列。
func (r *TopologyRepo) SetEntityLifecycle(ctx context.Context, tx pgx.Tx, id, actorID, reason string, state domain.EntityLifecycleState, at time.Time) (domain.ResourceNode, bool, error) {
	if !state.IsValid() {
		return domain.ResourceNode{}, false, ErrEntityLifecycleInvalid
	}
	current, err := r.GetByID(ctx, tx, id)
	if err != nil {
		return domain.ResourceNode{}, false, err
	}
	if current.LifecycleState == state {
		return current, false, nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	switch state {
	case domain.EntityLifecycleDecommissioned:
		if strings.TrimSpace(reason) == "" {
			return domain.ResourceNode{}, false, ErrEntityLifecycleReasonRequired
		}
		if strings.TrimSpace(actorID) == "" {
			return domain.ResourceNode{}, false, ErrEntityLifecycleActorRequired
		}
		// 条件 UPDATE 而不是无条件写：上面的读与这里的写之间没有行锁，两个并发下线
		// 会双双读到 active，后者把前者的操作人与原因覆盖掉，而审计正是 R1.2 的全部意义。
		// 带上状态条件后判定与写入在同一条语句里原子完成，抢输的一方 RowsAffected=0。
		tag, err := tx.Exec(ctx, `
			UPDATE resource_nodes
			SET lifecycle_state = 'decommissioned',
			    decommissioned_at = $2,
			    decommissioned_by = $3::uuid,
			    decommission_reason = $4
			WHERE id = $1 AND lifecycle_state <> 'decommissioned'
		`, id, at, actorID, strings.TrimSpace(reason))
		if err != nil {
			return domain.ResourceNode{}, false, fmt.Errorf("store: 下线实体失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			node, err := r.GetByID(ctx, tx, id)
			return node, false, err
		}
	case domain.EntityLifecycleActive:
		tag, err := tx.Exec(ctx, `
			UPDATE resource_nodes
			SET lifecycle_state = 'active',
			    decommissioned_at = NULL,
			    decommissioned_by = NULL,
			    decommission_reason = NULL
			WHERE id = $1 AND lifecycle_state <> 'active'
		`, id)
		if err != nil {
			return domain.ResourceNode{}, false, fmt.Errorf("store: 恢复实体失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			node, err := r.GetByID(ctx, tx, id)
			return node, false, err
		}
	}
	node, err := r.GetByID(ctx, tx, id)
	if err != nil {
		return domain.ResourceNode{}, false, err
	}
	return node, true, nil
}

// ListAliases 返回实体的全部 Entity Alias；entity_aliases 不区分创建来源，
// 受管实体与应用服务共用同一张表和同一套 ResolveAlias 语义。
func (r *TopologyRepo) ListAliases(ctx context.Context, tx pgx.Tx, entityID string) ([]domain.EntityAlias, error) {
	return listEntityAliases(ctx, tx, entityID)
}

// ResolveAlias 按 namespace+value 查找已启用的 canonical 实体。与
// ServiceCatalogRepo.ResolveAlias 同一条 SQL：命中 0 条表示未对齐，命中多条
// 是别名冲突，调用方不得任选其一。
func (r *TopologyRepo) ResolveAlias(ctx context.Context, tx pgx.Tx, namespace, value string) ([]domain.ResourceNode, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+resourceNodeColumnsQualified+`
		FROM entity_aliases a
		JOIN resource_nodes n ON n.id = a.entity_id
		WHERE a.namespace = $1 AND a.alias_value = $2 AND a.enabled
		ORDER BY n.entity_key
	`, namespace, value)
	if err != nil {
		return nil, fmt.Errorf("store: 解析 alias 失败: %w", err)
	}
	defer rows.Close()
	var nodes []domain.ResourceNode
	for rows.Next() {
		node, err := scanResourceNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []domain.ResourceNode{}
	}
	return nodes, nil
}

// EnsureAlias 给实体补一条 alias：已存在则保持 enabled，不覆盖其他实体上的同名
// alias（UNIQUE 是 namespace+value+entity_id，冲突留给治理队列而不是静默改归属）。
func (r *TopologyRepo) EnsureAlias(ctx context.Context, tx pgx.Tx, entityID, namespace, value string) error {
	if entityID == "" || strings.TrimSpace(namespace) == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("store: alias 字段非法")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO entity_aliases (namespace, alias_value, entity_id, enabled)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (namespace, alias_value, entity_id) DO UPDATE SET enabled = true
	`, namespace, value, entityID)
	if err != nil {
		return fmt.Errorf("store: 写入 alias 失败: %w", err)
	}
	return nil
}

// UnresolvedIdentity 是 unresolved_identities 的读模型（R1.2/R1.4）。
type UnresolvedIdentity struct {
	Namespace   string    `json:"namespace"`
	Value       string    `json:"value"`
	HostPort    string    `json:"host_port"`
	ServiceType string    `json:"service_type"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	SampleApp   string    `json:"sample_app"`
}

// UpsertUnresolvedIdentities 写入待对齐队列。first_seen 只在首次插入写，
// last_seen/host_port/service_type/sample_app 每次覆盖（任务 7 跨任务警告）。
func (r *TopologyRepo) UpsertUnresolvedIdentities(ctx context.Context, tx pgx.Tx, items []UnresolvedIdentity, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	for _, item := range items {
		if strings.TrimSpace(item.Namespace) == "" || strings.TrimSpace(item.Value) == "" {
			return fmt.Errorf("store: unresolved identity 字段非法")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO unresolved_identities (namespace, value, host_port, service_type, first_seen, last_seen, sample_app)
			VALUES ($1, $2, $3, $4, $5, $5, $6)
			ON CONFLICT (namespace, value) DO UPDATE SET
				host_port = EXCLUDED.host_port,
				service_type = EXCLUDED.service_type,
				last_seen = EXCLUDED.last_seen,
				sample_app = EXCLUDED.sample_app
		`, item.Namespace, item.Value, item.HostPort, item.ServiceType, now, item.SampleApp); err != nil {
			return fmt.Errorf("store: UPSERT unresolved_identities 失败: %w", err)
		}
	}
	return nil
}

// ListUnresolvedIdentities 按 last_seen 倒序分页；空集合返回 [] 而不是 nil（不变量 13）。
func (r *TopologyRepo) ListUnresolvedIdentities(ctx context.Context, tx pgx.Tx, limit, offset int) ([]UnresolvedIdentity, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := tx.Query(ctx, `
		SELECT namespace, value, COALESCE(host_port, ''), COALESCE(service_type, ''),
			first_seen, last_seen, COALESCE(sample_app, '')
		FROM unresolved_identities
		ORDER BY last_seen DESC, namespace, value
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 unresolved_identities 失败: %w", err)
	}
	defer rows.Close()
	items := make([]UnresolvedIdentity, 0)
	for rows.Next() {
		var item UnresolvedIdentity
		if err := rows.Scan(&item.Namespace, &item.Value, &item.HostPort, &item.ServiceType,
			&item.FirstSeen, &item.LastSeen, &item.SampleApp); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// RecordSyncFailure 在 fetch 失败、未进入 ImportSnapshot 时写一条 failed 同步记录，
// 让 SRE 能从 topology_sync_runs 看到退避原因（R2.9）。
func (r *TopologyRepo) RecordSyncFailure(ctx context.Context, tx pgx.Tx, source string, observedAt time.Time, errMsg string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("store: topology sync source 不能为空")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000]
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO topology_sync_runs (source, status, observed_at, snapshot_hash, error, finished_at)
		VALUES ($1, 'failed', $2, '', $3, now())
	`, source, observedAt, errMsg)
	if err != nil {
		return fmt.Errorf("store: 记录拓扑同步失败失败: %w", err)
	}
	return nil
}

// ListDependenciesForSource 返回以 entityID 为消费方、且写入来源等于 source 的依赖边，
// 用于按创建方（拓扑同步 / 应用服务登记 / 受管实体登记）分别回显各自声明的依赖，
// 不与其他来源的边混在一起（不变量 15 的 source 隔离）。
func (r *TopologyRepo) ListDependenciesForSource(ctx context.Context, tx pgx.Tx, source, entityID string) ([]domain.DependencyEdge, error) {
	return listEntityDependencies(ctx, tx, source, entityID)
}

func (r *TopologyRepo) ListNodes(ctx context.Context, tx pgx.Tx, kind domain.ResourceNodeKind, lifecycle domain.EntityLifecycleState, limit int) ([]domain.ResourceNode, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	query := `SELECT ` + resourceNodeColumns + ` FROM resource_nodes`
	args := []any{}
	var clauses []string
	if kind != "" {
		clauses = append(clauses, fmt.Sprintf("kind = $%d", len(args)+1))
		args = append(args, kind)
	}
	switch lifecycle {
	case "", domain.EntityLifecycleActive:
		clauses = append(clauses, "lifecycle_state = 'active'")
	case domain.EntityLifecycleDecommissioned:
		clauses = append(clauses, "lifecycle_state = 'decommissioned'")
	case domain.EntityLifecycleAll:
	default:
		return nil, fmt.Errorf("%w: %s", ErrEntityLifecycleInvalid, lifecycle)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += fmt.Sprintf(` ORDER BY CASE kind WHEN 'infrastructure_capability' THEN 1 WHEN 'business_system' THEN 2 ELSE 3 END, name LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询资源节点列表失败: %w", err)
	}
	defer rows.Close()
	var result []domain.ResourceNode
	for rows.Next() {
		item, err := scanResourceNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *TopologyRepo) ListEdgesForNode(ctx context.Context, tx pgx.Tx, nodeID string) ([]domain.DependencyEdge, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.id::text, e.source, e.from_node_id::text, e.to_node_id::text,
		       fn.external_ref, tn.external_ref, fn.name, tn.name, e.relation, e.criticality,
		       e.origin, e.evidence, e.observed_at
		FROM dependency_edges e
		JOIN resource_nodes fn ON fn.id = e.from_node_id
		JOIN resource_nodes tn ON tn.id = e.to_node_id
		WHERE e.from_node_id = $1 OR e.to_node_id = $1
		ORDER BY e.relation, fn.name, tn.name
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.DependencyEdge
	for rows.Next() {
		var edge domain.DependencyEdge
		var evidenceRaw []byte
		if err := rows.Scan(&edge.ID, &edge.Source, &edge.FromNodeID, &edge.ToNodeID,
			&edge.FromExternalRef, &edge.ToExternalRef, &edge.FromNodeName, &edge.ToNodeName,
			&edge.Relation, &edge.Criticality, &edge.Origin,
			&evidenceRaw, &edge.ObservedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(evidenceRaw, &edge.Evidence); err != nil {
			return nil, err
		}
		result = append(result, edge)
	}
	return result, rows.Err()
}

type InfrastructureOverview struct {
	ResourceCount           int       `json:"resource_count"`
	CapabilityCount         int       `json:"capability_count"`
	BusinessSystemCount     int       `json:"business_system_count"`
	ActiveFaultCount        int       `json:"active_fault_count"`
	CriticalFaultCount      int       `json:"critical_fault_count"`
	ConfirmedBusinessImpact int       `json:"confirmed_business_impact_count"`
	LastObservedAt          time.Time `json:"last_observed_at,omitempty"`
}

func (r *TopologyRepo) Overview(ctx context.Context, tx pgx.Tx) (InfrastructureOverview, error) {
	var overview InfrastructureOverview
	var lastObserved *time.Time
	err := tx.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE kind = 'resource'),
			count(*) FILTER (WHERE kind = 'infrastructure_capability'),
			count(*) FILTER (WHERE kind = 'business_system'),
			(SELECT count(*) FROM faults WHERE status IN ('candidate', 'active', 'monitoring')),
			(SELECT count(*) FROM faults WHERE status = 'active' AND severity = 'critical'),
			(SELECT count(DISTINCT i.node_id) FROM fault_impacts i
			 JOIN resource_nodes n ON n.id = i.node_id
			 JOIN faults f ON f.id = i.fault_id
			 WHERE i.state = 'confirmed' AND n.kind = 'business_system' AND n.lifecycle_state = 'active' AND f.status <> 'resolved'),
			max(last_observed_at)
		FROM resource_nodes
		WHERE lifecycle_state = 'active'
	`).Scan(&overview.ResourceCount, &overview.CapabilityCount, &overview.BusinessSystemCount,
		&overview.ActiveFaultCount, &overview.CriticalFaultCount, &overview.ConfirmedBusinessImpact,
		&lastObserved)
	if err != nil {
		return overview, fmt.Errorf("store: 查询基础设施总览失败: %w", err)
	}
	if lastObserved != nil {
		overview.LastObservedAt = *lastObserved
	}
	return overview, nil
}

// ListBlastRadius 从 provider 根节点沿 depends_on 反向遍历消费方。Path 是从根到
// 当前节点的 UUID 序列，CTE 用 path 去环；同一节点有多条路径时保留最短路径。
func (r *TopologyRepo) ListBlastRadius(ctx context.Context, tx pgx.Tx, source, rootExternalRef string, maxDepth int) ([]domain.BlastRadiusNode, error) {
	if maxDepth <= 0 {
		maxDepth = 8
	}
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE impacted AS (
			SELECT n.id, 0 AS depth, ARRAY[n.id]::uuid[] AS path
			FROM resource_nodes n
			WHERE n.source = $1 AND n.external_ref = $2

			UNION ALL

			SELECT nxt.id, i.depth + 1, i.path || nxt.id
			FROM impacted i
			JOIN resource_nodes cur
			  ON cur.id = i.id AND cur.lifecycle_state = 'active'
			JOIN dependency_edges e
			  ON e.source = $1
			 AND e.to_node_id = i.id
			 AND e.relation = 'depends_on'
			JOIN resource_nodes nxt
			  ON nxt.id = e.from_node_id AND nxt.lifecycle_state = 'active'
			WHERE i.depth < $3
			  AND NOT nxt.id = ANY(i.path)
		), shortest AS (
			SELECT DISTINCT ON (id) id, depth, path
			FROM impacted
			ORDER BY id, depth
		)
		SELECT `+resourceNodeColumnsQualified+`, s.depth,
		       ARRAY(SELECT p::text FROM unnest(s.path) AS p)
		FROM shortest s
		JOIN resource_nodes n ON n.id = s.id
		ORDER BY s.depth, n.name
	`, source, rootExternalRef, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("store: 查询爆炸半径失败: %w", err)
	}
	defer rows.Close()

	var result []domain.BlastRadiusNode
	for rows.Next() {
		var depth int
		var path []string
		node, err := scanResourceNode(rows, &depth, &path)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描爆炸半径失败: %w", err)
		}
		result = append(result, domain.BlastRadiusNode{Node: node, Depth: depth, Path: path})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历爆炸半径失败: %w", err)
	}
	if len(result) == 0 {
		return nil, ErrResourceNodeNotFound
	}
	return result, nil
}

func traversalLimits(maxDepth int, staleAfter time.Duration) (int, int64) {
	if maxDepth <= 0 || maxDepth > 8 {
		maxDepth = 8
	}
	if staleAfter <= 0 {
		staleAfter = 24 * time.Hour
	}
	return maxDepth, int64(staleAfter / time.Second)
}

// ListBlastRadiusByNodeID walks all declared and observed depends_on relations
// across source boundaries. Observed paths age, while declared configuration is
// valid until explicitly replaced.
func (r *TopologyRepo) ListBlastRadiusByNodeID(ctx context.Context, tx pgx.Tx, rootNodeID string, maxDepth int, staleAfter time.Duration) ([]domain.BlastRadiusNode, error) {
	maxDepth, staleSeconds := traversalLimits(maxDepth, staleAfter)
	return r.listCanonicalTraversal(ctx, tx, rootNodeID, maxDepth, staleSeconds, true)
}

// ListDependencyProvidersByNodeID follows consumer -> provider. It is used to
// find open upstream Faults that must be re-evaluated after application evidence
// or a service dependency declaration changes.
func (r *TopologyRepo) ListDependencyProvidersByNodeID(ctx context.Context, tx pgx.Tx, consumerNodeID string, maxDepth int, staleAfter time.Duration) ([]domain.BlastRadiusNode, error) {
	maxDepth, staleSeconds := traversalLimits(maxDepth, staleAfter)
	return r.listCanonicalTraversal(ctx, tx, consumerNodeID, maxDepth, staleSeconds, false)
}

func (r *TopologyRepo) listCanonicalTraversal(ctx context.Context, tx pgx.Tx, rootNodeID string, maxDepth int, staleSeconds int64, reverse bool) ([]domain.BlastRadiusNode, error) {
	edgeJoin := "e.to_node_id = i.id"
	nextNode := "e.from_node_id"
	if !reverse {
		edgeJoin = "e.from_node_id = i.id"
		nextNode = "e.to_node_id"
	}
	query := fmt.Sprintf(`
		WITH RECURSIVE reached AS (
			SELECT n.id, 0 AS depth, ARRAY[n.id]::uuid[] AS path, false AS stale
			FROM resource_nodes n WHERE n.id = $1

			UNION ALL

			SELECT nxt.id, i.depth + 1, i.path || nxt.id,
			       i.stale OR (e.origin = 'observed' AND e.observed_at < now() - ($3 * interval '1 second'))
			       OR e.no_recent_traffic
			FROM reached i
			JOIN resource_nodes cur ON cur.id = i.id AND cur.lifecycle_state = 'active'
			JOIN dependency_edges e ON %[1]s AND e.relation = 'depends_on'
			JOIN resource_nodes nxt ON nxt.id = %[2]s AND nxt.lifecycle_state = 'active'
			WHERE i.depth < $2 AND NOT nxt.id = ANY(i.path)
		), shortest AS (
			SELECT DISTINCT ON (id) id, depth, path, stale
			FROM reached ORDER BY id, depth, stale
		)
		SELECT `+resourceNodeColumnsQualified+`, s.depth,
		       ARRAY(SELECT p::text FROM unnest(s.path) AS p), s.stale
		FROM shortest s JOIN resource_nodes n ON n.id = s.id
		ORDER BY s.depth, n.name
	`, edgeJoin, nextNode)
	rows, err := tx.Query(ctx, query, rootNodeID, maxDepth, staleSeconds)
	if err != nil {
		return nil, fmt.Errorf("store: canonical relation 遍历失败: %w", err)
	}
	defer rows.Close()
	var result []domain.BlastRadiusNode
	for rows.Next() {
		var item domain.BlastRadiusNode
		node, err := scanResourceNode(rows, &item.Depth, &item.Path, &item.Stale)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描 canonical relation 路径失败: %w", err)
		}
		item.Node = node
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 读取 canonical relation 路径失败: %w", err)
	}
	if len(result) == 0 {
		return nil, ErrResourceNodeNotFound
	}
	return result, nil
}

// replaceEntityAliases 原子替换一个实体的全部 Entity Alias。entity_aliases 按
// entity_id 归属、不区分创建来源，ServiceCatalogRepo（应用服务）与 TopologyRepo
// （受管基础设施/平台实体）共用这一份实现，避免同一段 DELETE+INSERT 逻辑抄两份。
func replaceEntityAliases(ctx context.Context, tx pgx.Tx, entityID string, aliases []domain.EntityAlias) error {
	if _, err := tx.Exec(ctx, `DELETE FROM entity_aliases WHERE entity_id = $1`, entityID); err != nil {
		return fmt.Errorf("store: 清理 Entity Alias 失败: %w", err)
	}
	for _, alias := range aliases {
		if _, err := tx.Exec(ctx, `
			INSERT INTO entity_aliases (namespace, alias_value, entity_id, enabled)
			VALUES ($1, $2, $3, $4)
		`, strings.TrimSpace(alias.Namespace), strings.TrimSpace(alias.Value), entityID, alias.Enabled); err != nil {
			return fmt.Errorf("store: 保存 Entity Alias 失败: %w", err)
		}
	}
	return nil
}

func listEntityAliases(ctx context.Context, tx pgx.Tx, entityID string) ([]domain.EntityAlias, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, namespace, alias_value, entity_id::text, enabled, created_at, updated_at
		FROM entity_aliases WHERE entity_id = $1 ORDER BY namespace, alias_value
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	aliases := []domain.EntityAlias{}
	for rows.Next() {
		var alias domain.EntityAlias
		if err := rows.Scan(&alias.ID, &alias.Namespace, &alias.Value, &alias.EntityID,
			&alias.Enabled, &alias.CreatedAt, &alias.UpdatedAt); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	return aliases, rows.Err()
}

// replaceEntityDependencies 原子替换指定 source 下、以 entityID 为消费方（from）声明的
// 依赖边。source 隔离不同写入方的边集合（不变量 15）：service_registry 归应用服务、
// entity_registry 归受管实体、拓扑同步各用自己的 source，互相不清除对方写入的边；
// origin 固定 declared，与拓扑同步写入的 observed 边区分开。ServiceCatalogRepo 与
// TopologyRepo 共用这份实现，只是各自传入不同的 source。
func replaceEntityDependencies(ctx context.Context, tx pgx.Tx, source, entityID, environment string, dependencies []domain.DependencyEdge) error {
	if _, err := tx.Exec(ctx, `DELETE FROM dependency_edges WHERE source = $1 AND from_node_id = $2`, source, entityID); err != nil {
		return fmt.Errorf("store: 清理依赖失败: %w", err)
	}
	topology := NewTopologyRepo()
	for _, dependency := range dependencies {
		target, err := topology.GetByID(ctx, tx, dependency.ToNodeID)
		if errors.Is(err, ErrResourceNodeNotFound) {
			return fmt.Errorf("%w: 依赖目标实体不存在", ErrServiceDependencyInvalid)
		}
		if err != nil {
			return err
		}
		if target.ID == entityID {
			return fmt.Errorf("%w: 不能依赖自身", ErrServiceDependencyInvalid)
		}
		if target.Environment != "" && target.Environment != "global" && environment != "" && target.Environment != environment {
			return fmt.Errorf("%w: 与依赖实体环境不一致", ErrServiceDependencyInvalid)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dependency_edges (
				source, from_node_id, to_node_id, relation, criticality,
				evidence, observed_at, sync_run_id, origin
			) VALUES ($1, $2, $3, 'depends_on', $4, jsonb_build_object('authority', $1::text), now(), NULL, 'declared')
		`, source, entityID, target.ID, dependency.Criticality); err != nil {
			return fmt.Errorf("store: 保存依赖失败: %w", err)
		}
	}
	return nil
}

func listEntityDependencies(ctx context.Context, tx pgx.Tx, source, entityID string) ([]domain.DependencyEdge, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.id::text, e.source, e.from_node_id::text, e.to_node_id::text,
		       fn.external_ref, tn.external_ref, fn.name, tn.name, e.relation,
		       e.criticality, e.origin, e.evidence, e.observed_at
		FROM dependency_edges e
		JOIN resource_nodes fn ON fn.id = e.from_node_id
		JOIN resource_nodes tn ON tn.id = e.to_node_id
		WHERE e.source = $1 AND e.from_node_id = $2
		ORDER BY e.criticality DESC, tn.name
	`, source, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dependencies := []domain.DependencyEdge{}
	for rows.Next() {
		var edge domain.DependencyEdge
		var evidenceRaw []byte
		if err := rows.Scan(&edge.ID, &edge.Source, &edge.FromNodeID, &edge.ToNodeID,
			&edge.FromExternalRef, &edge.ToExternalRef, &edge.FromNodeName, &edge.ToNodeName,
			&edge.Relation, &edge.Criticality, &edge.Origin, &evidenceRaw, &edge.ObservedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(evidenceRaw, &edge.Evidence); err != nil {
			return nil, err
		}
		dependencies = append(dependencies, edge)
	}
	return dependencies, rows.Err()
}
