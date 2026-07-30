package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestTransition_FullMatrix 覆盖 R4.1 全转移矩阵：3 状态 x 3 状态共 9 种组合，
// 合法/非法全覆盖，非法组合断言可用 errors.Is/errors.As 判定。
func TestTransition_FullMatrix(t *testing.T) {
	statuses := []Status{StatusFiring, StatusAcknowledged, StatusResolved}
	legal := map[[2]Status]bool{
		{StatusFiring, StatusAcknowledged}:   true,
		{StatusFiring, StatusResolved}:       true,
		{StatusAcknowledged, StatusResolved}: true,
	}

	for _, from := range statuses {
		for _, to := range statuses {
			from, to := from, to
			wantLegal := legal[[2]Status{from, to}]
			t.Run(fmt.Sprintf("%s->%s", from, to), func(t *testing.T) {
				err := Transition(from, to)
				if wantLegal {
					if err != nil {
						t.Fatalf("Transition(%s, %s) = %v, want nil", from, to, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("Transition(%s, %s) = nil, want error", from, to)
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("Transition(%s, %s) error = %v, want errors.Is(ErrInvalidTransition)", from, to, err)
				}
				var te *TransitionError
				if !errors.As(err, &te) {
					t.Fatalf("Transition(%s, %s) error type = %T, want *TransitionError", from, to, err)
				}
				if te.From != from || te.To != to {
					t.Fatalf("TransitionError = {From:%s To:%s}, want {From:%s To:%s}", te.From, te.To, from, to)
				}
			})
		}
	}
}

func TestAck_LegalTransition(t *testing.T) {
	starts := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	now := starts.Add(3 * time.Minute)
	alert := Alert{ID: "a1", Status: StatusFiring, StartsAt: starts}

	updated, entry, err := Ack(alert, "u-1", "alice", now)
	if err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if updated.Status != StatusAcknowledged {
		t.Errorf("Status = %s, want %s", updated.Status, StatusAcknowledged)
	}
	if updated.AckedBy != "u-1" || !updated.AckedAt.Equal(now) {
		t.Errorf("AckedBy/AckedAt = %s/%v, want u-1/%v", updated.AckedBy, updated.AckedAt, now)
	}
	if entry.AlertID != "a1" || entry.Actor != "alice" || entry.Action != TimelineActionAck || !entry.At.Equal(now) {
		t.Errorf("unexpected timeline entry: %+v", entry)
	}
	rt, ok := entry.Detail["response_time_seconds"].(float64)
	if !ok || rt != 180 {
		t.Errorf("response_time_seconds = %v, want 180", entry.Detail["response_time_seconds"])
	}

	// 纯函数：入参不应被修改。
	if alert.Status != StatusFiring || !alert.AckedAt.IsZero() {
		t.Errorf("input alert was mutated: %+v", alert)
	}
}

func TestAck_IllegalTransition(t *testing.T) {
	for _, from := range []Status{StatusAcknowledged, StatusResolved} {
		alert := Alert{ID: "a1", Status: from}
		_, _, err := Ack(alert, "u-1", "alice", time.Now())
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("Ack() from %s: error = %v, want ErrInvalidTransition", from, err)
		}
	}
}

func TestAck_EmptyActorName(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusFiring}
	if _, _, err := Ack(alert, "u-1", "", time.Now()); err == nil {
		t.Error("Ack() with empty actorName: error = nil, want error")
	}
}

// 外部渠道（如飞书卡片）的操作者若无法映射到内部 users 记录，actorID 允许为空——
// store 层用 NULLIF 把空字符串落成 NULL，不报 UUID 类型错误。
func TestAck_EmptyActorID_Allowed(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusFiring}
	updated, entry, err := Ack(alert, "", "ou_lark_user", time.Now())
	if err != nil {
		t.Fatalf("Ack() error = %v, want nil（actorID 允许为空）", err)
	}
	if updated.AckedBy != "" {
		t.Errorf("AckedBy = %q, want 空字符串", updated.AckedBy)
	}
	if entry.Actor != "ou_lark_user" {
		t.Errorf("entry.Actor = %q, want ou_lark_user", entry.Actor)
	}
}

func TestResolve_LegalTransitions(t *testing.T) {
	now := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	for _, from := range []Status{StatusFiring, StatusAcknowledged} {
		alert := Alert{ID: "a1", Status: from}
		updated, entry, err := Resolve(alert, "bob", ResolveReasonManual, now)
		if err != nil {
			t.Fatalf("Resolve() from %s: error = %v", from, err)
		}
		if updated.Status != StatusResolved {
			t.Errorf("Status = %s, want resolved", updated.Status)
		}
		if !updated.EndsAt.Equal(now) {
			t.Errorf("EndsAt = %v, want %v", updated.EndsAt, now)
		}
		if entry.Action != TimelineActionResolve || entry.Actor != "bob" {
			t.Errorf("unexpected timeline entry: %+v", entry)
		}
		if entry.Detail["reason"] != "manual" {
			t.Errorf("Detail[reason] = %v, want manual", entry.Detail["reason"])
		}
		if _, hasLabel := entry.Detail["label"]; hasLabel {
			t.Errorf("manual resolve 不应带 label，实际 = %v", entry.Detail["label"])
		}
	}
}

func TestResolve_AlreadyResolved(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusResolved}
	_, _, err := Resolve(alert, "bob", ResolveReasonManual, time.Now())
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("error = %v, want ErrInvalidTransition", err)
	}
}

func TestResolve_ReasonLabels(t *testing.T) {
	tests := []struct {
		reason    ResolveReason
		wantLabel string
	}{
		{ResolveReasonTTL, "TTL 自动恢复"},
		{ResolveReasonAutoRemediation, "自愈恢复"},
	}
	for _, tt := range tests {
		alert := Alert{ID: "a1", Status: StatusFiring}
		_, entry, err := Resolve(alert, "system", tt.reason, time.Now())
		if err != nil {
			t.Fatalf("Resolve() reason=%s: error = %v", tt.reason, err)
		}
		if entry.Detail["label"] != tt.wantLabel {
			t.Errorf("reason=%s: Detail[label] = %v, want %q", tt.reason, entry.Detail["label"], tt.wantLabel)
		}
	}
}

func TestResolve_SourceReasonHasNoLabel(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusFiring}
	_, entry, err := Resolve(alert, "alertmanager", ResolveReasonSource, time.Now())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if _, hasLabel := entry.Detail["label"]; hasLabel {
		t.Errorf("source resolve 不应带 label，实际 = %v", entry.Detail["label"])
	}
}

func TestResolve_InvalidReason(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusFiring}
	if _, _, err := Resolve(alert, "bob", ResolveReason("bogus"), time.Now()); err == nil {
		t.Error("Resolve() with invalid reason: error = nil, want error")
	}
}

func TestResolve_EmptyActor(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusFiring}
	if _, _, err := Resolve(alert, "", ResolveReasonManual, time.Now()); err == nil {
		t.Error("Resolve() with empty actor: error = nil, want error")
	}
}

func TestHandoff_DoesNotChangeStatus(t *testing.T) {
	for _, status := range []Status{StatusFiring, StatusAcknowledged} {
		alert := Alert{ID: "a1", Status: status, AckedBy: "alice"}
		now := time.Now()
		updated, entry, err := Handoff(alert, "alice", "bob", now)
		if err != nil {
			t.Fatalf("Handoff() from %s: error = %v", status, err)
		}
		if updated.Status != status {
			t.Errorf("Status changed: got %s, want unchanged %s", updated.Status, status)
		}
		if updated.AckedBy != "bob" {
			t.Errorf("AckedBy = %s, want bob", updated.AckedBy)
		}
		if entry.Action != TimelineActionHandoff || entry.Detail["from"] != "alice" || entry.Detail["to"] != "bob" {
			t.Errorf("unexpected timeline entry: %+v", entry)
		}

		// 纯函数：入参不应被修改。
		if alert.AckedBy != "alice" {
			t.Errorf("input alert was mutated: AckedBy = %s", alert.AckedBy)
		}
	}
}

func TestHandoff_ResolvedAlertRejected(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusResolved}
	_, _, err := Handoff(alert, "alice", "bob", time.Now())
	if !errors.Is(err, ErrAlertResolved) {
		t.Errorf("error = %v, want ErrAlertResolved", err)
	}
}

func TestHandoff_EmptyActorOrTarget(t *testing.T) {
	alert := Alert{ID: "a1", Status: StatusFiring}
	if _, _, err := Handoff(alert, "", "bob", time.Now()); err == nil {
		t.Error("Handoff() with empty actor: error = nil, want error")
	}
	if _, _, err := Handoff(alert, "alice", "", time.Now()); err == nil {
		t.Error("Handoff() with empty target: error = nil, want error")
	}
}
