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
//   go test -tags=integration ./internal/store/... -run 'TestRoute|TestSilence|TestChangeEvent|TestNotificationPolicy|TestNotificationLog|TestShift|TestEscalationPolicy' -v

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

func TestIntegrationRepo_RotateHookIDHashAndEnforceUniqueHookIdentity(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	integrations := NewIntegrationRepo()

	var source *Integration
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var createErr error
		source, createErr = integrations.Create(ctx, tx, Integration{
			Kind: "alertmanager", Name: "managed-hook-source", HookIDHash: "old-hook-hash",
			DefaultSeverity: string(domain.SeverityWarning), Enabled: true,
		})
		return createErr
	})
	if err != nil {
		t.Fatalf("创建 integration 失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return integrations.RotateHookIDHash(ctx, tx, source.ID, "new-hook-hash")
	})
	if err != nil {
		t.Fatalf("轮换 hook hash 失败: %v", err)
	}
	var got *Integration
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		got, getErr = integrations.GetByID(ctx, tx, source.ID)
		return getErr
	})
	if err != nil || got.HookIDHash != "new-hook-hash" {
		t.Fatalf("轮换后 HookIDHash=%q err=%v, want new-hook-hash", got.HookIDHash, err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, createErr := integrations.Create(ctx, tx, Integration{
			Kind: "alertmanager", Name: "duplicate-hook-source", HookIDHash: "new-hook-hash",
			DefaultSeverity: string(domain.SeverityWarning), Enabled: true,
		})
		return createErr
	})
	if err == nil {
		t.Fatal("重复 hook_id_hash 应被 integrations_hook_id_hash_uidx 拒绝")
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

// TestEscalationPolicyRepo_SeverityConfig_DefaultZeroValue 验证默认 '{}' 反序列化
// 后是零值（Overrides/Skip 均 nil），确保存量策略不会被误读出空 map/slice（R1）。
func TestEscalationPolicyRepo_SeverityConfig_DefaultZeroValue(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	policies := NewEscalationPolicyRepo()

	var created *EscalationPolicy
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = policies.Create(ctx, tx, EscalationPolicy{
			TeamID: team.ID,
			Levels: []EscalationLevel{{WaitSeconds: 300, Targets: []string{"user:alice"}}},
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建升级策略失败: %v", err)
	}
	if created.SeverityConfig.Overrides != nil || created.SeverityConfig.Skip != nil {
		t.Fatalf("默认 SeverityConfig = %+v, want 零值（Overrides/Skip 均 nil）", created.SeverityConfig)
	}

	var got *EscalationPolicy
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = policies.GetByID(ctx, tx, created.ID)
		return err
	})
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if got.SeverityConfig.Overrides != nil || got.SeverityConfig.Skip != nil {
		t.Fatalf("GetByID 读回 SeverityConfig = %+v, want 零值", got.SeverityConfig)
	}
}

// TestEscalationPolicyRepo_SeverityConfig_RoundTrip 验证带 Overrides + Skip 的完整
// severity_config 经 Create -> GetByID 逐字段往返一致（R1）。
func TestEscalationPolicyRepo_SeverityConfig_RoundTrip(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	policies := NewEscalationPolicyRepo()

	cfg := SeverityConfig{
		Overrides: map[string][]int{"critical": {60, 180}},
		Skip:      []string{"info"},
	}

	var created *EscalationPolicy
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = policies.Create(ctx, tx, EscalationPolicy{
			TeamID:         team.ID,
			Levels:         []EscalationLevel{{WaitSeconds: 300, Targets: []string{"user:alice"}}},
			SeverityConfig: cfg,
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建升级策略失败: %v", err)
	}

	var got *EscalationPolicy
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = policies.GetByID(ctx, tx, created.ID)
		return err
	})
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if len(got.SeverityConfig.Overrides["critical"]) != 2 || got.SeverityConfig.Overrides["critical"][0] != 60 || got.SeverityConfig.Overrides["critical"][1] != 180 {
		t.Fatalf("got.SeverityConfig.Overrides = %+v, want critical: [60 180]", got.SeverityConfig.Overrides)
	}
	if len(got.SeverityConfig.Skip) != 1 || got.SeverityConfig.Skip[0] != "info" {
		t.Fatalf("got.SeverityConfig.Skip = %+v, want [info]", got.SeverityConfig.Skip)
	}
}

// TestEscalationPolicyRepo_Update_SeverityConfig 验证 Update 修改 severity_config
// 后读回一致（覆盖 severity_config 列而非仅 levels/team_id）。
func TestEscalationPolicyRepo_Update_SeverityConfig(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	policies := NewEscalationPolicyRepo()

	var created *EscalationPolicy
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = policies.Create(ctx, tx, EscalationPolicy{
			TeamID: team.ID,
			Levels: []EscalationLevel{{WaitSeconds: 300, Targets: []string{"user:alice"}}},
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建升级策略失败: %v", err)
	}

	updated := *created
	updated.SeverityConfig = SeverityConfig{
		Overrides: map[string][]int{"warning": {600}},
		Skip:      []string{"info", "debug"},
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return policies.Update(ctx, tx, updated)
	})
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	var got *EscalationPolicy
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = policies.GetByID(ctx, tx, created.ID)
		return err
	})
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if len(got.SeverityConfig.Overrides["warning"]) != 1 || got.SeverityConfig.Overrides["warning"][0] != 600 {
		t.Fatalf("got.SeverityConfig.Overrides = %+v, want warning: [600]", got.SeverityConfig.Overrides)
	}
	if len(got.SeverityConfig.Skip) != 2 || got.SeverityConfig.Skip[0] != "info" || got.SeverityConfig.Skip[1] != "debug" {
		t.Fatalf("got.SeverityConfig.Skip = %+v, want [info debug]", got.SeverityConfig.Skip)
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

// TestShiftRepo_GetByID_Update_RoundTrip 验证 GetByID/Update 往返，以及不存在的 id
// 走既有仓储 ErrNotFound 口径（供编辑班次前置校验复用）。
func TestShiftRepo_GetByID_Update_RoundTrip(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")

	var scheduleID string
	var alice, bob *User
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		schedules := NewScheduleRepo()
		sched, err := schedules.Create(ctx, tx, Schedule{TeamID: team.ID, Name: "primary", Timezone: "UTC"})
		if err != nil {
			return err
		}
		scheduleID = sched.ID

		users := NewUserRepo()
		alice, err = users.Create(ctx, tx, User{Username: "alice", PasswordHash: "x", DisplayName: "Alice"})
		if err != nil {
			return err
		}
		bob, err = users.Create(ctx, tx, User{Username: "bob", PasswordHash: "x", DisplayName: "Bob"})
		return err
	})
	if err != nil {
		t.Fatalf("准备值班数据失败: %v", err)
	}

	shifts := NewShiftRepo()
	var created *Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = shifts.Create(ctx, tx, Shift{
			ScheduleID: scheduleID,
			UserID:     alice.ID,
			StartsAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			EndsAt:     time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		})
		return err
	})
	if err != nil {
		t.Fatalf("创建班次失败: %v", err)
	}

	var got *Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = shifts.GetByID(ctx, tx, created.ID)
		return err
	})
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if got.UserID != alice.ID || !got.StartsAt.Equal(created.StartsAt) || !got.EndsAt.Equal(created.EndsAt) {
		t.Fatalf("GetByID = %+v, want 与 created 一致", got)
	}

	updated := *created
	updated.UserID = bob.ID
	updated.StartsAt = time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC)
	updated.EndsAt = time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC)
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return shifts.Update(ctx, tx, updated)
	})
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = shifts.GetByID(ctx, tx, created.ID)
		return err
	})
	if err != nil {
		t.Fatalf("Update 后 GetByID 失败: %v", err)
	}
	if got.UserID != bob.ID || !got.StartsAt.Equal(updated.StartsAt) || !got.EndsAt.Equal(updated.EndsAt) {
		t.Fatalf("Update 后 GetByID = %+v, want 与 updated 一致", got)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := shifts.GetByID(ctx, tx, "00000000-0000-0000-0000-000000000000")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(不存在 id) err = %v, want ErrNotFound", err)
	}
}

// TestShiftRepo_ListOverlapping 验证 ListOverlapping 的半开区间重叠判定：严格相邻不
// 命中、任意交叠（含完全包含/被包含）命中、excludeID 排除自身供编辑时跳过被编辑的班次。
func TestShiftRepo_ListOverlapping(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")

	var scheduleID, userID string
	var base *Shift // 基准班次：2026-01-02 00:00 ~ 2026-01-03 00:00
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
		base, err = shifts.Create(ctx, tx, Shift{
			ScheduleID: scheduleID,
			UserID:     userID,
			StartsAt:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			EndsAt:     time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		})
		return err
	})
	if err != nil {
		t.Fatalf("准备值班数据失败: %v", err)
	}

	shifts := NewShiftRepo()

	// 严格相邻：待判定区间 [2026-01-01 00:00, 2026-01-02 00:00) 的 to 恰好等于
	// base.StartsAt，不算重叠（允许无缝衔接排班）。
	var adjacentBefore []Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		adjacentBefore, err = shifts.ListOverlapping(ctx, tx, scheduleID,
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), base.StartsAt, "")
		return err
	})
	if err != nil {
		t.Fatalf("ListOverlapping(严格相邻-前) 失败: %v", err)
	}
	if len(adjacentBefore) != 0 {
		t.Fatalf("ListOverlapping(严格相邻-前) = %+v, want 空", adjacentBefore)
	}

	// 严格相邻：待判定区间 [2026-01-03 00:00, 2026-01-04 00:00) 的 from 恰好等于
	// base.EndsAt，不算重叠。
	var adjacentAfter []Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		adjacentAfter, err = shifts.ListOverlapping(ctx, tx, scheduleID,
			base.EndsAt, time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC), "")
		return err
	})
	if err != nil {
		t.Fatalf("ListOverlapping(严格相邻-后) 失败: %v", err)
	}
	if len(adjacentAfter) != 0 {
		t.Fatalf("ListOverlapping(严格相邻-后) = %+v, want 空", adjacentAfter)
	}

	// 任意交叠：完全包含 base 的更大区间。
	var containing []Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		containing, err = shifts.ListOverlapping(ctx, tx, scheduleID,
			time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC), "")
		return err
	})
	if err != nil {
		t.Fatalf("ListOverlapping(完全包含 base) 失败: %v", err)
	}
	if len(containing) != 1 || containing[0].ID != base.ID {
		t.Fatalf("ListOverlapping(完全包含 base) = %+v, want 恰好命中 base", containing)
	}

	// 任意交叠：被 base 完全包含的更小区间。
	var contained []Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		contained, err = shifts.ListOverlapping(ctx, tx, scheduleID,
			time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 18, 0, 0, 0, time.UTC), "")
		return err
	})
	if err != nil {
		t.Fatalf("ListOverlapping(被 base 包含) 失败: %v", err)
	}
	if len(contained) != 1 || contained[0].ID != base.ID {
		t.Fatalf("ListOverlapping(被 base 包含) = %+v, want 恰好命中 base", contained)
	}

	// excludeID 排除自身：编辑 base 本身时传入其 id，即便区间与自己重叠也不应命中。
	var excludingSelf []Shift
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		excludingSelf, err = shifts.ListOverlapping(ctx, tx, scheduleID,
			time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 18, 0, 0, 0, time.UTC), base.ID)
		return err
	})
	if err != nil {
		t.Fatalf("ListOverlapping(excludeID) 失败: %v", err)
	}
	if len(excludingSelf) != 0 {
		t.Fatalf("ListOverlapping(excludeID=base.ID) = %+v, want 空", excludingSelf)
	}
}
