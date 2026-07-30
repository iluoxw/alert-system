// Package store 是 PostgreSQL 仓储实现（基于 pgx）。
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 封装 pgx 连接池，是所有仓储（EventQueue 等）的基础设施。
type Store struct {
	Pool *pgxpool.Pool
}

// New 用 dsn 建立连接池并做一次 Ping 验证连通性。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 建立连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping 数据库失败: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close 释放连接池。
func (s *Store) Close() {
	s.Pool.Close()
}

// WithTx 在一个事务内执行 fn：fn 返回非 nil error 或 panic 都会回滚，否则提交。
// panic 会在回滚后重新抛出，交由调用方上层处理。
func (s *Store) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return
		}
		err = tx.Commit(ctx)
	}()

	err = fn(tx)
	return err
}
