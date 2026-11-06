package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestImpactStateIsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state ImpactState
		want  bool
	}{
		{name: "potential", state: ImpactStatePotential, want: true},
		{name: "confirmed", state: ImpactStateConfirmed, want: true},
		{name: "ruled out", state: ImpactStateRuledOut, want: true},
		{name: "unknown", state: ImpactState("likely"), want: false},
		{name: "empty", state: ImpactState(""), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.state.IsValid(); got != test.want {
				t.Fatalf("ImpactState(%q).IsValid() = %v, want %v", test.state, got, test.want)
			}
		})
	}
}

// TestAckFault 覆盖 R4.2/R4.3 的幂等矩阵：首次 ack 写入三个字段并产出时间线条目；
// 重复 ack 短路（不变量 6 口径，不重写、不报错）；空 actorName 必须被拒绝
// （时间线不能有一条不知道是谁做的 ack 记录，对齐不变量 7 的 domain.Ack 校验）。
func TestAckFault(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	base := Fault{ID: "fault-1", OwnerTeamID: "team-1"}

	t.Run("首次认领", func(t *testing.T) {
		t.Parallel()
		updated, entry, ok, err := AckFault(base, "user-1", "张三", now)
		if err != nil {
			t.Fatalf("AckFault() error = %v", err)
		}
		if !ok {
			t.Fatalf("AckFault() ok = false, want true")
		}
		if updated.AckedBy != "user-1" || updated.AckedByName != "张三" || !updated.AckedAt.Equal(now) {
			t.Fatalf("AckFault() updated = %+v, want acked_by/name/at 写入", updated)
		}
		// 认领不改变 owner（R3.4）。
		if updated.OwnerTeamID != base.OwnerTeamID {
			t.Fatalf("AckFault() OwnerTeamID = %q, want unchanged %q", updated.OwnerTeamID, base.OwnerTeamID)
		}
		if entry.FaultID != base.ID || entry.ActorID != "user-1" || entry.Actor != "张三" || entry.Action != FaultTimelineActionAck || !entry.At.Equal(now) {
			t.Fatalf("AckFault() entry = %+v, want 对应 fault/actor/action/at", entry)
		}
	})

	t.Run("重复认领短路", func(t *testing.T) {
		t.Parallel()
		acked := base
		acked.AckedBy = "user-1"
		acked.AckedByName = "张三"
		acked.AckedAt = now

		later := now.Add(time.Hour)
		updated, entry, ok, err := AckFault(acked, "user-2", "李四", later)
		if err != nil {
			t.Fatalf("AckFault() error = %v", err)
		}
		if ok {
			t.Fatalf("AckFault() ok = true, want false（已认领应短路）")
		}
		if !reflect.DeepEqual(updated, acked) {
			t.Fatalf("AckFault() 短路时不应改写入参，got %+v, want %+v", updated, acked)
		}
		if !reflect.DeepEqual(entry, FaultTimelineEntry{}) {
			t.Fatalf("AckFault() 短路时不应产出时间线条目，got %+v", entry)
		}
	})

	t.Run("空 actorName 被拒绝", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"", "   "} {
			_, _, ok, err := AckFault(base, "user-1", name, now)
			if !errors.Is(err, ErrAckActorNameRequired) {
				t.Fatalf("AckFault(actorName=%q) error = %v, want ErrAckActorNameRequired", name, err)
			}
			if ok {
				t.Fatalf("AckFault(actorName=%q) ok = true, want false", name)
			}
		}
	})
}
