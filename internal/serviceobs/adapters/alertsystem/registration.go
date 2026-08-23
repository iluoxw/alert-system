// Package alertsystem 是 Service Observability ports 的 alert-system 只读实现。
//
// 生产主落地是 alert-system：identity、canonical topology 与 Alert/Fault/Impact/Incident
// 都来自本系统已提交的事实。本包只 SELECT，不创建领域对象，不推进任何状态机，也不从
// Fault 数量推导 Health。
//
// 四个 adapter 各自独立：registration、topology、domain、profile resolver 每次调用都
// 开自己的只读一致读事务。这样一个非关键 section 失败不会拖垮其它 section——尤其是
// topology 失败时 domain 仍按自己独立计算的稳定边界返回事实，既不会变空，也不会放宽
// 到全库。
//
// fixtures 绝不进入这条路径：本包不读 docs/contracts 的任何 example，也不访问外部网络。
package alertsystem

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/store"
)

// Reader 是一个只读一致读事务内可用的查询集合。
//
// 它以接口出现的唯一目的是让 adapter 的编排逻辑（边界计算、bounds 标记、focus 白名单、
// typed 错误分流）能在没有数据库的单元测试里被覆盖；真实实现直接调用
// internal/store 的只读仓储，SQL 由 store 层负责。
type Reader interface {
	ListServiceIdentities(ctx context.Context, filter store.ServiceIdentityFilter) ([]store.ServiceIdentityRow, bool, error)
	ScanServiceIdentities(ctx context.Context, filter store.ServiceIdentityFilter, after store.ServiceIdentityScanCursor, batchSize int) ([]store.ServiceIdentityRow, error)
	GetServiceIdentity(ctx context.Context, serviceID string) (store.ServiceIdentityRow, error)
	ListDeclaredDependencies(ctx context.Context, entityID string, limit int) ([]store.DeclaredEdgeRow, error)
	ListCanonicalNeighborhood(ctx context.Context, query store.NeighborhoodQuery) (store.NeighborhoodRows, error)
	ServiceBoundaryEntityIDs(ctx context.Context, serviceEntityID string, rootEntityIDs []string, maxDepth, maxEntities int) ([]string, bool, error)
	DeclaredReachableEntityIDs(ctx context.Context, serviceEntityID string, maxDepth int) (map[string]struct{}, error)
	ResolveServiceByID(ctx context.Context, serviceID string) (store.ServiceResolutionRow, error)
	ResolveEntityByID(ctx context.Context, entityID string) (store.EntityResolutionRow, error)
	ReadDomainFacts(ctx context.Context, query store.DomainFactsQuery) (store.DomainFactsRows, error)
}

// ReadRunner 在一个只读一致读事务内提供 Reader。每个 adapter 调用恰好用一次 WithRead，
// 因此"adapter 内部一致读"是事务隔离级别给的保证，不是约定。
type ReadRunner interface {
	WithRead(ctx context.Context, fn func(Reader) error) error
}

// Limits 是 adapter 的有界参数。零值走各自的默认上界，永远不会变成无界查询。
type Limits struct {
	// DirectoryLimit 是目录一次返回的最大服务数。
	DirectoryLimit int
	// DependencyLimit 是单个服务 declared 依赖的最大条数。
	DependencyLimit int
	// NeighborhoodDepth 是 bounded neighborhood 的跳数。
	NeighborhoodDepth int
	// MaxNodes / MaxRelations 达到上限时 topology 显式 partial。
	MaxNodes     int
	MaxRelations int
	// BoundaryDepth / MaxBoundaryEntities 限制领域实体边界。
	BoundaryDepth       int
	MaxBoundaryEntities int
	// DomainLimit 是单类领域对象的最大条数。
	DomainLimit int
	// ObservedStaleAfter 是 observed 关系的 freshness TTL；declared 不因时间过期。
	ObservedStaleAfter time.Duration
	// ReachableDepth 是 Profile root 可达性校验的跳数上界。
	ReachableDepth int
}

const (
	defaultDirectoryLimit      = 200
	defaultDependencyLimit     = 200
	defaultNeighborhoodDepth   = 2
	defaultMaxNodes            = 100
	defaultMaxRelations        = 200
	defaultBoundaryDepth       = 1
	defaultMaxBoundaryEntities = 200
	defaultDomainLimit         = 200
	defaultObservedStaleAfter  = 24 * time.Hour
	defaultReachableDepth      = 8
)

func (l Limits) withDefaults() Limits {
	if l.DirectoryLimit <= 0 {
		l.DirectoryLimit = defaultDirectoryLimit
	}
	if l.DependencyLimit <= 0 {
		l.DependencyLimit = defaultDependencyLimit
	}
	if l.NeighborhoodDepth <= 0 {
		l.NeighborhoodDepth = defaultNeighborhoodDepth
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = defaultMaxNodes
	}
	if l.MaxRelations <= 0 {
		l.MaxRelations = defaultMaxRelations
	}
	if l.BoundaryDepth <= 0 {
		l.BoundaryDepth = defaultBoundaryDepth
	}
	if l.MaxBoundaryEntities <= 0 {
		l.MaxBoundaryEntities = defaultMaxBoundaryEntities
	}
	if l.DomainLimit <= 0 {
		l.DomainLimit = defaultDomainLimit
	}
	if l.ObservedStaleAfter <= 0 {
		l.ObservedStaleAfter = defaultObservedStaleAfter
	}
	if l.ReachableDepth <= 0 {
		l.ReachableDepth = defaultReachableDepth
	}
	return l
}

// maxFilterFragment 限制目录模糊匹配片段长度。超长片段直接截断，避免把巨大的字符串
// 下传到 ILIKE。
const maxFilterFragment = 128

// Clock 让 ReadAt 可注入。adapter 只用它标记"这次一致读发生在什么时候"，绝不用它伪造
// 观测时间。
type Clock func() time.Time

func (c Clock) now() time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	return c().UTC()
}

// NewRunner 返回基于 internal/store 的真实 ReadRunner。
func NewRunner(db *store.Store) ReadRunner {
	return &storeRunner{db: db, repo: store.NewServiceObservabilityRepo()}
}

type storeRunner struct {
	db   *store.Store
	repo *store.ServiceObservabilityRepo
}

// WithRead 开一个显式只读、REPEATABLE READ 的事务。context deadline/cancellation 原样
// 由 pgx 下传到数据库。
func (r *storeRunner) WithRead(ctx context.Context, fn func(Reader) error) error {
	return r.db.WithReadTx(ctx, func(tx pgx.Tx) error {
		return fn(&txReader{repo: r.repo, tx: tx})
	})
}

type txReader struct {
	repo *store.ServiceObservabilityRepo
	tx   pgx.Tx
}

func (t *txReader) ListServiceIdentities(ctx context.Context, filter store.ServiceIdentityFilter) ([]store.ServiceIdentityRow, bool, error) {
	return t.repo.ListServiceIdentities(ctx, t.tx, filter)
}

func (t *txReader) ScanServiceIdentities(ctx context.Context, filter store.ServiceIdentityFilter, after store.ServiceIdentityScanCursor, batchSize int) ([]store.ServiceIdentityRow, error) {
	return t.repo.ScanServiceIdentities(ctx, t.tx, filter, after, batchSize)
}

func (t *txReader) GetServiceIdentity(ctx context.Context, serviceID string) (store.ServiceIdentityRow, error) {
	return t.repo.GetServiceIdentity(ctx, t.tx, serviceID)
}

func (t *txReader) ListDeclaredDependencies(ctx context.Context, entityID string, limit int) ([]store.DeclaredEdgeRow, error) {
	return t.repo.ListDeclaredDependencies(ctx, t.tx, entityID, limit)
}

func (t *txReader) ListCanonicalNeighborhood(ctx context.Context, query store.NeighborhoodQuery) (store.NeighborhoodRows, error) {
	return t.repo.ListCanonicalNeighborhood(ctx, t.tx, query)
}

func (t *txReader) ServiceBoundaryEntityIDs(ctx context.Context, serviceEntityID string, rootEntityIDs []string, maxDepth, maxEntities int) ([]string, bool, error) {
	return t.repo.ServiceBoundaryEntityIDs(ctx, t.tx, serviceEntityID, rootEntityIDs, maxDepth, maxEntities)
}

func (t *txReader) DeclaredReachableEntityIDs(ctx context.Context, serviceEntityID string, maxDepth int) (map[string]struct{}, error) {
	return t.repo.DeclaredReachableEntityIDs(ctx, t.tx, serviceEntityID, maxDepth)
}

func (t *txReader) ResolveServiceByID(ctx context.Context, serviceID string) (store.ServiceResolutionRow, error) {
	return t.repo.ResolveServiceByID(ctx, t.tx, serviceID)
}

func (t *txReader) ResolveEntityByID(ctx context.Context, entityID string) (store.EntityResolutionRow, error) {
	return t.repo.ResolveEntityByID(ctx, t.tx, entityID)
}

func (t *txReader) ReadDomainFacts(ctx context.Context, query store.DomainFactsQuery) (store.DomainFactsRows, error) {
	return t.repo.ReadDomainFacts(ctx, t.tx, query)
}

// RegistrationAdapter 实现 serviceobs.RegistrationReader。
type RegistrationAdapter struct {
	runner ReadRunner
	limits Limits
	clock  Clock
}

// NewRegistrationAdapter 构造 registration adapter。
func NewRegistrationAdapter(runner ReadRunner, limits Limits, clock Clock) *RegistrationAdapter {
	return &RegistrationAdapter{runner: runner, limits: limits.withDefaults(), clock: clock}
}

var (
	_ serviceobs.RegistrationReader = (*RegistrationAdapter)(nil)
	_ serviceobs.CandidateScanner   = (*RegistrationAdapter)(nil)
)

// ScanCandidates 在**一个**只读一致读事务内用 keyset 分批走完 metadata 过滤后的完整
// 候选集合。
//
// 三条口径是这个方法存在的全部理由：
//
//  1. 目录摘要的分母是完整候选集合，所以不能被页面 limit 或 adapter 的 DirectoryLimit
//     悄悄截断——那会报出一个看起来完整、实际错误的计数；
//  2. 遍历必须有界：单批有 BatchSize（再受 store 单批硬上界约束），总量有显式安全上界；
//  3. 超过安全上界必须**显式失败**（ErrCandidateSetTooLarge），绝不返回被截断的集合。
//     判定方式是多取一行：恰好等于上界被接受，只有严格超过才失败。
//
// 全部批次共用同一个 REPEATABLE READ 快照，因此候选身份集合本身是一致的，不会因为
// 遍历期间的登记变更而漏行或重复。
func (a *RegistrationAdapter) ScanCandidates(
	ctx context.Context, req serviceobs.CandidateScanRequest,
) (serviceobs.CandidateSet, error) {
	batchSize := req.BatchSize
	if batchSize <= 0 {
		batchSize = serviceobs.DefaultCandidateBatchSize
	}
	if batchSize > a.limits.DirectoryLimit {
		batchSize = a.limits.DirectoryLimit
	}
	maxCandidates := req.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = serviceobs.DefaultMaxDirectoryCandidates
	}
	filter := store.ServiceIdentityFilter{
		Environment: req.Filter.Environment,
		TeamID:      req.Filter.TeamID,
		Tier:        req.Filter.Tier,
		Query:       truncate(req.Filter.Query, maxFilterFragment),
	}

	items := make([]serviceobs.ServiceIdentity, 0, batchSize)
	overflow := false
	err := a.runner.WithRead(ctx, func(reader Reader) error {
		var after store.ServiceIdentityScanCursor
		for {
			rows, err := reader.ScanServiceIdentities(ctx, filter, after, batchSize)
			if err != nil {
				return err
			}
			for _, row := range rows {
				items = append(items, projectIdentity(row))
				after = store.ServiceIdentityScanCursor{
					Tier: string(row.Tier), Name: row.Name, ServiceID: row.ServiceID,
				}
				// 只需要多取一行就能判定"严格超过上界"：恰好等于上界必须被接受，
				// 因此这里是 > 而不是 >=，而且判定发生在追加之后。
				if len(items) > maxCandidates {
					overflow = true
					return nil
				}
			}
			// 不足一批说明候选集合已经走完。恰好取满时再要一批（可能为空），
			// 这就是"取一行到上界之外"的自然实现。
			if len(rows) < batchSize {
				return nil
			}
		}
	})
	if err != nil {
		return serviceobs.CandidateSet{}, fmt.Errorf("alertsystem: 扫描候选服务失败: %w", err)
	}
	if overflow {
		return serviceobs.CandidateSet{}, fmt.Errorf("%w: 上界 %d",
			serviceobs.ErrCandidateSetTooLarge, maxCandidates)
	}
	return serviceobs.CandidateSet{Items: items, ReadAt: a.clock.now()}, nil
}

// ListServices 是目录用的轻量 identity 查询。
//
// 它只做一条 SELECT：enabled-only、metadata 参数化、不逐行拉 aliases/dependencies。
// 目录有几百行，逐行再查两张表就是 N+1；密度需求由 Page View 投影满足，不靠加查询。
func (a *RegistrationAdapter) ListServices(
	ctx context.Context, filter serviceobs.RegistrationFilter,
) (serviceobs.RegistrationList, error) {
	limit := filter.Limit
	if limit <= 0 || limit > a.limits.DirectoryLimit {
		limit = a.limits.DirectoryLimit
	}
	storeFilter := store.ServiceIdentityFilter{
		Environment: filter.Environment,
		TeamID:      filter.TeamID,
		Tier:        filter.Tier,
		Query:       truncate(filter.Query, maxFilterFragment),
		Limit:       limit,
	}

	result := serviceobs.RegistrationList{Items: []serviceobs.ServiceIdentity{}}
	err := a.runner.WithRead(ctx, func(reader Reader) error {
		rows, truncated, err := reader.ListServiceIdentities(ctx, storeFilter)
		if err != nil {
			return err
		}
		result.Truncated = truncated
		result.Items = make([]serviceobs.ServiceIdentity, 0, len(rows))
		for _, row := range rows {
			result.Items = append(result.Items, projectIdentity(row))
		}
		return nil
	})
	if err != nil {
		return serviceobs.RegistrationList{}, fmt.Errorf("alertsystem: 读取服务目录失败: %w", err)
	}
	result.ReadAt = a.clock.now()
	return result, nil
}

// ReadService 读取单个服务的身份与 declared 依赖。
//
// 身份与依赖在同一个只读一致读事务内完成，因此不会出现"身份是新的、依赖是旧的"这种
// 撕裂快照。服务不存在（含 ID 形状非法）统一映射成 serviceobs.ErrServiceNotFound，
// 上层按 404 处理，不泄露存在性。
func (a *RegistrationAdapter) ReadService(
	ctx context.Context, serviceID string,
) (serviceobs.ServiceFacts, error) {
	var facts serviceobs.ServiceFacts
	err := a.runner.WithRead(ctx, func(reader Reader) error {
		row, err := reader.GetServiceIdentity(ctx, serviceID)
		if err != nil {
			return err
		}
		facts.Identity = projectIdentity(row)
		edges, err := reader.ListDeclaredDependencies(ctx, row.EntityID, a.limits.DependencyLimit)
		if err != nil {
			return err
		}
		facts.Dependencies = make([]serviceobs.DeclaredDependency, 0, len(edges))
		for _, edge := range edges {
			facts.Dependencies = append(facts.Dependencies, serviceobs.DeclaredDependency{
				RelationID:   edge.RelationID,
				FromEntityID: edge.FromEntityID,
				ToEntityID:   edge.ToEntityID,
				ToEntityKey:  edge.ToEntityKey,
				ToEntityName: edge.ToEntityName,
				ToLayer:      model.EntityLayer(edge.ToLayer),
				ToKind:       edge.ToKind,
				Relation:     edge.Relation,
				Criticality:  edge.Criticality,
				// 登记依赖是服务负责人声明的结构事实，永远是 declared。
				Provenance: model.ProvenanceDeclared,
				ObservedAt: edge.ObservedAt,
			})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrServiceObservabilityNotFound) ||
			errors.Is(err, store.ErrServiceObservabilityInvalidID) {
			return serviceobs.ServiceFacts{}, serviceobs.ErrServiceNotFound
		}
		return serviceobs.ServiceFacts{}, fmt.Errorf("alertsystem: 读取服务登记失败: %w", err)
	}
	facts.ReadAt = a.clock.now()
	return facts, nil
}

func projectIdentity(row store.ServiceIdentityRow) serviceobs.ServiceIdentity {
	return serviceobs.ServiceIdentity{
		ServiceID:        row.ServiceID,
		EntityID:         row.EntityID,
		EntityKey:        row.EntityKey,
		Name:             row.Name,
		Description:      row.Description,
		Layer:            model.EntityLayer(row.Layer),
		Kind:             model.EntityKind(row.Kind),
		Environment:      row.Environment,
		OwnerTeamID:      row.OwnerTeamID,
		OwnerTeamName:    row.OwnerTeamName,
		Tier:             model.ServiceTier(row.Tier),
		DashboardURL:     row.DashboardURL,
		RunbookURL:       row.RunbookURL,
		Enabled:          row.Enabled,
		EntityObservedAt: row.EntityObservedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// identifierPattern 与 Profile schema 的 identifier 一致。adapter 再校验一次是纵深
// 防御：即便未来 Registry 放宽，也不会有奇怪的 key 进到 JSONB 匹配参数里。
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
