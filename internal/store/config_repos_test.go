//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// 本文件是集成测试，setupTestStore/applyMigrations 复用 events_test.go 里已定义的
// 版本（同包、同 build tag），无需重复实现。silences/change_events/notification_log
// 的测试见 silences_test.go（拆分对应 config_repos*.go / silences.go 的生产代码分层）。
// 实测运行：
//   go test -tags=integration ./internal/store/... -run 'TestRoute|TestSilence|TestChangeEvent|TestNotificationPolicy|TestNotificationLog|TestShift' -v

// mustTeam 建一个团队，供依赖 team_id 外键的测试复用。
func mustTeam(t *testing.T, ctx context.Context, st *Store, name string) *Team {
	t.Helper()
	teams := NewTeamRepo()
	var team *Team
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		team, err = teams.Create(ctx, tx, name)
		return err
	})
	if err != nil {
		t.Fatalf("创建团队 %s 失败: %v", name, err)
	}
	return team
}

// TestRouteRepo_ListOrdered_ByPriority 验证 ListOrdered 按 priority 升序返回，
// 对应路由 stage 需要的「按优先级有序匹配」（R5.1）。
func TestRouteRepo_ListOrdered_ByPriority(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	routes := NewRouteRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		for _, p := range []int{30, 10, 20} {
			if _, err := routes.Create(ctx, tx, Route{
				Priority:     p,
				Matchers:     domain.MatcherSet{{Name: "service.name", Op: domain.MatchEqual, Value: "checkout"}},
				TargetTeamID: team.ID,
				GroupBy:      []string{"host.name"},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("创建路由失败: %v", err)
	}

	var list []Route
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		list, err = routes.ListOrdered(ctx, tx, 0)
		return err
	})
	if err != nil {
		t.Fatalf("ListOrdered 失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("期望 3 条路由，got %d", len(list))
	}
	for i, want := range []int{10, 20, 30} {
		if list[i].Priority != want {
			t.Fatalf("list[%d].Priority = %d, want %d（顺序: %v）", i, list[i].Priority, want, list)
		}
	}
}

// TestRouteRepo_Create_PriorityConflict 验证 priority 唯一约束冲突返回可判别的
// ErrRoutePriorityConflict，而不是裸的驱动层错误。
func TestRouteRepo_Create_PriorityConflict(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	routes := NewRouteRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := routes.Create(ctx, tx, Route{Priority: 1, TargetTeamID: team.ID})
		return err
	})
	if err != nil {
		t.Fatalf("首次创建路由失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := routes.Create(ctx, tx, Route{Priority: 1, TargetTeamID: team.ID})
		return err
	})
	if !errors.Is(err, ErrRoutePriorityConflict) {
		t.Fatalf("err = %v, want ErrRoutePriorityConflict", err)
	}
}

// TestNotificationPolicyRepo_GetGlobalDefault 验证全局默认查询只取
// team_id IS NULL 且 matchers 为空的那一条，不会被同为 team_id IS NULL 但携带
// matchers 的「全局范围业务绑定策略」干扰。
func TestNotificationPolicyRepo_GetGlobalDefault(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	policies := NewNotificationPolicyRepo()

	steps := NotificationSteps{
		domain.SeverityWarning: {
			{WaitSeconds: 0, Channels: []string{"email"}, Mode: "replace"},
			{WaitSeconds: 300, Channels: []string{"im"}, Mode: "replace"},
		},
		domain.SeverityCritical: {
			{WaitSeconds: 0, Channels: []string{"im"}, Mode: "replace"},
			{WaitSeconds: 180, Channels: []string{"phone"}, Mode: "replace"},
		},
	}

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		// 全局范围但绑定了 matchers 的策略：不应被 GetGlobalDefault 选中。
		if _, err := policies.Create(ctx, tx, NotificationPolicy{
			Name:     "global-scoped-checkout",
			Matchers: domain.MatcherSet{{Name: "service.name", Op: domain.MatchEqual, Value: "checkout"}},
			Steps:    steps,
		}); err != nil {
			return err
		}
		// 真正的全局默认：team_id 与 matchers 均为空（Matchers 留空，验证 nil -> [] 的
		// 归一化在 NotificationPolicyRepo.Create 里生效，而不是靠调用方显式传空切片）。
		_, err := policies.Create(ctx, tx, NotificationPolicy{
			Name:  "global-default",
			Steps: steps,
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建通知策略失败: %v", err)
	}

	var got *NotificationPolicy
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = policies.GetGlobalDefault(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("GetGlobalDefault 失败: %v", err)
	}
	if got.Name != "global-default" {
		t.Fatalf("got.Name = %q, want %q", got.Name, "global-default")
	}
	if got.TeamID != "" {
		t.Fatalf("got.TeamID = %q, want 空字符串", got.TeamID)
	}

	// R13.1 样例：warning: email -> 5min -> im；critical: im -> 3min -> phone。
	warningSteps := got.Steps[domain.SeverityWarning]
	if len(warningSteps) != 2 || warningSteps[1].WaitSeconds != 300 || warningSteps[1].Channels[0] != "im" {
		t.Fatalf("warning steps 不符合样例: %+v", warningSteps)
	}
	criticalSteps := got.Steps[domain.SeverityCritical]
	if len(criticalSteps) != 2 || criticalSteps[1].WaitSeconds != 180 || criticalSteps[1].Channels[0] != "phone" {
		t.Fatalf("critical steps 不符合样例: %+v", criticalSteps)
	}
}

// TestNotificationPolicyRepo_ListByTeam 验证团队绑定策略的查询只返回该团队的记录。
func TestNotificationPolicyRepo_ListByTeam(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	teamA := mustTeam(t, ctx, st, "team-a")
	teamB := mustTeam(t, ctx, st, "team-b")
	policies := NewNotificationPolicyRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := policies.Create(ctx, tx, NotificationPolicy{Name: "a-default", TeamID: teamA.ID, Steps: NotificationSteps{}}); err != nil {
			return err
		}
		_, err := policies.Create(ctx, tx, NotificationPolicy{Name: "b-default", TeamID: teamB.ID, Steps: NotificationSteps{}})
		return err
	})
	if err != nil {
		t.Fatalf("创建通知策略失败: %v", err)
	}

	var list []NotificationPolicy
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		list, err = policies.ListByTeam(ctx, tx, teamA.ID, 0)
		return err
	})
	if err != nil {
		t.Fatalf("ListByTeam 失败: %v", err)
	}
	if len(list) != 1 || list[0].Name != "a-default" {
		t.Fatalf("ListByTeam(teamA) = %+v, want 只含 a-default", list)
	}
}

// TestShiftRepo_ListInWindow 验证「某时刻谁在值班」所依赖的区间求交查询（R5.5）。
func TestShiftRepo_ListInWindow(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")

	var scheduleID, userID string
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		schedules := NewScheduleRepo()
		sched, err := schedules.Create(ctx, tx, Schedule{TeamID: team.ID, Name: "primary", Timezone: "UTC"})
		if err != nil {
			return err
		}
		scheduleID = sched.ID

		users := NewUserRepo()
		u, err := users.Create(ctx, tx, User{Username: "alice", PasswordHash: "x", DisplayName: "Alice"})
		if err != nil {
			return err
		}
		userID = u.ID

		shifts := NewShiftRepo()
		_, err = shifts.Create(ctx, tx, Shift{
			ScheduleID: scheduleID,
			UserID:     userID,
			StartsAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			EndsAt:     time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		})
		return err
	})
	if err != nil {
		t.Fatalf("准备值班数据失败: %v", err)
	}

	shifts := NewShiftRepo()
	var covering []Shift
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		covering, err = shifts.ListInWindow(ctx, tx, scheduleID, at, at.Add(time.Nanosecond), 0)
		return err
	})
	if err != nil {
		t.Fatalf("ListInWindow 失败: %v", err)
	}
	if len(covering) != 1 || covering[0].UserID != userID {
		t.Fatalf("覆盖 %v 的班次 = %+v, want 恰好 1 条属于 %s", at, covering, userID)
	}
}
