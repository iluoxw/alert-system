package ccloud

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

type fakeFetcher struct {
	full     map[string]ProjectTopology
	svc      map[string]ServiceTopology
	fullErr  error
	svcErr   error
	fullHits int
	svcHits  int
}

func (f *fakeFetcher) FetchFullLinkTopology(_ context.Context, projectID string) (ProjectTopology, error) {
	f.fullHits++
	if f.fullErr != nil {
		return ProjectTopology{}, f.fullErr
	}
	return f.full[projectID], nil
}

func (f *fakeFetcher) FetchServiceTopology(_ context.Context, appID string) (ServiceTopology, error) {
	f.svcHits++
	if f.svcErr != nil {
		return ServiceTopology{}, f.svcErr
	}
	return f.svc[appID], nil
}

type fakeTx struct{}

func (fakeTx) WithTx(_ context.Context, fn func(tx pgx.Tx) error) error { return fn(nil) }

type fakeTopology struct {
	aliases    map[[2]string][]domain.ResourceNode
	imported   []domain.TopologySnapshot
	failures   []string
	unresolved []store.UnresolvedIdentity
	ensured    [][3]string
	importErr  error
}

func (f *fakeTopology) ImportSnapshot(_ context.Context, _ pgx.Tx, snapshot domain.TopologySnapshot) (store.TopologyImportResult, error) {
	if f.importErr != nil {
		return store.TopologyImportResult{}, f.importErr
	}
	f.imported = append(f.imported, snapshot)
	ids := map[string]string{}
	for i, n := range snapshot.Nodes {
		ids[n.ExternalRef] = "node-" + n.ExternalRef
		_ = i
	}
	return store.TopologyImportResult{NodeIDs: ids, NodeCount: len(snapshot.Nodes), EdgeCount: len(snapshot.Edges)}, nil
}

func (f *fakeTopology) RecordSyncFailure(_ context.Context, _ pgx.Tx, _ string, _ time.Time, errMsg string) error {
	f.failures = append(f.failures, errMsg)
	return nil
}

func (f *fakeTopology) ResolveAlias(_ context.Context, _ pgx.Tx, namespace, value string) ([]domain.ResourceNode, error) {
	return f.aliases[[2]string{namespace, value}], nil
}

func (f *fakeTopology) EnsureAlias(_ context.Context, _ pgx.Tx, entityID, namespace, value string) error {
	f.ensured = append(f.ensured, [3]string{entityID, namespace, value})
	return nil
}

func (f *fakeTopology) UpsertUnresolvedIdentities(_ context.Context, _ pgx.Tx, items []store.UnresolvedIdentity, _ time.Time) error {
	f.unresolved = append(f.unresolved, items...)
	return nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBackoffDelay(t *testing.T) {
	interval := time.Minute
	if got := backoffDelay(interval, 0); got != interval {
		t.Fatalf("failures=0: %s", got)
	}
	if got := backoffDelay(interval, 1); got != 2*interval {
		t.Fatalf("failures=1: %s", got)
	}
	if got := backoffDelay(interval, 3); got != 8*interval {
		t.Fatalf("failures=3: %s", got)
	}
	if got := backoffDelay(interval, 9); got != 8*interval {
		t.Fatalf("failures=9 应封顶 8x: %s", got)
	}
}

func TestRun_IntervalZero_NoIO(t *testing.T) {
	fetcher := &fakeFetcher{fullErr: errors.New("should not fetch")}
	s := NewSyncer(fakeTx{}, &fakeTopology{}, fetcher, []string{"p1"}, "production", 0, silentLogger())
	s.Run(context.Background())
	if fetcher.fullHits != 0 {
		t.Fatalf("interval=0 不应发起请求, hits=%d", fetcher.fullHits)
	}
}

func TestToDomainEdges_MapsStaleAndNodeIDs(t *testing.T) {
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	edges := toDomainEdges(ImportResult{Edges: []Edge{
		{FromRef: "app-1", ToRef: "rely-1", FromEntityID: "", ToEntityID: "canonical-rely", Stale: true},
	}}, now)
	if len(edges) != 1 {
		t.Fatalf("edges = %d", len(edges))
	}
	e := edges[0]
	if e.FromExternalRef != "app-1" || e.ToNodeID != "canonical-rely" {
		t.Fatalf("跨源字段未映射: %+v", e)
	}
	if !e.NoRecentTraffic || !e.ObservedAt.Equal(now) {
		t.Fatalf("Stale 应落 NoRecentTraffic 且不得改早 ObservedAt: %+v", e)
	}
	if e.Source != TopologySource || e.Origin != domain.RelationOriginObserved {
		t.Fatalf("source/origin: %+v", e)
	}
}

func TestSyncOnce_FetchFailureRecordsRunAndSkipsImport(t *testing.T) {
	topo := &fakeTopology{}
	s := NewSyncer(fakeTx{}, topo, &fakeFetcher{fullErr: errors.New("upstream 503")}, []string{"p1"}, "production", time.Minute, silentLogger())
	if err := s.SyncOnce(context.Background()); err == nil {
		t.Fatal("want fetch error")
	}
	if len(topo.failures) != 1 || len(topo.imported) != 0 {
		t.Fatalf("失败应只写 topology_sync_runs，不得 ImportSnapshot: failures=%d imported=%d", len(topo.failures), len(topo.imported))
	}
}

func TestSyncOnce_BuildsSnapshotAndUnresolved(t *testing.T) {
	app := TopologyNode{Key: "app-unmanaged", ServiceName: "demo-app", NodeType: nodeTypeCCloudApp}
	platform := TopologyNode{Key: "apisix-1", ServiceName: "gateway", NodeType: nodeTypeAPISIX}
	rely := TopologyNode{Key: "rely-0", ServiceName: ":0", NodeType: nodeTypeRelyNode, ServiceType: " REDIS"}
	fetcher := &fakeFetcher{
		full: map[string]ProjectTopology{
			"proj-a": {ProjectID: "proj-a", Nodes: []TopologyNode{app, platform}, Edges: []TopologyEdge{
				{From: "app-unmanaged", To: "apisix-1", TotalCount: 3},
			}},
		},
		svc: map[string]ServiceTopology{
			"app-unmanaged": {AppID: "app-unmanaged", Nodes: []TopologyNode{app, rely}, Edges: nil},
		},
	}
	topo := &fakeTopology{aliases: map[[2]string][]domain.ResourceNode{}}
	s := NewSyncer(fakeTx{}, topo, fetcher, []string{"proj-a"}, "production", time.Minute, silentLogger())
	s.now = func() time.Time { return time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC) }
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(topo.imported) != 1 {
		t.Fatalf("imported = %d", len(topo.imported))
	}
	snap := topo.imported[0]
	if snap.Source != TopologySource || len(snap.Nodes) != 2 {
		t.Fatalf("snapshot nodes=%d source=%s", len(snap.Nodes), snap.Source)
	}
	// 环境必须取自配置：平台响应没有环境字段，曾误用项目 ID（proj-a）作为环境。
	if snap.Environment != "production" {
		t.Fatalf("snapshot environment = %q, want production", snap.Environment)
	}
	for _, n := range snap.Nodes {
		if n.Environment != "production" {
			t.Fatalf("导入实体 %s 的 environment = %q, want production（不得为项目 ID）", n.ExternalRef, n.Environment)
		}
	}
	if len(snap.Edges) != 1 || snap.Edges[0].NoRecentTraffic {
		t.Fatalf("有流量边不应标 NoRecentTraffic: %+v", snap.Edges)
	}
	if len(topo.unresolved) != 1 || topo.unresolved[0].Value != "rely-0" {
		t.Fatalf("未对齐 RELY_NODE 应进队列: %+v", topo.unresolved)
	}
	if fetcher.fullHits != 1 || fetcher.svcHits != 1 {
		t.Fatalf("hits full=%d svc=%d", fetcher.fullHits, fetcher.svcHits)
	}
}

func TestSyncOnce_IdempotentReplay(t *testing.T) {
	app := TopologyNode{Key: "app-1", ServiceName: "app", NodeType: nodeTypeCCloudApp}
	fetcher := &fakeFetcher{
		full: map[string]ProjectTopology{"p": {ProjectID: "p", Nodes: []TopologyNode{app}}},
		svc:  map[string]ServiceTopology{"app-1": {AppID: "app-1"}},
	}
	topo := &fakeTopology{}
	s := NewSyncer(fakeTx{}, topo, fetcher, []string{"p"}, "production", time.Minute, silentLogger())
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(topo.imported) != 2 || len(topo.imported[0].Edges) != len(topo.imported[1].Edges) {
		t.Fatalf("重放边数应不变: %+v", topo.imported)
	}
}
