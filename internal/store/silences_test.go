//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// 本文件承接 config_repos_test.go：覆盖 silences.go 里的 silence / change_events /
// notification_log 三张表的仓储行为，同 build tag、同 setupTestStore 助手。

// TestSilenceRepo_ListActive_WindowBoundary 固化 ListActive 的边界语义：
// at == starts_at 命中（起始含），at == ends_at 不命中（结束不含）。
func TestSilenceRepo_ListActive_WindowBoundary(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	silences := NewSilenceRepo()

	starts := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	ends := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := silences.Create(ctx, tx, Silence{
			Matchers: domain.MatcherSet{{Name: "env", Op: domain.MatchEqual, Value: "staging"}},
			StartsAt: starts,
			EndsAt:   ends,
			Comment:  "计划维护",
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建静默失败: %v", err)
	}

	cases := []struct {
		name    string
		at      time.Time
		wantHit bool
	}{
		{"at == starts_at 命中", starts, true},
		{"窗口中段命中", starts.Add(time.Hour), true},
		{"at == ends_at 不命中", ends, false},
		{"窗口之前不命中", starts.Add(-time.Minute), false},
		{"窗口之后不命中", ends.Add(time.Minute), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var active []Silence
			err := st.WithTx(ctx, func(tx pgx.Tx) error {
				var err error
				active, err = silences.ListActive(ctx, tx, c.at, 0)
				return err
			})
			if err != nil {
				t.Fatalf("ListActive 失败: %v", err)
			}
			got := len(active) == 1
			if got != c.wantHit {
				t.Fatalf("at=%v: 命中=%v, want %v", c.at, got, c.wantHit)
			}
		})
	}
}

// TestChangeEventRepo_ListRecentByResource 验证 resource_ref JSONB 包含匹配与
// occurred_at 倒序（R7.4/R7.5）。
func TestChangeEventRepo_ListRecentByResource(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	changes := NewChangeEventRepo()

	now := time.Now().UTC().Truncate(time.Second)
	resourceA := map[string]string{"service.name": "checkout", "host.name": "node-1"}
	resourceB := map[string]string{"service.name": "billing"}

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		entries := []ChangeEvent{
			{Kind: "deploy", ResourceRef: resourceA, OccurredAt: now.Add(-2 * time.Hour), Payload: json.RawMessage(`{"version":"v1"}`)},
			{Kind: "deploy", ResourceRef: resourceA, OccurredAt: now.Add(-1 * time.Hour), Payload: json.RawMessage(`{"version":"v2"}`)},
			{Kind: "scale_out", ResourceRef: resourceB, OccurredAt: now.Add(-30 * time.Minute)},
		}
		for _, e := range entries {
			if _, err := changes.Insert(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("写入变更事件失败: %v", err)
	}

	var recent []ChangeEvent
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		recent, err = changes.ListRecentByResource(ctx, tx, map[string]string{"service.name": "checkout"}, now.Add(-3*time.Hour), 0)
		return err
	})
	if err != nil {
		t.Fatalf("ListRecentByResource 失败: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("期望只命中 resourceA 的 2 条，got %d: %+v", len(recent), recent)
	}
	if recent[0].OccurredAt.Before(recent[1].OccurredAt) {
		t.Fatalf("期望按 occurred_at 倒序，got %v then %v", recent[0].OccurredAt, recent[1].OccurredAt)
	}
	// payload 原样透传即可，这里只做内容检查，避免对 JSONB 格式化细节做过强假设。
	var v map[string]any
	if err := json.Unmarshal(recent[0].Payload, &v); err != nil {
		t.Fatalf("解析最近一条 payload 失败: %v", err)
	}
	if v["version"] != "v2" {
		t.Fatalf("最近一条 payload.version = %v, want v2", v["version"])
	}
}

// TestNotificationLogRepo_IdempotencyKey 验证幂等键查重：GetByIdempotencyKey 命中
// 已插入记录；对同一 idempotency_key 重复 Insert 返回 ErrIdempotencyConflict
// 而不是覆盖或报错中断（对应 handler 幂等约定）。
func TestNotificationLogRepo_IdempotencyKey(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	logs := NewNotificationLogRepo()

	entry := NotificationLogEntry{
		GroupKey:       "group-1",
		Channel:        "im",
		Target:         "user-1",
		IdempotencyKey: "alert-1:im:step-0",
	}

	var created *NotificationLogEntry
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = logs.Insert(ctx, tx, entry)
		return err
	})
	if err != nil {
		t.Fatalf("首次 Insert 失败: %v", err)
	}
	if created.Status != "pending" {
		t.Fatalf("created.Status = %q, want pending", created.Status)
	}

	var found *NotificationLogEntry
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		found, err = logs.GetByIdempotencyKey(ctx, tx, entry.IdempotencyKey)
		return err
	})
	if err != nil {
		t.Fatalf("GetByIdempotencyKey 失败: %v", err)
	}
	if found.ID != created.ID {
		t.Fatalf("found.ID = %s, want %s", found.ID, created.ID)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := logs.Insert(ctx, tx, entry)
		return err
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("重复 Insert err = %v, want ErrIdempotencyConflict", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return logs.UpdateStatus(ctx, tx, created.ID, "sent", 1, "")
	})
	if err != nil {
		t.Fatalf("UpdateStatus 失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		found, err = logs.GetByIdempotencyKey(ctx, tx, entry.IdempotencyKey)
		return err
	})
	if err != nil {
		t.Fatalf("再次 GetByIdempotencyKey 失败: %v", err)
	}
	if found.Status != "sent" || found.Attempts != 1 {
		t.Fatalf("found = %+v, want status=sent attempts=1", found)
	}
}
