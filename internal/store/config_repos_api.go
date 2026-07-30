package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// 本文件是 config_repos.go / config_repos_routing.go / config_repos_notify.go
// 既有仓储类型的纯增量补充：管理 API（任务 27）需要"列出全部记录"而不是仅按团队
// 过滤，既有文件只提供了 ListByTeam，这里新增 ListAll，不改动既有文件与方法。

// ListAll 按团队、名称升序返回全部值班表，供管理端不带 team_id 过滤条件时使用。
func (r *ScheduleRepo) ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]Schedule, error) {
	rows, err := tx.Query(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY team_id, name LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出全部值班表失败: %w", err)
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描值班表失败: %w", err)
		}
		schedules = append(schedules, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历值班表失败: %w", err)
	}
	return schedules, nil
}

// ListAll 按团队、创建时间升序返回全部升级策略，供管理端不带 team_id 过滤条件时使用。
func (r *EscalationPolicyRepo) ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]EscalationPolicy, error) {
	rows, err := tx.Query(ctx, `SELECT `+escalationPolicyColumns+` FROM escalation_policies ORDER BY team_id, created_at LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出全部升级策略失败: %w", err)
	}
	defer rows.Close()

	var policies []EscalationPolicy
	for rows.Next() {
		p, err := scanEscalationPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描升级策略失败: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历升级策略失败: %w", err)
	}
	return policies, nil
}

// ListAll 按创建时间升序返回全部通知策略（含全局默认与团队默认），供管理端不带
// team_id 过滤条件时使用。
func (r *NotificationPolicyRepo) ListAll(ctx context.Context, tx pgx.Tx, limit int) ([]NotificationPolicy, error) {
	rows, err := tx.Query(ctx, `SELECT `+notificationPolicyColumns+` FROM notification_policies ORDER BY created_at LIMIT $1`, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: 列出全部通知策略失败: %w", err)
	}
	defer rows.Close()

	var policies []NotificationPolicy
	for rows.Next() {
		p, err := scanNotificationPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描通知策略失败: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历通知策略失败: %w", err)
	}
	return policies, nil
}
