//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestUpsertUnresolvedIdentities_FirstSeenStable(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewTopologyRepo()
	first := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	second := first.Add(2 * time.Hour)

	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.UpsertUnresolvedIdentities(ctx, tx, []UnresolvedIdentity{{
			Namespace: "ccloud.rely_node", Value: "rely-key-1",
			HostPort: ":0", ServiceType: "REDIS", SampleApp: "app-a",
		}}, first)
	}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return repo.UpsertUnresolvedIdentities(ctx, tx, []UnresolvedIdentity{{
			Namespace: "ccloud.rely_node", Value: "rely-key-1",
			HostPort: "10.0.0.1:6379", ServiceType: "REDIS", SampleApp: "app-b",
		}}, second)
	}); err != nil {
		t.Fatalf("再次写入: %v", err)
	}

	var items []UnresolvedIdentity
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		items, listErr = repo.ListUnresolvedIdentities(ctx, tx, 50, 0)
		return listErr
	}); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应幂等收敛为 1 行, got %d", len(items))
	}
	got := items[0]
	if !got.FirstSeen.Equal(first) {
		t.Fatalf("first_seen 被改写: %s want %s", got.FirstSeen, first)
	}
	if !got.LastSeen.Equal(second) {
		t.Fatalf("last_seen 未覆盖: %s want %s", got.LastSeen, second)
	}
	if got.HostPort != "10.0.0.1:6379" || got.SampleApp != "app-b" {
		t.Fatalf("展示字段未覆盖: %+v", got)
	}
}

func TestListUnresolvedIdentities_EmptySlice(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	repo := NewTopologyRepo()
	var items []UnresolvedIdentity
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		items, listErr = repo.ListUnresolvedIdentities(ctx, tx, 10, 0)
		return listErr
	}); err != nil {
		t.Fatal(err)
	}
	if items == nil {
		t.Fatal("空集合必须返回 [] 而不是 nil")
	}
	if len(items) != 0 {
		t.Fatalf("len = %d", len(items))
	}
}
