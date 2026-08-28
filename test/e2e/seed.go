package e2e

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"alert-system/internal/store"
)

// seedAdminUsername/seedAdminPassword 是 bootstrap 阶段创建的第一个管理员账号，
// 供各场景登录后经真实 HTTP admin API 创建 route/schedule/escalation-policy/
// notification-policy/integration/silence。
const (
	seedAdminUsername    = "e2e-admin"
	seedAdminPassword    = "e2e-admin-pass-000" //nolint:gosec // 测试固定密码，非生产凭据
	seedApproverUsername = "e2e-approver"
	seedApproverPassword = "e2e-approver-pass-000" //nolint:gosec // 测试固定密码，非生产凭据
)

// seedIdentities 是 bootstrapAdmin 的产出。
type seedIdentities struct {
	Team     *store.Team
	User     *store.User
	Approver *store.User
}

// bootstrapAdmin 直接调用 store 仓储创建团队、管理员用户及其 admin 角色成员关系。
//
// 这是唯一没有办法经真实 HTTP 完成的一步：internal/api/config.go 挂载的全部
// /api/v1/admin/* 端点都要求 sessionAuth()+requireAdmin()，而"创建第一个 admin
// 账号"正是这条鸡生蛋链条的起点——没有已认证的 admin 会话，就无法调用创建用户/
// 成员关系的端点来产生第一个 admin 会话。bootstrap 之后的一切配置（route/
// schedule/escalation-policy/notification-policy/integration/silence）都改走真实
// HTTP admin API，用这个 bootstrap 出来的账号登录。
func bootstrapAdmin(ctx context.Context, st *store.Store, teams *store.TeamRepo, users *store.UserRepo) (*seedIdentities, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(seedAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("e2e: 生成种子管理员密码哈希失败: %w", err)
	}

	var team *store.Team
	var user *store.User
	var approver *store.User
	err = st.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		team, txErr = teams.Create(ctx, tx, "e2e-team")
		if txErr != nil {
			return fmt.Errorf("创建种子团队失败: %w", txErr)
		}
		user, txErr = users.Create(ctx, tx, store.User{
			Username:     seedAdminUsername,
			PasswordHash: string(hash),
			DisplayName:  "E2E Admin",
			Email:        "e2e-admin@example.com",
			Enabled:      true,
		})
		if txErr != nil {
			return fmt.Errorf("创建种子管理员失败: %w", txErr)
		}
		approverHash, hashErr := bcrypt.GenerateFromPassword([]byte(seedApproverPassword), bcrypt.DefaultCost)
		if hashErr != nil {
			return fmt.Errorf("生成审批人密码哈希失败: %w", hashErr)
		}
		approver, txErr = users.Create(ctx, tx, store.User{
			Username: seedApproverUsername, PasswordHash: string(approverHash),
			DisplayName: "E2E Independent Approver", Email: "e2e-approver@example.com", Enabled: true,
		})
		if txErr != nil {
			return fmt.Errorf("创建种子审批人失败: %w", txErr)
		}
		// memberships 仓储（internal/store/memberships.go）只有 IsAdminAnyTeam 一个
		// 查询方法，没有 Create——建 admin 成员关系用原生 SQL，与
		// migrations/000002_org_routing.up.sql 的 memberships 表结构（
		// user_id/team_id/role CHECK IN ('admin','member')）对齐。
		if _, txErr = tx.Exec(ctx, `
			INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')
		`, user.ID, team.ID); txErr != nil {
			return fmt.Errorf("创建种子管理员成员关系失败: %w", txErr)
		}
		if _, txErr = tx.Exec(ctx, `
			INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')
		`, approver.ID, team.ID); txErr != nil {
			return fmt.Errorf("创建种子审批人成员关系失败: %w", txErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &seedIdentities{Team: team, User: user, Approver: approver}, nil
}

// seedShift 直接调用 store.ShiftRepo 创建一条覆盖"现在"的值班班次：
// [now-startsAgo, now+endsIn)。
//
// internal/api/config.go 的 admin 端点只有 schedules 的 CRUD，没有任何 shift 相关
// 端点（已回报 team-lead 的集成缝隙之一）——建班次没有 HTTP 路径可走，只能直接调
// 仓储。endsIn 用足够宽的窗口（数小时级）避免因场景执行耗时导致班次意外过期。
func seedShift(ctx context.Context, st *store.Store, shifts *store.ShiftRepo, scheduleID, userID string, startsAgo, endsIn time.Duration) (*store.Shift, error) {
	now := time.Now()
	var shift *store.Shift
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		shift, txErr = shifts.Create(ctx, tx, store.Shift{
			ScheduleID: scheduleID,
			UserID:     userID,
			StartsAt:   now.Add(-startsAgo),
			EndsAt:     now.Add(endsIn),
		})
		return txErr
	})
	if err != nil {
		return nil, fmt.Errorf("e2e: 创建种子值班班次失败: %w", err)
	}
	return shift, nil
}

// seedConfigResult 汇总 seedConfig 经真实 HTTP admin API 创建出的配置，供各场景
// 测试构造 ingest payload（token）时使用。
type seedConfigResult struct {
	TeamID         string
	AdminUserID    string
	ApproverUserID string

	GrafanaWebhookPath string
	GenericWebhookPath string
}

// seedConfig 用 bootstrapAdmin 产出的管理员账号登录后，经真实 /api/v1/admin/*
// HTTP 端点依次创建：两条路由（兜底路由 group_by=[alertname] 供场景①②③R13；
// 场景⑤专用路由不配置 group_by，验证 Ruling 2 的单例组回归）、值班表（+ 直接调
// 仓储建一条覆盖当前时刻的班次——schedules 有 CRUD 端点但 shifts 没有）、升级
// 策略（Levels 的 Targets 是团队 ID，oncall.Resolver 按团队 ID 查值班表求值，不是
// 用户 ID，见 internal/oncall/escalation.go）、全局默认通知策略（team_id=""+
// matchers=[]，R13.5 三级兜底链的最后一级）、两个 webhook 集成（grafana 用于
// 仍需验证本地路由/分组/通知的场景①②/R13/⑤；generic 用于场景③）。真正的
// Alertmanager 权威源由专门的 authority E2E 场景创建，不能复用来测试本地通知。
//
// 所有等待时长都用秒级短配置压缩：group_wait/group_interval=1s 让分组尽快 flush，
// escalation 两级 wait=0/6s，notification steps 的 warning 两级 wait=0/3s（对应
// R13 样例：email 立即 -> lark 3 秒后升级）。
func seedConfig(ctx context.Context, sys *system, ids *seedIdentities) (*seedConfigResult, error) {
	client, err := newAPIClient(sys.Server.URL)
	if err != nil {
		return nil, err
	}
	if err := client.login(seedAdminUsername, seedAdminPassword); err != nil {
		return nil, err
	}

	teamID := ids.Team.ID

	if _, _, err := client.doJSON(http.MethodPost, "/api/v1/admin/routes", map[string]any{
		"priority":                100,
		"matchers":                []map[string]string{{"name": "severity", "op": "=~", "value": ".*"}},
		"target_team_id":          teamID,
		"group_by":                []string{"alertname"},
		"group_wait_seconds":      1,
		"group_interval_seconds":  1,
		"repeat_interval_seconds": 4,
	}, nil); err != nil {
		return nil, fmt.Errorf("e2e: 创建种子路由失败: %w", err)
	}

	// 场景⑤专用路由：优先级低于上面的兜底路由（数值更小、按 priority 升序更早命中），
	// 只匹配 alertname 前缀 e2e-s5- 的告警，不配置 group_by——验证 Ruling 2（无
	// group_by 路由退化为单例组，而不是"永不通知"）的回归场景。
	if _, _, err := client.doJSON(http.MethodPost, "/api/v1/admin/routes", map[string]any{
		"priority":                50,
		"matchers":                []map[string]string{{"name": "alertname", "op": "=~", "value": "^e2e-s5-.*"}},
		"target_team_id":          teamID,
		"group_wait_seconds":      1,
		"group_interval_seconds":  1,
		"repeat_interval_seconds": 4,
	}, nil); err != nil {
		return nil, fmt.Errorf("e2e: 创建场景⑤种子路由（无 group_by）失败: %w", err)
	}

	var scheduleResp map[string]any
	if _, _, err := client.doJSON(http.MethodPost, "/api/v1/admin/schedules", map[string]any{
		"team_id":  teamID,
		"name":     "e2e-schedule",
		"timezone": "UTC",
	}, &scheduleResp); err != nil {
		return nil, fmt.Errorf("e2e: 创建种子值班表失败: %w", err)
	}
	scheduleID, _ := scheduleResp["id"].(string)
	if scheduleID == "" {
		return nil, fmt.Errorf("e2e: 种子值班表响应缺少 id: %+v", scheduleResp)
	}
	if _, err := seedShift(ctx, sys.Store, sys.Shifts, scheduleID, ids.User.ID, time.Hour, 6*time.Hour); err != nil {
		return nil, err
	}

	if _, _, err := client.doJSON(http.MethodPost, "/api/v1/admin/escalation-policies", map[string]any{
		"team_id": teamID,
		"levels": []map[string]any{
			{"wait_seconds": 0, "targets": []string{teamID}},
			{"wait_seconds": 6, "targets": []string{teamID}},
		},
	}, nil); err != nil {
		return nil, fmt.Errorf("e2e: 创建种子升级策略失败: %w", err)
	}

	if _, _, err := client.doJSON(http.MethodPost, "/api/v1/admin/notification-policies", map[string]any{
		"name":     "e2e-global-default",
		"matchers": []map[string]string{},
		"team_id":  "",
		"steps": map[string]any{
			"critical": []map[string]any{
				{"wait_seconds": 0, "channels": []string{"lark"}, "mode": "replace"},
			},
			"warning": []map[string]any{
				{"wait_seconds": 0, "channels": []string{"email"}, "mode": "replace"},
				{"wait_seconds": 3, "channels": []string{"lark"}, "mode": "replace"},
			},
		},
	}, nil); err != nil {
		return nil, fmt.Errorf("e2e: 创建种子通知策略失败: %w", err)
	}

	grafanaWebhookPath, err := createIntegration(client, "grafana", "e2e-grafana")
	if err != nil {
		return nil, err
	}
	genericWebhookPath, err := createIntegration(client, "generic", "e2e-generic")
	if err != nil {
		return nil, err
	}

	return &seedConfigResult{
		TeamID:             teamID,
		AdminUserID:        ids.User.ID,
		ApproverUserID:     ids.Approver.ID,
		GrafanaWebhookPath: grafanaWebhookPath,
		GenericWebhookPath: genericWebhookPath,
	}, nil
}

// createIntegration 经 POST /api/v1/admin/integrations 创建一个 integration，
// 返回一次性下发的 webhook_path。hook_id 的 SHA-256 由服务端存库，测试代码不会
// 自己复现 hash 口径。
func createIntegration(client *apiClient, kind, name string) (webhookPath string, err error) {
	var resp map[string]any
	status, body, err := client.doJSON(http.MethodPost, "/api/v1/admin/integrations", map[string]any{
		"kind":             kind,
		"name":             name,
		"default_severity": "warning",
	}, &resp)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("e2e: 创建种子集成（kind=%s）失败 status=%d body=%s", kind, status, body)
	}
	id, _ := resp["id"].(string)
	webhookPath, _ = resp["webhook_path"].(string)
	if id == "" || webhookPath == "" {
		return "", fmt.Errorf("e2e: 种子集成（kind=%s）响应缺少 id/webhook_path: %+v", kind, resp)
	}
	if _, exists := resp["token"]; exists {
		return "", fmt.Errorf("e2e: 种子集成（kind=%s）响应不应包含兼容 token: %+v", kind, resp)
	}
	return webhookPath, nil
}
