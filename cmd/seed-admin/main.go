// Command seed-admin 是初始管理员引导工具：系统没有注册入口，users.password_hash
// 是 bcrypt 哈希（无法手写 SQL 造账号），本命令一次性把「team + admin 用户 +
// admin 角色 membership」建齐，供本地/首次部署引导登录使用。
//
// 幂等：team 按 name 已存在则复用；user 按 username 已存在则重置密码（bcrypt 重
// 哈希）并确保 enabled=true；membership 确保该 user 在该 team 下是 admin 角色
// （已是 admin 则原样保留，是 member 则升级）。可重复执行，例如密码遗忘后重跑本
// 命令即可重置。
//
// 这是一次性运维工具，直接用 pgx 裸连接执行 SQL，不接入 internal/store 仓储层。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const defaultDSN = "postgres://alert_system:alert_system@localhost:5433/alert_system?sslmode=disable"

func main() {
	dsnDefault := os.Getenv("DB_DSN")
	if dsnDefault == "" {
		dsnDefault = defaultDSN
	}

	dsn := flag.String("dsn", dsnDefault, "Postgres DSN（默认取环境变量 DB_DSN，否则用本地 compose 默认值）")
	username := flag.String("username", "admin", "管理员用户名")
	password := flag.String("password", "", "管理员密码（必填，明文只用于本次哈希，不落盘不打印）")
	team := flag.String("team", "default", "所属团队名（不存在则创建）")
	displayName := flag.String("display-name", "", "显示名（默认同 -username）")
	flag.Parse()

	if *password == "" {
		fmt.Fprintln(os.Stderr, "错误: -password 不能为空")
		fmt.Fprintln(os.Stderr, "用法: go run ./cmd/seed-admin -password <密码> [-username admin] [-team default] [-dsn ...]")
		os.Exit(1)
	}
	if *displayName == "" {
		*displayName = *username
	}

	if err := run(context.Background(), *dsn, *username, *password, *team, *displayName); err != nil {
		fmt.Fprintf(os.Stderr, "seed-admin 失败: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn, username, password, team, displayName string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("生成密码哈希失败: %w", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 提交成功后 Rollback 在已提交事务上是 no-op

	var teamID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO teams (name) VALUES ($1)
		 ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		 RETURNING id`,
		team,
	).Scan(&teamID); err != nil {
		return fmt.Errorf("创建/查询团队失败: %w", err)
	}

	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, display_name, enabled)
		 VALUES ($1, $2, $3, true)
		 ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash, enabled = true
		 RETURNING id`,
		username, string(hash), displayName,
	).Scan(&userID); err != nil {
		return fmt.Errorf("创建/更新用户失败: %w", err)
	}

	var role string
	if err := tx.QueryRow(ctx,
		`INSERT INTO memberships (user_id, team_id, role)
		 VALUES ($1, $2, 'admin')
		 ON CONFLICT (team_id, user_id) DO UPDATE SET role = 'admin'
		 RETURNING role`,
		userID, teamID,
	).Scan(&role); err != nil {
		return fmt.Errorf("创建/更新 membership 失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}

	fmt.Printf("完成: user_id=%s team_id=%s role=%s\n", userID, teamID, role)
	return nil
}
