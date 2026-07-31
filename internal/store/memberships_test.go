//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// 本文件是集成测试，setupTestStore/applyMigrations 复用 events_test.go 里已定义的
// 版本（同包、同 build tag），mustTeam 复用 config_repos_test.go 的版本。实测运行：
//   go test -tags=integration ./internal/store/... -run TestMembershipRepo -v

// mustUser 建一个用户，供依赖 user_id 外键的测试复用。
func mustUser(t *testing.T, ctx context.Context, st *Store, username string) *User {
	t.Helper()
	users := NewUserRepo()
	var u *User
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		u, err = users.Create(ctx, tx, User{Username: username, PasswordHash: "x", DisplayName: username})
		return err
	})
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", username, err)
	}
	return u
}

// TestMembershipRepo_AddListUpdateRemove 覆盖成员管理的完整 CRUD 生命周期
// （C1：ListByTeam/Add/UpdateRole/Remove），并验证 ListByTeam join users 的
// 字段正确性与按 username 排序。
func TestMembershipRepo_AddListUpdateRemove(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	alice := mustUser(t, ctx, st, "alice")
	bob := mustUser(t, ctx, st, "bob")
	memberships := NewMembershipRepo()

	// 故意先加 bob 再加 alice，验证 ListByTeam 按 username 排序而非插入顺序。
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		if err := memberships.Add(ctx, tx, team.ID, bob.ID, "member"); err != nil {
			return err
		}
		return memberships.Add(ctx, tx, team.ID, alice.ID, "admin")
	})
	if err != nil {
		t.Fatalf("添加团队成员失败: %v", err)
	}

	var list []Member
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		list, err = memberships.ListByTeam(ctx, tx, team.ID)
		return err
	})
	if err != nil {
		t.Fatalf("ListByTeam 失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望 2 名成员，got %d: %+v", len(list), list)
	}
	if list[0].Username != "alice" || list[0].UserID != alice.ID || list[0].DisplayName != "alice" || list[0].Role != "admin" {
		t.Fatalf("list[0] = %+v, want alice/admin", list[0])
	}
	if list[1].Username != "bob" || list[1].UserID != bob.ID || list[1].Role != "member" {
		t.Fatalf("list[1] = %+v, want bob/member", list[1])
	}

	// UpdateRole：bob 由 member 升为 admin。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return memberships.UpdateRole(ctx, tx, team.ID, bob.ID, "admin")
	})
	if err != nil {
		t.Fatalf("UpdateRole 失败: %v", err)
	}

	var adminCount int
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		adminCount, err = memberships.CountAdmins(ctx, tx, team.ID)
		return err
	})
	if err != nil {
		t.Fatalf("CountAdmins 失败: %v", err)
	}
	if adminCount != 2 {
		t.Fatalf("CountAdmins = %d, want 2（alice + 升级后的 bob）", adminCount)
	}

	// Remove：移除 bob 后 admin 数应回落到 1。
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return memberships.Remove(ctx, tx, team.ID, bob.ID)
	})
	if err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		adminCount, err = memberships.CountAdmins(ctx, tx, team.ID)
		return err
	})
	if err != nil {
		t.Fatalf("移除后 CountAdmins 失败: %v", err)
	}
	if adminCount != 1 {
		t.Fatalf("移除后 CountAdmins = %d, want 1", adminCount)
	}
}

// TestMembershipRepo_Add_DuplicateConflict 验证重复添加同一 (team, user) 返回可被
// API 层识别为 409 的 ErrMembershipConflict，而非笼统的底层约束错误。
func TestMembershipRepo_Add_DuplicateConflict(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	alice := mustUser(t, ctx, st, "alice")
	memberships := NewMembershipRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return memberships.Add(ctx, tx, team.ID, alice.ID, "member")
	})
	if err != nil {
		t.Fatalf("首次添加失败: %v", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return memberships.Add(ctx, tx, team.ID, alice.ID, "admin")
	})
	if !errors.Is(err, ErrMembershipConflict) {
		t.Fatalf("重复添加 err = %v, want ErrMembershipConflict", err)
	}
}

// TestMembershipRepo_UpdateRole_NotFound 验证操作不存在的成员返回 ErrNotFound，
// 与 TeamRepo/UserRepo 的 Update/Delete 保持同一「不存在」口径。
func TestMembershipRepo_UpdateRole_NotFound(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	alice := mustUser(t, ctx, st, "alice")
	memberships := NewMembershipRepo()

	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return memberships.UpdateRole(ctx, tx, team.ID, alice.ID, "admin")
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRole 对不存在成员 err = %v, want ErrNotFound", err)
	}

	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		return memberships.Remove(ctx, tx, team.ID, alice.ID)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove 对不存在成员 err = %v, want ErrNotFound", err)
	}
}

// TestMembershipRepo_CountAdmins_EmptyTeam 验证空团队的 CountAdmins 返回 0
// 而非报错（新建团队尚无成员是正常状态）。
func TestMembershipRepo_CountAdmins_EmptyTeam(t *testing.T) {
	st := setupTestStore(t)
	ctx := context.Background()
	team := mustTeam(t, ctx, st, "sre-core")
	memberships := NewMembershipRepo()

	var count int
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		count, err = memberships.CountAdmins(ctx, tx, team.ID)
		return err
	})
	if err != nil {
		t.Fatalf("CountAdmins 失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountAdmins = %d, want 0", count)
	}
}
