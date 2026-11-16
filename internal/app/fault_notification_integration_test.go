//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
	"alert-system/internal/scheduler"
	"alert-system/internal/store"
	"github.com/jackc/pgx/v5"
)

type notificationTestChannel struct {
	calls atomic.Int32
	fail  bool
}

func (c *notificationTestChannel) Kind() string { return "fault-test" }
func (c *notificationTestChannel) Send(_ context.Context, _ string, _ notify.Message) error {
	c.calls.Add(1)
	if c.fail {
		return errors.New("fake unavailable")
	}
	return nil
}
func TestFaultNotificationReliability(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := store.NewFaultRepo()
	repo := store.NewFaultNotificationRepo()
	sched := scheduler.New(st, time.Millisecond)
	o := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, nil, 0, "fault-test", "secret-target", "")
	o.deliveries = repo
	f := nudgeMustFault(t, ctx, st, "notify-active", domain.FaultStatusActive)
	txfn := func(fn func(pgx.Tx) error) {
		t.Helper()
		if e := st.WithTx(ctx, fn); e != nil {
			t.Fatal(e)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		txfn(func(tx pgx.Tx) error { return tx.QueryRow(ctx, query, args...).Scan(&n) })
		return n
	}
	// 预览置于真正的只读事务；不能创建 watch、timer 或时间线。
	txfn(func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); e != nil {
			return e
		}
		p, e := o.Preview(ctx, tx, f.ID, false)
		if e == nil && len(p.Targets) != 1 {
			t.Fatal(p)
		}
		return e
	})
	if n := count("SELECT count(*) FROM fault_notification_watches"); n != 0 {
		t.Fatal(n)
	}
	// 多实例同时提交，唯一意图与稳定 round 不依赖 pending timer 的防抖。
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- st.WithTx(ctx, func(tx pgx.Tx) error { _, e := o.Submit(ctx, tx, f.ID, false, "", "admin"); return e })
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if n := count("SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1", f.ID); n != 1 {
		t.Fatal(n)
	}
	if n := count("SELECT count(*) FROM fault_timeline_entries WHERE fault_id=$1 AND action='notify_backfill'", f.ID); n != 1 {
		t.Fatal(n)
	}
	var req notify.SendRequest
	txfn(func(tx pgx.Tx) error {
		var p []byte
		if e := tx.QueryRow(ctx, "SELECT payload FROM timers WHERE kind='notify_send' LIMIT 1").Scan(&p); e != nil {
			return e
		}
		return json.Unmarshal(p, &req)
	})
	if req.FaultNotificationID == "" {
		t.Fatal("missing durable identity")
	}
	ch := &notificationTestChannel{fail: true}
	notify.Register(ch)
	sender := notify.NewSender(st, store.NewNotificationLogRepo(), store.NewTimelineRepo())
	sender.MaxAttempts = 1
	sender.BeforeFaultSend = o.BeforeSend
	sender.OnFaultResult = o.RecordResult
	if e := sender.Send(ctx, req); e == nil {
		t.Fatal("failure must propagate")
	}
	txfn(func(tx pgx.Tx) error {
		current, e := faults.GetByID(ctx, tx, f.ID)
		if !current.OwnerNotifiedAt.IsZero() {
			t.Fatal("queued/failed counted as sent")
		}
		return e
	})
	// 已失败不被自动扫描无限重发；同一 payload 的重复调用也不重新外发。
	if e := sender.Send(ctx, req); e != nil {
		t.Fatal(e)
	}
	if ch.calls.Load() != 1 {
		t.Fatal(ch.calls.Load())
	}
	ch.fail = false
	txfn(func(tx pgx.Tx) error { _, e := o.Submit(ctx, tx, f.ID, true, "", "admin"); return e })
	txfn(func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, "SELECT payload FROM timers WHERE kind='notify_send'")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var p []byte
			if e = rows.Scan(&p); e != nil {
				return e
			}
			var candidate notify.SendRequest
			if e = json.Unmarshal(p, &candidate); e != nil {
				return e
			}
			if candidate.FaultNotificationGeneration == 1 {
				req = candidate
			}
		}
		return rows.Err()
	})
	if e := sender.Send(ctx, req); e != nil {
		t.Fatal(e)
	}
	txfn(func(tx pgx.Tx) error {
		f, e := faults.GetByID(ctx, tx, f.ID)
		if f.OwnerNotifiedAt.IsZero() {
			t.Fatal("sent missing")
		}
		return e
	})
	if e := sender.Send(ctx, req); e != nil {
		t.Fatal(e)
	}
	if ch.calls.Load() != 2 {
		t.Fatal("sent duplicated")
	}
	// 无目标先登记，补渠道后新进程实例对账补投；认领与恢复对象不自动补投。
	o.fallbackTarget = ""
	f2 := nudgeMustFault(t, ctx, st, "notify-no-target", domain.FaultStatusActive)
	txfn(func(tx pgx.Tx) error { return o.QueueFaultActivated(ctx, tx, f2) })
	o2 := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, nil, 0, "fault-test", "new-secret", "")
	o2.deliveries = repo
	o2.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	txfn(func(tx pgx.Tx) error { return o2.HandleReconcile(ctx, tx, "global", nil) })
	if n := count("SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1", f2.ID); n != 1 {
		t.Fatal("补渠道未恢复", n)
	}
	f3 := nudgeMustFault(t, ctx, st, "notify-acked", domain.FaultStatusActive)
	txfn(func(tx pgx.Tx) error {
		if e := o.QueueFaultActivated(ctx, tx, f3); e != nil {
			return e
		}
		_, _, e := faults.Ack(ctx, tx, f3.ID, "", "operator", time.Now())
		return e
	})
	txfn(func(tx pgx.Tx) error { return o2.HandleReconcile(ctx, tx, "global", nil) })
	if n := count("SELECT count(*) FROM fault_notification_deliveries WHERE fault_id=$1", f3.ID); n != 0 {
		t.Fatal("已认领不应补投")
	}
	// 结构down/up往返保留领域数据；ledger升级另由主Agent验收。
	for _, path := range []string{"../../migrations/000032_fault_notification_delivery.down.sql", "../../migrations/000032_fault_notification_delivery.up.sql"} {
		sql, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		txfn(func(tx pgx.Tx) error { _, e := tx.Exec(ctx, string(sql)); return e })
	}
	if n := count("SELECT count(*) FROM faults"); n != 3 {
		t.Fatal("migration changed domain data", n)
	}

}

func TestFaultNotificationCoverageAndCooldown(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	faults := store.NewFaultRepo()
	repo := store.NewFaultNotificationRepo()
	sched := scheduler.New(st, time.Millisecond)
	up := nudgeMustFault(t, ctx, st, "notify-provider", domain.FaultStatusActive)
	down := nudgeMustFault(t, ctx, st, "notify-consumer", domain.FaultStatusActive)
	providers := &fakeFaultOutboxProviders{providers: map[string][]string{"consumer": {"provider", "consumer"}}}
	// fake 拓扑仅注入节点遍历；真实 faults 查询仍走PG。
	txfn := func(fn func(pgx.Tx) error) {
		t.Helper()
		if e := st.WithTx(ctx, fn); e != nil {
			t.Fatal(e)
		}
	}
	txfn(func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO resource_nodes(id,source,external_ref,entity_key,kind,name,layer,last_observed_at) VALUES('11111111-1111-4111-8111-111111111111','test','provider','test:provider','resource','provider','infrastructure',now()),('22222222-2222-4222-8222-222222222222','test','consumer','test:consumer','resource','consumer','infrastructure',now())`)
		return e
	})
	// 用 canonical node UUID 填 provider 返回；包含root以验证不可自我抑制。
	providerID := "11111111-1111-4111-8111-111111111111"
	consumerID := "22222222-2222-4222-8222-222222222222"
	txfn(func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE faults SET resource_node_id=CASE WHEN id=$1 THEN $2::uuid ELSE $3::uuid END WHERE id IN ($1,$4)`, up.ID, providerID, consumerID, down.ID)
		return e
	})
	providers.providers = map[string][]string{consumerID: {providerID, consumerID}}
	o := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, providers, 0, "fault-test", "same-secret", "")
	o.deliveries = repo
	txfn(func(tx pgx.Tx) error {
		f, e := faults.GetByID(ctx, tx, up.ID)
		if e != nil {
			return e
		}
		return o.QueueFaultActivated(ctx, tx, f)
	})
	// 上游仅queued时不抑制下游。
	txfn(func(tx pgx.Tx) error {
		f, e := faults.GetByID(ctx, tx, down.ID)
		if e != nil {
			return e
		}
		return o.QueueFaultActivated(ctx, tx, f)
	})
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, down.ID)
		if e == nil && (len(ds) != 1 || ds[0].Status != "queued") {
			t.Fatal(ds)
		}
		return e
	})
	// 渠道接受后，只对相同target覆盖，另一目标仍需发送。
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, up.ID)
		if e != nil {
			return e
		}
		_, _, e = repo.Result(ctx, tx, ds[0].ID, 0, "sent")
		return e
	})
	down2 := nudgeMustFault(t, ctx, st, "notify-covered", domain.FaultStatusActive)
	txfn(func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE faults SET resource_node_id=$2 WHERE id=$1`, down2.ID, consumerID)
		return e
	})
	txfn(func(tx pgx.Tx) error {
		p, e := o.Preview(ctx, tx, down2.ID, false)
		if e == nil && p.Targets[0].Action != "covered" {
			t.Fatal(p)
		}
		if e != nil {
			return e
		}
		f, e := faults.GetByID(ctx, tx, down2.ID)
		if e != nil {
			return e
		}
		return o.QueueFaultActivated(ctx, tx, f)
	})
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, down2.ID)
		if e == nil && ds[0].Status != "covered" {
			t.Fatal(ds)
		}
		return e
	})
	o.fallbackTarget = "different-secret"
	txfn(func(tx pgx.Tx) error {
		f, e := faults.GetByID(ctx, tx, down2.ID)
		if e != nil {
			return e
		}
		return o.QueueFaultActivated(ctx, tx, f)
	})
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, down2.ID)
		if e == nil && len(ds) != 2 {
			t.Fatal(ds)
		}
		return e
	})
	// 冷却登记到期对账；同correlation key resolved保留历史、新一轮为新fault。
	old := nudgeMustFault(t, ctx, st, "notify-cooldown", domain.FaultStatusActive)
	txfn(func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE faults SET status='resolved',resolved_at=now() WHERE id=$1`, old.ID)
		return e
	})
	fresh := nudgeMustFault(t, ctx, st, "notify-cooldown", domain.FaultStatusActive)
	o.cooldown = time.Minute
	txfn(func(tx pgx.Tx) error { return o.QueueFaultActivated(ctx, tx, fresh) })
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, fresh.ID)
		if len(ds) != 0 {
			t.Fatal(ds)
		}
		return e
	})
	o.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	txfn(func(tx pgx.Tx) error { return o.HandleReconcile(ctx, tx, "global", nil) })
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, fresh.ID)
		if len(ds) != 1 {
			t.Fatal(ds)
		}
		return e
	})
	// 新激活epoch不能被上一轮成功覆盖，旧receipt/result也不能更新当前字段。
	var oldReq notify.SendRequest
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, up.ID)
		if e != nil {
			return e
		}
		oldReq = notify.SendRequest{FaultNotificationID: ds[0].ID}
		return repo.Activate(ctx, tx, up.ID, time.Now().UnixNano())
	})
	txfn(func(tx pgx.Tx) error {
		ok, e := o.BeforeSend(ctx, tx, oldReq)
		if ok {
			t.Fatal("old epoch allowed")
		}
		return e
	})
	txfn(func(tx pgx.Tx) error { return o.RecordResult(ctx, tx, oldReq, "sent") })
	txfn(func(tx pgx.Tx) error {
		current, e := faults.GetByID(ctx, tx, up.ID)
		if !current.OwnerNotifiedAt.IsZero() {
			t.Fatal("旧epoch更新当前送达时间")
		}
		return e
	})
	// 多目标部分成功：一条sent不掩盖另一条failed，状态与审计不输出目标原文。
	multi := nudgeMustFault(t, ctx, st, "notify-partial", domain.FaultStatusActive)
	const teamID = "33333333-3333-4333-8333-333333333333"
	txfn(func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `INSERT INTO teams(id,name)VALUES($1,'notify-partial-team')`, teamID); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `UPDATE faults SET owner_team_id=$2 WHERE id=$1`, multi.ID, teamID)
		return e
	})
	o.cooldown = 0
	o.ops = &fakeFaultOutboxOpsRepo{settings: map[string]*store.OpsSettings{teamID: {Channels: []store.OpsChannelTarget{{Kind: "fault-test", Target: "partial-secret-A"}, {Kind: "fault-test", Target: "partial-secret-B"}}}}}
	txfn(func(tx pgx.Tx) error {
		f, e := faults.GetByID(ctx, tx, multi.ID)
		if e != nil {
			return e
		}
		return o.QueueFaultActivated(ctx, tx, f)
	})
	var deliveries []store.FaultNotificationDelivery
	txfn(func(tx pgx.Tx) error { var e error; deliveries, e = repo.List(ctx, tx, multi.ID); return e })
	if len(deliveries) != 2 {
		t.Fatal(deliveries)
	}
	channel := &notificationTestChannel{}
	notify.Register(channel)
	sender := notify.NewSender(st, store.NewNotificationLogRepo(), store.NewTimelineRepo())
	sender.MaxAttempts = 1
	sender.BeforeFaultSend = o.BeforeSend
	sender.OnFaultResult = o.RecordResult
	for i, d := range deliveries {
		var req notify.SendRequest
		txfn(func(tx pgx.Tx) error {
			var raw []byte
			if e := tx.QueryRow(ctx, `SELECT payload FROM timers WHERE kind='notify_send' AND ref_id=$1`, "fault-delivery:"+d.ID+":0").Scan(&raw); e != nil {
				return e
			}
			return json.Unmarshal(raw, &req)
		})
		channel.fail = i == 1
		e := sender.Send(ctx, req)
		if (i == 1) != (e != nil) {
			t.Fatal(e)
		}
	}
	txfn(func(tx pgx.Tx) error {
		p, e := o.Preview(ctx, tx, multi.ID, false)
		if e != nil {
			return e
		}
		statuses := map[string]int{}
		for _, d := range p.Deliveries {
			statuses[d.Status]++
		}
		if statuses["sent"] != 1 || statuses["failed"] != 1 {
			t.Fatal(statuses)
		}
		raw, _ := json.Marshal(p)
		if strings.Contains(string(raw), "partial-secret") {
			t.Fatal("目标泄露")
		}
		return nil
	})

}

func TestFaultNotificationOrphanAndConcurrency(t *testing.T) {
	st := setupTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	faults := store.NewFaultRepo()
	repo := store.NewFaultNotificationRepo()
	sched := scheduler.New(st, time.Millisecond)
	o := NewFaultOutbox(sched, faults, &fakeFaultOutboxOpsRepo{}, nil, 0, "fault-test", "secret", "")
	o.deliveries = repo
	f := nudgeMustFault(t, ctx, st, "notify-race", domain.FaultStatusActive)
	txfn := func(fn func(pgx.Tx) error) {
		t.Helper()
		if e := st.WithTx(ctx, fn); e != nil {
			t.Fatal(e)
		}
	}
	txfn(func(tx pgx.Tx) error { return o.QueueFaultActivated(ctx, tx, f) })
	var req notify.SendRequest
	txfn(func(tx pgx.Tx) error {
		var p []byte
		if e := tx.QueryRow(ctx, `SELECT payload FROM timers WHERE kind='notify_send'`).Scan(&p); e != nil {
			return e
		}
		return json.Unmarshal(p, &req)
	})
	// 模拟已写sending但发送前placeholder失败，timer终止。对账释放为显式失败，不自动重投。
	txfn(func(tx pgx.Tx) error {
		ok, e := o.BeforeSend(ctx, tx, req)
		if !ok {
			t.Fatal("not claimed")
		}
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE timers SET status='failed' WHERE kind='notify_send'`)
		return e
	})
	o.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	txfn(func(tx pgx.Tx) error { return o.HandleReconcile(ctx, tx, "global", nil) })
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, f.ID)
		if ds[0].Status != "failed" {
			t.Fatal(ds)
		}
		return e
	})
	// 显式最多两轮补投，不能持续按按钮形成无限重试。
	for i := 0; i < 3; i++ {
		txfn(func(tx pgx.Tx) error { _, e := o.Submit(ctx, tx, f.ID, true, "", "admin"); return e })
		txfn(func(tx pgx.Tx) error {
			ds, e := repo.List(ctx, tx, f.ID)
			if e != nil {
				return e
			}
			_, _, e = repo.Result(ctx, tx, ds[0].ID, ds[0].Generation, "failed")
			return e
		})
	}
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, f.ID)
		if ds[0].Generation != 2 {
			t.Fatal(ds)
		}
		return e
	})
	// 故意竞争回写、Submit、对账，ctx deadline 会把锁顺序错误暴露为失败。
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- st.WithTx(ctx, func(tx pgx.Tx) error {
				switch n % 3 {
				case 0:
					return o.RecordResult(ctx, tx, req, "sent")
				case 1:
					_, e := o.Submit(ctx, tx, f.ID, false, "", "admin")
					return e
				default:
					return o.HandleReconcile(ctx, tx, "global", nil)
				}
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	// 一个坏watch不阻断其他Fault或周期续挂，错误原因可从状态端点读取。
	bad := nudgeMustFault(t, ctx, st, "notify-bad-item", domain.FaultStatusActive)
	good := nudgeMustFault(t, ctx, st, "notify-good-item", domain.FaultStatusActive)
	txfn(func(tx pgx.Tx) error {
		if _, e := repo.Enroll(ctx, tx, bad.ID, bad.FirstDetected.UnixNano()); e != nil {
			return e
		}
		_, e := repo.Enroll(ctx, tx, good.ID, good.FirstDetected.UnixNano())
		return e
	})
	o.faults = failingNotificationFaultRepo{faultOutboxFaultRepo: faults, badID: bad.ID}
	txfn(func(tx pgx.Tx) error { return o.HandleReconcile(ctx, tx, "global", nil) })
	txfn(func(tx pgx.Tx) error {
		reason, _, e := repo.Watch(ctx, tx, bad.ID)
		if reason != "reconcile_error" {
			t.Fatal(reason)
		}
		return e
	})
	txfn(func(tx pgx.Tx) error {
		ds, e := repo.List(ctx, tx, good.ID)
		if len(ds) != 1 {
			t.Fatal(ds)
		}
		return e
	})

}

type failingNotificationFaultRepo struct {
	faultOutboxFaultRepo
	badID string
}

func (r failingNotificationFaultRepo) GetByID(ctx context.Context, tx pgx.Tx, id string) (domain.Fault, error) {
	if id == r.badID {
		return domain.Fault{}, errors.New("fake per-item DB failure")
	}
	return r.faultOutboxFaultRepo.GetByID(ctx, tx, id)
}
