package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// 本文件是 Service Observability 的只读查询层。它只 SELECT：没有 INSERT/UPDATE/DELETE，
// 也没有任何领域状态推进。所有查询都在调用方给的 tx 内执行，参数全部经 pgx 占位符下传，
// 不做任何字符串拼 SQL。
//
// 三条硬约束：
//
//  1. 边界必须有界：邻域遍历有 max depth/nodes/relations，领域读取有 limit，达到上限
//     时显式返回 truncated 标记，不静默截断。
//  2. pgx 单连接事务不能在 Rows 仍打开时发下一条查询：所有二阶段查询都先完整扫描、
//     检查 rows.Err()、显式 Close，再发起下一条。
//  3. 领域边界只由 service entity、Profile roots 与 declared 依赖决定；任何一步失败都
//     宁可返回错误，也不放宽成"查全库"。

// ServiceObservabilityRepo 是 Service Observability 的只读仓储。
type ServiceObservabilityRepo struct{}

func NewServiceObservabilityRepo() *ServiceObservabilityRepo { return &ServiceObservabilityRepo{} }

// 遍历与结果集的硬上界。它们与既有 canonical 遍历上界（8 跳）保持一致。
const (
	// MaxObservabilityTraversalDepth 是任何邻域/边界遍历的硬上界。
	MaxObservabilityTraversalDepth = 8
	// DefaultNeighborhoodDepth 是 bounded service neighborhood 的默认跳数。
	// 页面要的是"服务邻域"，不是整张图。
	DefaultNeighborhoodDepth = 2
	// DefaultBoundaryDepth 是领域实体边界沿 declared 边扩展的默认跳数。
	DefaultBoundaryDepth = 1
	// MaxObservabilityRows 是单次列表查询的硬上界。
	MaxObservabilityRows = 500
	// DefaultObservedStaleAfter 与 impact projector 的口径一致。
	DefaultObservedStaleAfter = 24 * time.Hour
)

var (
	// ErrServiceObservabilityNotFound 表示该 service registration 不存在。
	ErrServiceObservabilityNotFound = errors.New("store: service observability 服务不存在")
	// ErrServiceObservabilityInvalidID 表示传入的 ID 连 UUID 形状都不满足。它与
	// "不存在"分开：调用方可以据此避免把明显非法的输入下传到数据库。
	ErrServiceObservabilityInvalidID = errors.New("store: service observability ID 格式非法")
)

// WithReadTx 在一个显式只读、REPEATABLE READ 的事务内执行 fn。
//
// REPEATABLE READ 让同一个 adapter 内的多条语句看到同一个快照，"每个 adapter 内部
// 保持一致读"因此是数据库层面的保证而不是约定。ReadOnly 让任何误加的写语句在数据库
// 层直接失败，而不是等 code review 发现。
func (s *Store) WithReadTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return fmt.Errorf("store: 开启只读事务失败: %w", err)
	}
	// 只读事务没有需要提交的副作用，统一回滚即可释放快照。
	defer func() { _ = tx.Rollback(ctx) }()
	return fn(tx)
}

// ServiceIdentityRow 是目录与总览共用的轻量身份行：不含 aliases、不含 dependencies。
type ServiceIdentityRow struct {
	ServiceID        string
	EntityID         string
	EntityKey        string
	Name             string
	Description      string
	Layer            domain.EntityLayer
	Kind             domain.ResourceNodeKind
	Environment      string
	OwnerTeamID      string
	OwnerTeamName    string
	Tier             domain.ServiceTier
	DashboardURL     string
	RunbookURL       string
	Enabled          bool
	EntityObservedAt time.Time
	UpdatedAt        time.Time
}

// ServiceIdentityFilter 是目录的 metadata 过滤条件，全部参数化。
type ServiceIdentityFilter struct {
	Environment string
	TeamID      string
	Tier        string
	Query       string
	Limit       int
}

const serviceIdentitySelect = `
	SELECT sr.id::text, n.id::text, n.entity_key, n.name, sr.description,
		n.layer, n.kind, n.environment, sr.owner_team_id::text, t.name, sr.tier,
		sr.dashboard_url, sr.runbook_url, sr.enabled, n.last_observed_at, sr.updated_at
	FROM service_registrations sr
	JOIN resource_nodes n ON n.id = sr.entity_id
	JOIN teams t ON t.id = sr.owner_team_id
`

func scanServiceIdentity(row topologyRowScanner) (ServiceIdentityRow, error) {
	var item ServiceIdentityRow
	err := row.Scan(&item.ServiceID, &item.EntityID, &item.EntityKey, &item.Name,
		&item.Description, &item.Layer, &item.Kind, &item.Environment, &item.OwnerTeamID,
		&item.OwnerTeamName, &item.Tier, &item.DashboardURL, &item.RunbookURL,
		&item.Enabled, &item.EntityObservedAt, &item.UpdatedAt)
	return item, err
}

// ListServiceIdentities 返回 enabled registration 的轻量身份。
//
// 它刻意不读 aliases/dependencies：目录有几百行，逐行再查两张表就是 N+1。metadata
// 过滤全部参数化下传；`q` 的 LIKE 通配符在 Go 侧转义，避免用户输入被当成模式。
//
// 第二个返回值为 true 表示还有更多候选行被 limit 截断（内部多取一行判断）。
func (r *ServiceObservabilityRepo) ListServiceIdentities(
	ctx context.Context, tx pgx.Tx, filter ServiceIdentityFilter,
) ([]ServiceIdentityRow, bool, error) {
	limit := clampObservabilityLimit(filter.Limit)
	rows, err := tx.Query(ctx, serviceIdentitySelect+`
		WHERE sr.enabled
			AND ($1::text = '' OR n.environment = $1::text)
			AND (NULLIF($2::text, '') IS NULL OR sr.owner_team_id = NULLIF($2::text, '')::uuid)
			AND ($3::text = '' OR sr.tier = $3::text)
			AND ($4::text = '' OR n.name ILIKE '%' || $4::text || '%' ESCAPE '\'
				OR n.entity_key ILIKE '%' || $4::text || '%' ESCAPE '\')
		ORDER BY sr.tier, n.name, sr.id
		LIMIT $5
	`, filter.Environment, filter.TeamID, filter.Tier,
		escapeLikeFragment(filter.Query), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: 查询服务身份列表失败: %w", err)
	}
	defer rows.Close()
	items := make([]ServiceIdentityRow, 0, limit)
	truncated := false
	for rows.Next() {
		item, scanErr := scanServiceIdentity(rows)
		if scanErr != nil {
			return nil, false, fmt.Errorf("store: 扫描服务身份失败: %w", scanErr)
		}
		if len(items) == limit {
			truncated = true
			break
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: 遍历服务身份失败: %w", err)
	}
	return items, truncated, nil
}

// GetServiceIdentity 按 registration ID 读取单个服务身份。
//
// 这里不加 enabled 过滤：可见性口径与既有 GET /api/v1/services 一致，禁用的登记仍然
// 存在，只是不进目录。
func (r *ServiceObservabilityRepo) GetServiceIdentity(
	ctx context.Context, tx pgx.Tx, serviceID string,
) (ServiceIdentityRow, error) {
	if !looksLikeUUID(serviceID) {
		// 明显非法的 ID 不下传到数据库：UUID 转换失败会污染整个只读事务。
		return ServiceIdentityRow{}, ErrServiceObservabilityInvalidID
	}
	item, err := scanServiceIdentity(tx.QueryRow(ctx, serviceIdentitySelect+` WHERE sr.id = $1::uuid`, serviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceIdentityRow{}, ErrServiceObservabilityNotFound
	}
	if err != nil {
		return ServiceIdentityRow{}, fmt.Errorf("store: 查询服务身份失败: %w", err)
	}
	return item, nil
}

// DeclaredEdgeRow 是一条 declared 依赖边。方向固定 from(消费者) -> to(提供者)。
type DeclaredEdgeRow struct {
	RelationID   string
	FromEntityID string
	ToEntityID   string
	ToEntityKey  string
	ToEntityName string
	ToLayer      domain.EntityLayer
	ToKind       string
	Relation     string
	Criticality  string
	ObservedAt   time.Time
}

// ListDeclaredDependencies 返回该实体登记的 declared 依赖（含目标实体的展示字段）。
//
// 只取 origin='declared'：declared 配置在被显式替换前一直有效，不因遥测缺失消失。
func (r *ServiceObservabilityRepo) ListDeclaredDependencies(
	ctx context.Context, tx pgx.Tx, entityID string, limit int,
) ([]DeclaredEdgeRow, error) {
	if !looksLikeUUID(entityID) {
		return nil, ErrServiceObservabilityInvalidID
	}
	rows, err := tx.Query(ctx, `
		SELECT e.id::text, e.from_node_id::text, e.to_node_id::text,
			tn.entity_key, tn.name, tn.layer, tn.kind, e.relation,
			e.criticality, e.observed_at
		FROM dependency_edges e
		JOIN resource_nodes tn ON tn.id = e.to_node_id
		WHERE e.from_node_id = $1::uuid AND e.origin = 'declared'
		ORDER BY tn.layer, tn.name, e.id
		LIMIT $2
	`, entityID, clampObservabilityLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 查询 declared 依赖失败: %w", err)
	}
	defer rows.Close()
	edges := []DeclaredEdgeRow{}
	for rows.Next() {
		var edge DeclaredEdgeRow
		if err := rows.Scan(&edge.RelationID, &edge.FromEntityID, &edge.ToEntityID,
			&edge.ToEntityKey, &edge.ToEntityName, &edge.ToLayer, &edge.ToKind,
			&edge.Relation, &edge.Criticality, &edge.ObservedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 declared 依赖失败: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 declared 依赖失败: %w", err)
	}
	return edges, nil
}

// NeighborhoodNodeRow 是邻域里的一个实体，Depth 是到最近 seed 的跳数。
type NeighborhoodNodeRow struct {
	EntityID    string
	EntityKey   string
	Name        string
	Layer       domain.EntityLayer
	Kind        string
	Environment string
	Depth       int
	ObservedAt  time.Time
}

// NeighborhoodRelationRow 是邻域里的一条真实依赖边。
type NeighborhoodRelationRow struct {
	RelationID     string
	SourceEntityID string
	TargetEntityID string
	Relation       string
	Origin         string
	Criticality    string
	ObservedAt     time.Time
	Stale          bool
}

// NeighborhoodRows 是一次 bounded 邻域读取的原始行。
type NeighborhoodRows struct {
	Nodes []NeighborhoodNodeRow
	// NodesTruncated / RelationsTruncated 为 true 表示达到上限而截断，调用方必须显式
	// 标记 partial，不能静默丢弃。
	NodesTruncated     bool
	Relations          []NeighborhoodRelationRow
	RelationsTruncated bool
}

// NeighborhoodQuery 是 bounded canonical neighborhood 的查询参数。
type NeighborhoodQuery struct {
	// SeedEntityIDs 是 service entity 与 Profile roots。为空直接返回空邻域，
	// 绝不退化成"扫全库"。
	SeedEntityIDs []string
	MaxDepth      int
	MaxNodes      int
	MaxRelations  int
	StaleAfter    time.Duration
}

// ListCanonicalNeighborhood 从 seed 实体出发，按 canonical 依赖边双向有界扩展。
//
// 双向是必须的：请求方向是 consumer -> provider，但一个服务既有它依赖的提供者，也有
// 依赖它的消费者，两边都属于"服务邻域"。路径数组做环路守卫，depth 做硬上界。
//
// 节点与关系分两条查询：先完整读完节点并 Close（pgx 单连接约束），再按已选中的节点集
// 读关系。这样关系一定落在已投影的节点之间，页面不会出现悬空的边。
func (r *ServiceObservabilityRepo) ListCanonicalNeighborhood(
	ctx context.Context, tx pgx.Tx, query NeighborhoodQuery,
) (NeighborhoodRows, error) {
	result := NeighborhoodRows{Nodes: []NeighborhoodNodeRow{}, Relations: []NeighborhoodRelationRow{}}
	seeds := normalizeUUIDSet(query.SeedEntityIDs)
	if len(seeds) == 0 {
		return result, nil
	}
	depth := clampTraversalDepth(query.MaxDepth, DefaultNeighborhoodDepth)
	maxNodes := clampObservabilityLimit(query.MaxNodes)
	maxRelations := clampObservabilityLimit(query.MaxRelations)
	staleSeconds := int64(observedStaleAfter(query.StaleAfter) / time.Second)

	nodeRows, err := tx.Query(ctx, `
		WITH RECURSIVE reached AS (
			SELECT n.id, 0 AS depth, ARRAY[n.id]::uuid[] AS path
			FROM resource_nodes n WHERE n.id = ANY($1::uuid[])

			UNION ALL

			SELECT nxt.id, r.depth + 1, r.path || nxt.id
			FROM reached r
			JOIN LATERAL (
				SELECT e.to_node_id AS id FROM dependency_edges e WHERE e.from_node_id = r.id
				UNION
				SELECT e.from_node_id AS id FROM dependency_edges e WHERE e.to_node_id = r.id
			) nxt ON true
			WHERE r.depth < $2 AND NOT nxt.id = ANY(r.path)
		), shortest AS (
			SELECT DISTINCT ON (id) id, depth FROM reached ORDER BY id, depth
		)
		SELECT n.id::text, n.entity_key, n.name, n.layer, n.kind, n.environment,
			s.depth, n.last_observed_at
		FROM shortest s JOIN resource_nodes n ON n.id = s.id
		ORDER BY s.depth, n.layer, n.name, n.id
		LIMIT $3
	`, seeds, depth, maxNodes+1)
	if err != nil {
		return result, fmt.Errorf("store: 查询 canonical 邻域实体失败: %w", err)
	}
	for nodeRows.Next() {
		var node NeighborhoodNodeRow
		if scanErr := nodeRows.Scan(&node.EntityID, &node.EntityKey, &node.Name, &node.Layer,
			&node.Kind, &node.Environment, &node.Depth, &node.ObservedAt); scanErr != nil {
			nodeRows.Close()
			return result, fmt.Errorf("store: 扫描 canonical 邻域实体失败: %w", scanErr)
		}
		if len(result.Nodes) == maxNodes {
			result.NodesTruncated = true
			break
		}
		result.Nodes = append(result.Nodes, node)
	}
	if err := nodeRows.Err(); err != nil {
		nodeRows.Close()
		return result, fmt.Errorf("store: 遍历 canonical 邻域实体失败: %w", err)
	}
	// 必须先释放这一批 Rows 再发下一条查询，否则同一连接会报 conn busy。
	nodeRows.Close()

	nodeIDs := make([]string, 0, len(result.Nodes))
	for _, node := range result.Nodes {
		nodeIDs = append(nodeIDs, node.EntityID)
	}
	if len(nodeIDs) == 0 {
		return result, nil
	}

	relationRows, err := tx.Query(ctx, `
		SELECT e.id::text, e.from_node_id::text, e.to_node_id::text, e.relation,
			e.origin, e.criticality, e.observed_at,
			(e.origin = 'observed' AND e.observed_at < now() - ($2 * interval '1 second')) AS stale
		FROM dependency_edges e
		WHERE e.from_node_id = ANY($1::uuid[]) AND e.to_node_id = ANY($1::uuid[])
		ORDER BY e.from_node_id, e.to_node_id, e.relation, e.origin, e.id
		LIMIT $3
	`, nodeIDs, staleSeconds, maxRelations+1)
	if err != nil {
		return result, fmt.Errorf("store: 查询 canonical 邻域关系失败: %w", err)
	}
	defer relationRows.Close()
	for relationRows.Next() {
		var relation NeighborhoodRelationRow
		if scanErr := relationRows.Scan(&relation.RelationID, &relation.SourceEntityID,
			&relation.TargetEntityID, &relation.Relation, &relation.Origin,
			&relation.Criticality, &relation.ObservedAt, &relation.Stale); scanErr != nil {
			return result, fmt.Errorf("store: 扫描 canonical 邻域关系失败: %w", scanErr)
		}
		if len(result.Relations) == maxRelations {
			result.RelationsTruncated = true
			break
		}
		result.Relations = append(result.Relations, relation)
	}
	if err := relationRows.Err(); err != nil {
		return result, fmt.Errorf("store: 遍历 canonical 邻域关系失败: %w", err)
	}
	return result, nil
}

// ServiceBoundaryEntityIDs 计算领域读取使用的稳定实体边界。
//
// 边界 = service entity ∪ Profile roots ∪ 沿 declared 边有界可达的提供者。它完全不
// 依赖 topology adapter 的返回：topology 失败时 domain 仍按同一边界返回事实。
// seed 为空时返回空集合，绝不放宽成全库。
//
// 第二个返回值为 true 表示还有更多实体被 maxEntities 截断（内部多取一行判断）。
func (r *ServiceObservabilityRepo) ServiceBoundaryEntityIDs(
	ctx context.Context, tx pgx.Tx, serviceEntityID string, rootEntityIDs []string,
	maxDepth, maxEntities int,
) ([]string, bool, error) {
	seeds := normalizeUUIDSet(append([]string{serviceEntityID}, rootEntityIDs...))
	if len(seeds) == 0 {
		return []string{}, false, nil
	}
	depth := clampTraversalDepth(maxDepth, DefaultBoundaryDepth)
	limit := clampObservabilityLimit(maxEntities)
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE reached AS (
			SELECT n.id, 0 AS depth, ARRAY[n.id]::uuid[] AS path
			FROM resource_nodes n WHERE n.id = ANY($1::uuid[])

			UNION ALL

			SELECT e.to_node_id, r.depth + 1, r.path || e.to_node_id
			FROM reached r
			JOIN dependency_edges e ON e.from_node_id = r.id AND e.origin = 'declared'
			WHERE r.depth < $2 AND NOT e.to_node_id = ANY(r.path)
		), shortest AS (
			SELECT DISTINCT ON (id) id, depth FROM reached ORDER BY id, depth
		)
		SELECT s.id::text
		FROM shortest s JOIN resource_nodes n ON n.id = s.id
		ORDER BY s.depth, n.name, s.id
		LIMIT $3
	`, seeds, depth, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: 计算服务实体边界失败: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	truncated := false
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("store: 扫描服务实体边界失败: %w", err)
		}
		if len(ids) == limit {
			truncated = true
			break
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: 遍历服务实体边界失败: %w", err)
	}
	sort.Strings(ids)
	return ids, truncated, nil
}

// DeclaredReachableEntityIDs 返回从 service entity 出发沿 declared depends_on/part_of
// 有界可达的实体集合（不含自身）。Profile Registry 用它校验 root 可达性。
func (r *ServiceObservabilityRepo) DeclaredReachableEntityIDs(
	ctx context.Context, tx pgx.Tx, serviceEntityID string, maxDepth int,
) (map[string]struct{}, error) {
	if !looksLikeUUID(serviceEntityID) {
		return nil, ErrServiceObservabilityInvalidID
	}
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE reached AS (
			SELECT n.id, 0 AS depth, ARRAY[n.id]::uuid[] AS path
			FROM resource_nodes n WHERE n.id = $1::uuid

			UNION ALL

			SELECT e.to_node_id, r.depth + 1, r.path || e.to_node_id
			FROM reached r
			JOIN dependency_edges e ON e.from_node_id = r.id
				AND e.origin = 'declared'
				AND e.relation IN ('depends_on', 'part_of')
			WHERE r.depth < $2 AND NOT e.to_node_id = ANY(r.path)
		)
		SELECT DISTINCT id::text FROM reached WHERE depth > 0
	`, serviceEntityID, clampTraversalDepth(maxDepth, MaxObservabilityTraversalDepth))
	if err != nil {
		return nil, fmt.Errorf("store: 遍历 declared 可达实体失败: %w", err)
	}
	defer rows.Close()
	reachable := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: 扫描 declared 可达实体失败: %w", err)
		}
		reachable[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 读取 declared 可达实体失败: %w", err)
	}
	return reachable, nil
}

// EntityResolutionRow 是 Profile root entity 的解析结果。
type EntityResolutionRow struct {
	EntityID    string
	Environment string
	Managed     bool
	Layer       domain.EntityLayer
	Kind        domain.ResourceNodeKind
}

// ResolveEntityByID 按真实 resource_nodes.id 解析实体。找不到返回
// ErrServiceObservabilityNotFound；绝不做 entity_key fallback。
func (r *ServiceObservabilityRepo) ResolveEntityByID(
	ctx context.Context, tx pgx.Tx, entityID string,
) (EntityResolutionRow, error) {
	if !looksLikeUUID(entityID) {
		return EntityResolutionRow{}, ErrServiceObservabilityInvalidID
	}
	var row EntityResolutionRow
	err := tx.QueryRow(ctx, `
		SELECT id::text, environment, managed, layer, kind
		FROM resource_nodes WHERE id = $1::uuid
	`, entityID).Scan(&row.EntityID, &row.Environment, &row.Managed, &row.Layer, &row.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return EntityResolutionRow{}, ErrServiceObservabilityNotFound
	}
	if err != nil {
		return EntityResolutionRow{}, fmt.Errorf("store: 解析实体失败: %w", err)
	}
	return row, nil
}

// ServiceResolutionRow 是 Profile service_ref.service_id 的解析结果。
type ServiceResolutionRow struct {
	ServiceID   string
	EntityID    string
	Environment string
	Managed     bool
	Enabled     bool
}

// ResolveServiceByID 按真实 service_registrations.id 解析服务及其 canonical entity。
func (r *ServiceObservabilityRepo) ResolveServiceByID(
	ctx context.Context, tx pgx.Tx, serviceID string,
) (ServiceResolutionRow, error) {
	if !looksLikeUUID(serviceID) {
		return ServiceResolutionRow{}, ErrServiceObservabilityInvalidID
	}
	var row ServiceResolutionRow
	err := tx.QueryRow(ctx, `
		SELECT sr.id::text, n.id::text, n.environment, n.managed, sr.enabled
		FROM service_registrations sr
		JOIN resource_nodes n ON n.id = sr.entity_id
		WHERE sr.id = $1::uuid
	`, serviceID).Scan(&row.ServiceID, &row.EntityID, &row.Environment, &row.Managed, &row.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceResolutionRow{}, ErrServiceObservabilityNotFound
	}
	if err != nil {
		return ServiceResolutionRow{}, fmt.Errorf("store: 解析服务登记失败: %w", err)
	}
	return row, nil
}

// DomainFactsQuery 是领域事实读取参数。
type DomainFactsQuery struct {
	// EntityIDs 是调用方算好的稳定边界。为空时不查任何领域对象：宁可返回空，
	// 也不放宽到全库。
	EntityIDs []string
	// FocusKey/FocusValue 非空时对 alerts 的 labels/resource_ref 做参数化 JSONB 匹配。
	// key 必须已由调用方按已发布 Profile 的 focus_kinds 校验过。
	FocusKey   string
	FocusValue string
	Limit      int
}

// DomainAlertRow 是一条 active Alert。
//
// 三个状态轴分开保留：Alertmanager 写 SourceState/SuppressionState，用户写
// ResponseState。UpstreamFingerprint 非空表示该 Alert 由 Alertmanager 权威同步而来，
// 此时 SourceState 才是权威状态；本地来源只能看 Status。
type DomainAlertRow struct {
	AlertID             string
	Title               string
	Status              string
	UpstreamFingerprint string
	SourceState         string
	ResponseState       string
	SuppressionState    string
	Severity            string
	StartsAt            time.Time
	UpdatedAt           time.Time
}

// DomainFaultRow 是一条未关闭 Fault 及其 impact 计数。
type DomainFaultRow struct {
	FaultID              string
	Title                string
	Status               string
	Severity             string
	Kind                 string
	Scope                string
	EntityID             string
	EntityName           string
	OwnerTeamName        string
	FirstDetectedAt      time.Time
	UpdatedAt            time.Time
	ConfirmedImpactCount int
	PotentialImpactCount int
}

// DomainImpactRow 是一条 FaultImpact。
type DomainImpactRow struct {
	FaultID    string
	EntityID   string
	EntityName string
	Layer      domain.EntityLayer
	State      string
	UpdatedAt  time.Time
}

// DomainIncidentRow 是一个关联 Incident。
type DomainIncidentRow struct {
	IncidentID string
	Number     int64
	Title      string
	Status     string
	Severity   string
	Relation   string
	UpdatedAt  time.Time
}

// DomainCountRow 是服务边界级的领域计数。
//
// 三个计数使用领域词表的严格含义：
//   - ActiveFaults 只数 status='active'；candidate 仍在列表里可见，但它不是 active。
//   - ConfirmedImpacts 数边界内 active Fault 关联的 distinct confirmed FaultImpact
//     （即 (fault_id, node_id) 组合），这是爆炸半径口径，不要求服务自身是 Impact target。
//   - ActiveIncidents 只数 included 关联且未 resolved 的 Incident。
type DomainCountRow struct {
	ActiveFaults     int
	ConfirmedImpacts int
	ActiveIncidents  int
}

// DomainFactsRows 是一次领域读取的原始行集合。
type DomainFactsRows struct {
	Alerts    []DomainAlertRow
	Faults    []DomainFaultRow
	Impacts   []DomainImpactRow
	Incidents []DomainIncidentRow
	Counts    DomainCountRow
	// FocusApplied 为 true 表示 focus 真正参与了匹配。
	FocusApplied bool
	// LatestFactAt 是这批事实里最新的真实时间。
	LatestFactAt time.Time
}

// activeAlertPredicate 与既有 fault 关联口径一致：Alertmanager 来源以 source_state 为
// 权威，本地来源退回 status。绝不把"暂时看不到"推断成 resolved。
const activeAlertPredicate = `(
	(a.upstream_fingerprint <> '' AND a.source_state = 'firing')
	OR (a.upstream_fingerprint = '' AND a.status <> 'resolved')
)`

// ReadDomainFacts 读取边界内的 active Alert/Fault/Impact/Incident 与服务级计数。
//
// 执行顺序刻意固定：alerts -> faults -> impacts -> incidents -> counts。每一步都完整
// 读完并释放 Rows 再发下一条（pgx 单连接约束）。focus 只收窄 alerts 与经 alert 关联的
// faults/impacts/incidents，计数始终按整个边界统计。
func (r *ServiceObservabilityRepo) ReadDomainFacts(
	ctx context.Context, tx pgx.Tx, query DomainFactsQuery,
) (DomainFactsRows, error) {
	result := DomainFactsRows{
		Alerts:    []DomainAlertRow{},
		Faults:    []DomainFaultRow{},
		Impacts:   []DomainImpactRow{},
		Incidents: []DomainIncidentRow{},
	}
	entityIDs := normalizeUUIDSet(query.EntityIDs)
	if len(entityIDs) == 0 {
		return result, nil
	}
	limit := clampObservabilityLimit(query.Limit)

	focusJSON := []byte("{}")
	focusActive := query.FocusKey != "" && query.FocusValue != ""
	if focusActive {
		encoded, err := json.Marshal(map[string]string{query.FocusKey: query.FocusValue})
		if err != nil {
			return result, fmt.Errorf("store: 序列化 focus 匹配条件失败: %w", err)
		}
		focusJSON = encoded
	}
	result.FocusApplied = focusActive

	alerts, focusedAlertIDs, err := r.readDomainAlerts(ctx, tx, entityIDs, focusActive, focusJSON, limit)
	if err != nil {
		return result, err
	}
	result.Alerts = alerts

	faults, err := r.readDomainFaults(ctx, tx, entityIDs, focusActive, focusedAlertIDs, limit)
	if err != nil {
		return result, err
	}
	result.Faults = faults

	faultIDs := make([]string, 0, len(faults))
	for _, fault := range faults {
		faultIDs = append(faultIDs, fault.FaultID)
	}
	if len(faultIDs) > 0 {
		if result.Impacts, err = r.readDomainImpacts(ctx, tx, faultIDs, limit); err != nil {
			return result, err
		}
		if result.Incidents, err = r.readDomainIncidents(ctx, tx, faultIDs, limit); err != nil {
			return result, err
		}
	}

	if result.Counts, err = r.readDomainCounts(ctx, tx, entityIDs); err != nil {
		return result, err
	}

	for _, alert := range result.Alerts {
		result.LatestFactAt = laterTime(result.LatestFactAt, alert.UpdatedAt)
	}
	for _, fault := range result.Faults {
		result.LatestFactAt = laterTime(result.LatestFactAt, fault.UpdatedAt)
	}
	for _, impact := range result.Impacts {
		result.LatestFactAt = laterTime(result.LatestFactAt, impact.UpdatedAt)
	}
	for _, incident := range result.Incidents {
		result.LatestFactAt = laterTime(result.LatestFactAt, incident.UpdatedAt)
	}
	return result, nil
}

// readDomainAlerts 取边界内的 active Alert。
//
// 边界关联有两条已提交路径：经 fault_alert_links 关联到边界 Fault，或经
// alert_mapping_results 直接映射到边界实体。两者都是事实，取并集；excluded 证据不算
// 关联。Silenced/Inhibited 只影响通知，仍然是有效证据，所以不按 suppression 过滤。
func (r *ServiceObservabilityRepo) readDomainAlerts(
	ctx context.Context, tx pgx.Tx, entityIDs []string, focusActive bool, focusJSON []byte, limit int,
) ([]DomainAlertRow, []string, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id::text, a.title, a.status, a.upstream_fingerprint, a.source_state,
			a.response_state, a.suppression_state, a.severity, a.starts_at, a.updated_at
		FROM alerts a
		WHERE `+activeAlertPredicate+`
			AND (
				a.id IN (
					SELECT l.alert_id FROM fault_alert_links l
					JOIN faults f ON f.id = l.fault_id
					WHERE f.resource_node_id = ANY($1::uuid[])
						AND f.status <> 'resolved' AND l.role <> 'excluded'
				)
				OR a.id IN (
					SELECT m.alert_id FROM alert_mapping_results m
					WHERE m.entity_id = ANY($1::uuid[])
				)
			)
			AND (NOT $2::boolean OR a.labels @> $3::jsonb OR a.resource_ref @> $3::jsonb)
		ORDER BY a.starts_at DESC, a.id
		LIMIT $4
	`, entityIDs, focusActive, focusJSON, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 查询边界内 Alert 失败: %w", err)
	}
	defer rows.Close()
	alerts := []DomainAlertRow{}
	ids := []string{}
	for rows.Next() {
		var alert DomainAlertRow
		if err := rows.Scan(&alert.AlertID, &alert.Title, &alert.Status,
			&alert.UpstreamFingerprint, &alert.SourceState, &alert.ResponseState,
			&alert.SuppressionState, &alert.Severity, &alert.StartsAt, &alert.UpdatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: 扫描边界内 Alert 失败: %w", err)
		}
		alerts = append(alerts, alert)
		ids = append(ids, alert.AlertID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: 遍历边界内 Alert 失败: %w", err)
	}
	return alerts, ids, nil
}

// readDomainFaults 取边界内未关闭的 Fault。
//
// focus 生效时只保留与 focus 匹配 Alert 有关联证据的 Fault：focus 是收窄，不是另建
// 一套边界。focus 匹配到零条 Alert 时结果为空，这是"查询成功但无结果"，不是失败。
func (r *ServiceObservabilityRepo) readDomainFaults(
	ctx context.Context, tx pgx.Tx, entityIDs []string, focusActive bool, focusedAlertIDs []string, limit int,
) ([]DomainFaultRow, error) {
	if focusActive && len(focusedAlertIDs) == 0 {
		return []DomainFaultRow{}, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT f.id::text, f.title, f.status, f.severity, f.kind, f.scope,
			COALESCE(f.resource_node_id::text, ''), COALESCE(n.name, ''),
			COALESCE(t.name, ''), f.first_detected_at, f.updated_at,
			COALESCE(ci.cnt, 0), COALESCE(pi.cnt, 0)
		FROM faults f
		JOIN resource_nodes n ON n.id = f.resource_node_id
		LEFT JOIN teams t ON t.id = n.owner_team_id
		LEFT JOIN (
			SELECT fault_id, count(*) AS cnt FROM fault_impacts
			WHERE state = 'confirmed' GROUP BY fault_id
		) ci ON ci.fault_id = f.id
		LEFT JOIN (
			SELECT fault_id, count(*) AS cnt FROM fault_impacts
			WHERE state = 'potential' GROUP BY fault_id
		) pi ON pi.fault_id = f.id
		WHERE f.resource_node_id = ANY($1::uuid[])
			AND f.status <> 'resolved'
			AND (NOT $2::boolean OR f.id IN (
				SELECT l.fault_id FROM fault_alert_links l
				WHERE l.alert_id = ANY($3::uuid[]) AND l.role <> 'excluded'
			))
		ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
			f.first_detected_at DESC, f.id
		LIMIT $4
	`, entityIDs, focusActive, focusedAlertIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询边界内 Fault 失败: %w", err)
	}
	defer rows.Close()
	faults := []DomainFaultRow{}
	for rows.Next() {
		var fault DomainFaultRow
		if err := rows.Scan(&fault.FaultID, &fault.Title, &fault.Status, &fault.Severity,
			&fault.Kind, &fault.Scope, &fault.EntityID, &fault.EntityName,
			&fault.OwnerTeamName, &fault.FirstDetectedAt, &fault.UpdatedAt,
			&fault.ConfirmedImpactCount, &fault.PotentialImpactCount); err != nil {
			return nil, fmt.Errorf("store: 扫描边界内 Fault 失败: %w", err)
		}
		faults = append(faults, fault)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历边界内 Fault 失败: %w", err)
	}
	return faults, nil
}

func (r *ServiceObservabilityRepo) readDomainImpacts(
	ctx context.Context, tx pgx.Tx, faultIDs []string, limit int,
) ([]DomainImpactRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT fi.fault_id::text, fi.node_id::text, n.name, n.layer, fi.state, fi.updated_at
		FROM fault_impacts fi
		JOIN resource_nodes n ON n.id = fi.node_id
		WHERE fi.fault_id = ANY($1::uuid[])
		ORDER BY fi.state, n.name, fi.fault_id
		LIMIT $2
	`, faultIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 FaultImpact 失败: %w", err)
	}
	defer rows.Close()
	impacts := []DomainImpactRow{}
	for rows.Next() {
		var impact DomainImpactRow
		if err := rows.Scan(&impact.FaultID, &impact.EntityID, &impact.EntityName,
			&impact.Layer, &impact.State, &impact.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描 FaultImpact 失败: %w", err)
		}
		impacts = append(impacts, impact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 FaultImpact 失败: %w", err)
	}
	return impacts, nil
}

func (r *ServiceObservabilityRepo) readDomainIncidents(
	ctx context.Context, tx pgx.Tx, faultIDs []string, limit int,
) ([]DomainIncidentRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (i.id)
			i.id::text, i.number, i.title, i.status, i.severity, l.relation, i.updated_at
		FROM incidents i
		JOIN incident_fault_links l ON l.incident_id = i.id
		WHERE l.fault_id = ANY($1::uuid[])
			AND l.disposition = 'included'
			AND i.status <> 'resolved'
		ORDER BY i.id, l.relation
		LIMIT $2
	`, faultIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询关联 Incident 失败: %w", err)
	}
	defer rows.Close()
	incidents := []DomainIncidentRow{}
	for rows.Next() {
		var incident DomainIncidentRow
		if err := rows.Scan(&incident.IncidentID, &incident.Number, &incident.Title,
			&incident.Status, &incident.Severity, &incident.Relation,
			&incident.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描关联 Incident 失败: %w", err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历关联 Incident 失败: %w", err)
	}
	sort.SliceStable(incidents, func(i, j int) bool { return incidents[i].Number > incidents[j].Number })
	return incidents, nil
}

func (r *ServiceObservabilityRepo) readDomainCounts(
	ctx context.Context, tx pgx.Tx, entityIDs []string,
) (DomainCountRow, error) {
	var counts DomainCountRow
	err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM faults f
				WHERE f.resource_node_id = ANY($1::uuid[]) AND f.status = 'active'),
			(SELECT count(DISTINCT (fi.fault_id, fi.node_id)) FROM fault_impacts fi
				JOIN faults f ON f.id = fi.fault_id
				WHERE f.resource_node_id = ANY($1::uuid[])
					AND f.status = 'active' AND fi.state = 'confirmed'),
			(SELECT count(DISTINCT i.id) FROM incidents i
				JOIN incident_fault_links l ON l.incident_id = i.id
				JOIN faults f ON f.id = l.fault_id
				WHERE f.resource_node_id = ANY($1::uuid[])
					AND l.disposition = 'included' AND i.status <> 'resolved')
	`, entityIDs).Scan(&counts.ActiveFaults, &counts.ConfirmedImpacts, &counts.ActiveIncidents)
	if err != nil {
		return DomainCountRow{}, fmt.Errorf("store: 统计领域计数失败: %w", err)
	}
	return counts, nil
}

// clampObservabilityLimit 把 limit 收敛到 [1, MaxObservabilityRows]。
func clampObservabilityLimit(limit int) int {
	if limit <= 0 || limit > MaxObservabilityRows {
		return MaxObservabilityRows
	}
	return limit
}

// clampTraversalDepth 把跳数收敛到 [1, MaxObservabilityTraversalDepth]。
func clampTraversalDepth(depth, fallback int) int {
	if depth <= 0 {
		depth = fallback
	}
	if depth <= 0 {
		depth = 1
	}
	if depth > MaxObservabilityTraversalDepth {
		depth = MaxObservabilityTraversalDepth
	}
	return depth
}

func observedStaleAfter(staleAfter time.Duration) time.Duration {
	if staleAfter <= 0 {
		return DefaultObservedStaleAfter
	}
	return staleAfter
}

// normalizeUUIDSet 去重、排序并丢弃形状非法的 ID。
//
// 丢弃而不是报错，是因为 Profile root 或调用方传入的集合里出现一个坏 ID 时，正确行为是
// 缩小边界而不是让整个 section 失败；真正需要 typed not_found 的路径（resolver）另有
// ErrServiceObservabilityInvalidID。
func normalizeUUIDSet(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if !looksLikeUUID(trimmed) {
			continue
		}
		lowered := strings.ToLower(trimmed)
		if _, exists := seen[lowered]; exists {
			continue
		}
		seen[lowered] = struct{}{}
		out = append(out, lowered)
	}
	sort.Strings(out)
	return out
}

// looksLikeUUID 只做形状检查（8-4-4-4-12 十六进制）。它的作用是在把值下传到
// ::uuid 转换之前挡住明显非法的输入：一次失败的转换会让整个只读事务进入 aborted 状态。
func looksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// escapeLikeFragment 转义 LIKE 通配符，配合 SQL 里的 ESCAPE '\'。
// 用户输入的 % 与 _ 必须是字面量，不能变成模式。
func escapeLikeFragment(fragment string) string {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return ""
	}
	replaced := strings.ReplaceAll(fragment, `\`, `\\`)
	replaced = strings.ReplaceAll(replaced, "%", `\%`)
	return strings.ReplaceAll(replaced, "_", `\_`)
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
