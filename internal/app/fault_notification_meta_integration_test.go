//go:build integration

package app

import (
	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestFaultNotificationMetaIdentityAndPrincipalIsolation(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := store.NewFaultRepo()
	repo := store.NewFaultNotificationRepo()
	sched := scheduler.New(st, time.Millisecond)
	f := nudgeMustFault(t, ctx, st, "meta-principal", domain.FaultStatusActive)
	o := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, nil, 0, "fault-test", "fallback", "")
	o.deliveries = repo
	txfn := func(fn func(pgx.Tx) error) {
		t.Helper()
		if e := st.WithTx(ctx, fn); e != nil {
			t.Fatal(e)
		}
	}
	origins := []notify.SendRequest{}
	txfn(func(tx pgx.Tx) error {
		if _, e := repo.Enroll(ctx, tx, f.ID, 1); e != nil {
			return e
		}
		for i, target := range []string{"target-A", "target-B"} {
			d, _, e := repo.Ensure(ctx, tx, f.ID, 1, "original", store.NotificationTargetHash("original", target), "failed")
			if e != nil {
				return e
			}
			origins = append(origins, notify.SendRequest{FaultNotificationID: d.ID, FaultNotificationGeneration: 1 - i, GroupKey: faultActivatedGroupKey(f.ID), ChannelKind: "original", Target: target, Round: 3})
		}
		return nil
	})
	// 一旧epoch重试和一新epoch首投可能round相同；多个原目标也不能折叠同fallback的pending timer。
	for _, req := range origins {
		txfn(func(tx pgx.Tx) error {
			return scheduleNotifyFailureMetaAlert(ctx, tx, sched, faults, "fault-test", "fallback", time.Now(), req)
		})
	}
	var requests []notify.SendRequest
	txfn(func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT payload FROM timers WHERE kind='notify_send' AND status='pending' ORDER BY ref_id`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				return e
			}
			var req notify.SendRequest
			if e = json.Unmarshal(raw, &req); e != nil {
				return e
			}
			requests = append(requests, req)
		}
		return rows.Err()
	})
	if len(requests) != 2 {
		t.Fatalf("两个独立源意图fallback timer被折叠: %d", len(requests))
	}
	ch := &notificationTestChannel{}
	notify.Register(ch)
	sender := notify.NewSender(st, store.NewNotificationLogRepo(), store.NewTimelineRepo())
	sender.MaxAttempts = 1
	sender.BeforeFaultSend = o.BeforeSend
	sender.OnFaultResult = o.RecordResult
	sender.OnFinalFailure = func(context.Context, pgx.Tx, notify.SendRequest) error {
		t.Error("元告警不应递归调度fallback")
		return nil
	}
	for _, req := range requests {
		if req.FaultNotificationID != "" || req.MetaSourceDeliveryID == "" {
			t.Fatal("meta继承主意图身份或丢失源身份")
		}
		if e := sender.Send(ctx, req); e != nil {
			t.Fatal(e)
		}
	}
	for _, req := range requests {
		if e := sender.Send(ctx, req); e != nil {
			t.Fatal(e)
		}
	}
	if ch.calls.Load() != 2 {
		t.Fatalf("fallback发送次数=%d,want2", ch.calls.Load())
	}
	// 元告警本身失败也不能把原delivery改写为成功或再次调度。
	ch.fail = true
	failed := buildMetaAlertRequest(origins[0], "fault-test", "another-fallback", time.Now())
	if e := sender.Send(ctx, failed); e == nil {
		t.Fatal("元告警失败未传播")
	}
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, f.ID)
		if e != nil {
			return e
		}
		for _, d := range ds {
			if d.Status != "failed" {
				t.Fatalf("fallback反写主delivery: %+v", d)
			}
		}
		current, e := faults.GetByID(ctx, tx, f.ID)
		if !current.OwnerNotifiedAt.IsZero() {
			t.Fatal("fallback不能证明主通知已送达")
		}
		return e
	})
}
