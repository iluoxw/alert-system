package ccloud

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

// TopologySource 是 ImportSnapshot / topology_sync_runs 使用的 source 字面量。
// 按 source 隔离：本轮只替换 ccloud 边，不得触碰 entity_registry 等其它来源。
const TopologySource = "ccloud"

type txRunner interface {
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

type topologyFetcher interface {
	FetchFullLinkTopology(ctx context.Context, projectID string) (ProjectTopology, error)
	FetchServiceTopology(ctx context.Context, appID string) (ServiceTopology, error)
}

type topologyWriter interface {
	ImportSnapshot(ctx context.Context, tx pgx.Tx, snapshot domain.TopologySnapshot) (store.TopologyImportResult, error)
	RecordSyncFailure(ctx context.Context, tx pgx.Tx, source string, observedAt time.Time, errMsg string) error
	ResolveAlias(ctx context.Context, tx pgx.Tx, namespace, value string) ([]domain.ResourceNode, error)
	EnsureAlias(ctx context.Context, tx pgx.Tx, entityID, namespace, value string) error
	UpsertUnresolvedIdentities(ctx context.Context, tx pgx.Tx, items []store.UnresolvedIdentity, now time.Time) error
}

// Syncer 是 ccloud 拓扑的独立周期 loop（不变量 23）：事务外 fetch，短事务
// ImportSnapshot。不使用 scheduler timer。
type Syncer struct {
	store      txRunner
	topology   topologyWriter
	client     topologyFetcher
	projectIDs []string
	// environment 由配置显式声明，写入全部导入实体；平台响应里没有环境字段。
	environment string
	interval    time.Duration
	logger      *slog.Logger
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewSyncer 构造同步器。interval<=0 时 Run 立即返回，不产生任何 IO。
func NewSyncer(st txRunner, topology topologyWriter, client topologyFetcher, projectIDs []string, environment string, interval time.Duration, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		store: st, topology: topology, client: client,
		projectIDs: append([]string(nil), projectIDs...), environment: environment,
		interval: interval, logger: logger, now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// Run 启动即跑一轮，之后按 interval 循环；连续失败指数退避（上限 8×interval），
// 避免失败重试风暴。ctx 取消后退出。interval<=0 直接返回。
func (s *Syncer) Run(ctx context.Context) {
	if s.interval <= 0 {
		return
	}
	failures := 0
	for {
		if err := s.SyncOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error("ccloud: 拓扑同步失败", "err", err, "failures", failures+1)
			failures++
		} else if err == nil {
			failures = 0
		}
		if err := s.sleep(ctx, backoffDelay(s.interval, failures)); err != nil {
			return
		}
	}
}

func backoffDelay(interval time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return interval
	}
	shift := failures
	if shift > 3 {
		shift = 3
	}
	return interval * time.Duration(1<<shift)
}

// SyncOnce 拉取全部项目的全链路 + 各应用服务拓扑，映射后一次性 ImportSnapshot
// （必须同一次替换全部 ccloud 边，否则后一个项目会删掉前一个项目的边）。
func (s *Syncer) SyncOnce(ctx context.Context) error {
	observedAt := s.now()
	full, svc, err := s.fetchAll(ctx)
	if err != nil {
		if recErr := s.recordFailure(ctx, observedAt, err); recErr != nil {
			s.logger.Error("ccloud: 记录同步失败失败", "err", recErr)
		}
		return err
	}

	var result ImportResult
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		resolve := func(namespace, value string) (string, bool) {
			nodes, resolveErr := s.topology.ResolveAlias(ctx, tx, namespace, value)
			if resolveErr != nil || len(nodes) != 1 {
				return "", false
			}
			return nodes[0].ID, true
		}
		result = MapTopology(mergeProjects(full), svc, resolve)
		snapshot := buildSnapshot(s.environment, observedAt, collectNodes(full, svc), result)
		imported, importErr := s.topology.ImportSnapshot(ctx, tx, snapshot)
		if importErr != nil {
			return importErr
		}
		if err := s.persistAliasesAndUnresolved(ctx, tx, collectNodes(full, svc), result, imported, observedAt); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if recErr := s.recordFailure(ctx, observedAt, err); recErr != nil {
			s.logger.Error("ccloud: 记录同步失败失败", "err", recErr)
		}
		return err
	}

	if result.DroppedExternalHTTP > 0 {
		s.logger.Warn("ccloud: EXTERNAL_HTTP_SERVER 节点被忽略且计数非零，需重新评估忽略规则",
			"dropped_external_http", result.DroppedExternalHTTP)
	}
	s.logger.Info("ccloud: 拓扑同步完成",
		"edges", len(result.Edges),
		"unresolved", len(result.Unresolved),
		"dropped_self_loop", result.DroppedSelfLoop,
		"dropped_external_http", result.DroppedExternalHTTP)
	return nil
}

func (s *Syncer) fetchAll(ctx context.Context) ([]ProjectTopology, []ServiceTopology, error) {
	full := make([]ProjectTopology, 0, len(s.projectIDs))
	appIDs := make([]string, 0)
	seenApp := make(map[string]struct{})
	for _, projectID := range s.projectIDs {
		topo, err := s.client.FetchFullLinkTopology(ctx, projectID)
		if err != nil {
			return nil, nil, err
		}
		full = append(full, topo)
		for _, n := range topo.Nodes {
			if ClassifyNode(n) == NodeClassApp {
				if _, ok := seenApp[n.Key]; ok {
					continue
				}
				seenApp[n.Key] = struct{}{}
				appIDs = append(appIDs, n.Key)
			}
		}
	}
	svc := make([]ServiceTopology, 0, len(appIDs))
	for _, appID := range appIDs {
		topo, err := s.client.FetchServiceTopology(ctx, appID)
		if err != nil {
			return nil, nil, err
		}
		svc = append(svc, topo)
	}
	return full, svc, nil
}

func (s *Syncer) persistAliasesAndUnresolved(ctx context.Context, tx pgx.Tx, nodes map[string]TopologyNode, result ImportResult, imported store.TopologyImportResult, now time.Time) error {
	for key, entityID := range result.ResolvedApps {
		if err := s.topology.EnsureAlias(ctx, tx, entityID, AliasNamespaceAppID, key); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		switch ClassifyNode(n) {
		case NodeClassApp:
			if _, resolved := result.ResolvedApps[n.Key]; resolved {
				continue
			}
			if id := imported.NodeIDs[n.Key]; id != "" {
				if err := s.topology.EnsureAlias(ctx, tx, id, AliasNamespaceAppID, n.Key); err != nil {
					return err
				}
			}
		case NodeClassRelyNode:
			id, ok := resolveFromResult(result, n.Key)
			if !ok {
				if nodes, err := s.topology.ResolveAlias(ctx, tx, AliasNamespaceRelyNode, n.Key); err == nil && len(nodes) == 1 {
					id, ok = nodes[0].ID, true
				} else if IsLegalHostPort(n.ServiceName) {
					if nodes, err := s.topology.ResolveAlias(ctx, tx, AliasNamespaceRelyNode, n.ServiceName); err == nil && len(nodes) == 1 {
						id, ok = nodes[0].ID, true
					}
				}
			}
			if !ok {
				continue
			}
			if err := s.topology.EnsureAlias(ctx, tx, id, AliasNamespaceRelyNode, n.Key); err != nil {
				return err
			}
			if IsLegalHostPort(n.ServiceName) {
				if err := s.topology.EnsureAlias(ctx, tx, id, AliasNamespaceRelyNode, n.ServiceName); err != nil {
					return err
				}
			}
		}
	}
	items := make([]store.UnresolvedIdentity, 0, len(result.Unresolved))
	for _, u := range result.Unresolved {
		items = append(items, store.UnresolvedIdentity{
			Namespace: u.Namespace, Value: u.Value, HostPort: u.HostPort,
			ServiceType: u.ServiceType, SampleApp: u.SampleAppID,
		})
	}
	return s.topology.UpsertUnresolvedIdentities(ctx, tx, items, now)
}

func resolveFromResult(result ImportResult, relyKey string) (string, bool) {
	for _, e := range result.Edges {
		if e.FromRef == relyKey && e.FromEntityID != "" {
			return e.FromEntityID, true
		}
		if e.ToRef == relyKey && e.ToEntityID != "" {
			return e.ToEntityID, true
		}
	}
	return "", false
}

func (s *Syncer) recordFailure(ctx context.Context, observedAt time.Time, syncErr error) error {
	return s.store.WithTx(ctx, func(tx pgx.Tx) error {
		return s.topology.RecordSyncFailure(ctx, tx, TopologySource, observedAt, syncErr.Error())
	})
}

func mergeProjects(full []ProjectTopology) ProjectTopology {
	merged := ProjectTopology{ProjectID: firstProjectID(full)}
	seenNode := make(map[string]struct{})
	seenEdge := make(map[[2]string]struct{})
	for _, p := range full {
		for _, n := range p.Nodes {
			if _, ok := seenNode[n.Key]; ok {
				continue
			}
			seenNode[n.Key] = struct{}{}
			merged.Nodes = append(merged.Nodes, n)
		}
		for _, e := range p.Edges {
			key := [2]string{e.From, e.To}
			if _, ok := seenEdge[key]; ok {
				continue
			}
			seenEdge[key] = struct{}{}
			merged.Edges = append(merged.Edges, e)
		}
	}
	return merged
}

func firstProjectID(full []ProjectTopology) string {
	if len(full) == 0 {
		return TopologySource
	}
	return full[0].ProjectID
}

func collectNodes(full []ProjectTopology, svc []ServiceTopology) map[string]TopologyNode {
	nodes := make(map[string]TopologyNode)
	for _, p := range full {
		for _, n := range p.Nodes {
			nodes[n.Key] = n
		}
	}
	for _, s := range svc {
		for _, n := range s.Nodes {
			nodes[n.Key] = n
		}
	}
	return nodes
}

func displayName(n TopologyNode) string {
	if name := strings.TrimSpace(n.ServiceName); name != "" {
		return name
	}
	return n.Key
}

// buildSnapshot 把 MapTopology 结果转成 ImportSnapshot 输入：未登记的 CCLOUD_APP
// 与 APISIX/WAF 进入 Nodes；已登记应用与已对齐 RELY_NODE 只通过 NodeID 出现在边上
// （跨源边，任务 8）。Stale → NoRecentTraffic，不得改早 ObservedAt。
func buildSnapshot(environment string, observedAt time.Time, nodes map[string]TopologyNode, result ImportResult) domain.TopologySnapshot {
	snapshotNodes := make([]domain.ResourceNode, 0)
	seen := make(map[string]struct{})
	for _, n := range nodes {
		switch ClassifyNode(n) {
		case NodeClassApp:
			if _, resolved := result.ResolvedApps[n.Key]; resolved {
				continue
			}
		case NodeClassPlatform:
		default:
			continue
		}
		if _, ok := seen[n.Key]; ok {
			continue
		}
		seen[n.Key] = struct{}{}
		kind := domain.ResourceNodeKindBusinessSystem
		if ClassifyNode(n) == NodeClassPlatform {
			kind = domain.ResourceNodeKindInfrastructureCapability
		}
		snapshotNodes = append(snapshotNodes, domain.ResourceNode{
			ExternalRef: n.Key,
			Kind:        kind,
			Name:        displayName(n),
			Provider:    TopologySource,
			Environment: environment,
			Source:      TopologySource,
		})
	}
	return domain.TopologySnapshot{
		SchemaVersion: 1,
		Source:        TopologySource,
		Environment:   environment,
		ObservedAt:    observedAt,
		Nodes:         snapshotNodes,
		Edges:         toDomainEdges(result, observedAt),
	}
}

func toDomainEdges(result ImportResult, observedAt time.Time) []domain.DependencyEdge {
	edges := make([]domain.DependencyEdge, 0, len(result.Edges))
	for _, e := range result.Edges {
		edges = append(edges, domain.DependencyEdge{
			Source:          TopologySource,
			FromExternalRef: e.FromRef,
			ToExternalRef:   e.ToRef,
			FromNodeID:      e.FromEntityID,
			ToNodeID:        e.ToEntityID,
			Relation:        domain.DependencyRelationDependsOn,
			Origin:          domain.RelationOriginObserved,
			ObservedAt:      observedAt,
			NoRecentTraffic: e.Stale,
		})
	}
	return edges
}
