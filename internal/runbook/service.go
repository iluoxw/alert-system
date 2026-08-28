// Package runbook 实现 Incident 受控操作。执行器只接收已注册 action key 和类型化
// 参数，不存在 shell/command 字段。
package runbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/store"
)

var (
	ErrActionNotAllowed  = errors.New("runbook: 动作不在白名单")
	ErrActionDisabled    = errors.New("runbook: 动作未启用")
	ErrGuardRejected     = errors.New("runbook: 安全护栏拒绝执行")
	ErrInvalidParameters = errors.New("runbook: 参数非法")
	ErrExecutorDisabled  = errors.New("runbook: 当前环境未配置执行器")
)

type ExecutionRequest struct {
	RunID      string
	IncidentID string
	ActionKey  string
	Mode       domain.RunbookRunMode
	Parameters map[string]any
}

type Executor interface {
	Execute(ctx context.Context, request ExecutionRequest) (map[string]any, error)
}

// DisabledExecutor 是生产默认值：未显式配置受支持 API 适配器前，Runbook 不会退化为
// 本机命令或 shell。请求仍会落审计为 failed。
type DisabledExecutor struct{}

func (DisabledExecutor) Execute(context.Context, ExecutionRequest) (map[string]any, error) {
	return nil, ErrExecutorDisabled
}

func (DisabledExecutor) Availability(string) (bool, string) {
	return false, "服务端未配置该动作的执行器"
}

type Service struct {
	tx        *store.Store
	incidents *store.IncidentRepo
	runs      *store.RunbookRepo
	executor  Executor
	now       func() time.Time
}

func NewService(tx *store.Store, incidents *store.IncidentRepo, runs *store.RunbookRepo, executor Executor) *Service {
	if executor == nil {
		executor = DisabledExecutor{}
	}
	return &Service{tx: tx, incidents: incidents, runs: runs, executor: executor, now: time.Now}
}

func (s *Service) ListDefinitions(ctx context.Context) (items []domain.RunbookDefinition, err error) {
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		items, listErr = s.runs.ListDefinitions(ctx, tx)
		return listErr
	})
	if err == nil {
		for index := range items {
			if !items[index].Enabled {
				items[index].RuntimeAvailable = false
				items[index].RuntimeReason = "动作定义未启用"
				continue
			}
			items[index].RuntimeAvailable, items[index].RuntimeReason = executorAvailability(s.executor, items[index].ActionKey)
		}
	}
	return items, err
}

func (s *Service) ListRuns(ctx context.Context, incidentID string) (items []domain.RunbookRun, err error) {
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.incidents.GetByID(ctx, tx, incidentID); err != nil {
			return err
		}
		var listErr error
		items, listErr = s.runs.ListRuns(ctx, tx, incidentID)
		return listErr
	})
	return items, err
}

func (s *Service) Execute(ctx context.Context, incidentID, actionKey string, parameters map[string]any, actor incidentservice.Actor) (domain.RunbookRun, error) {
	if actor.UserID == "" || actor.Display == "" || incidentID == "" || strings.TrimSpace(actionKey) == "" {
		return domain.RunbookRun{}, ErrInvalidParameters
	}
	if parameters == nil {
		parameters = map[string]any{}
	}
	var run domain.RunbookRun
	now := s.now()
	guardErr := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		incident, err := s.incidents.GetByID(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		if incident.Status == domain.IncidentStatusResolved {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, "Incident 已解决", now, &run)
		}
		roles, roleErr := s.incidents.ListRoles(ctx, tx, incidentID)
		if roleErr != nil {
			return roleErr
		}
		if !hasAnyIncidentRole(roles, actor.UserID) {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, "执行人未被分配 Incident 响应角色", now, &run)
		}
		definition, err := s.runs.GetDefinition(ctx, tx, actionKey)
		if errors.Is(err, store.ErrRunbookNotFound) {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, ErrActionNotAllowed.Error(), now, &run)
		}
		if err != nil {
			return err
		}
		if !definition.Enabled {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, ErrActionDisabled.Error(), now, &run)
		}
		if err := validateActionParameters(parameters); err != nil {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, err.Error(), now, &run)
		}
		if definition.RiskLevel == domain.RunbookRiskMutating && !canExecuteMutatingRunbook(roles, actor.UserID) {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, "变更执行人必须是 Incident Commander 或 Technical Lead", now, &run)
		}
		mode := modeForDefinition(definition)
		run, err = s.runs.CreateRun(ctx, tx, domain.RunbookRun{
			IncidentID: incidentID, ActionKey: actionKey, Mode: mode,
			Status: domain.RunbookRunRunning, Parameters: sanitizeAuditParameters(parameters),
			RequestedBy: actor.UserID, ExecutedBy: actor.UserID,
			RequestedAt: now, StartedAt: now,
		})
		if err != nil {
			return err
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, "Runbook 已启动", map[string]any{
			"run_id": run.ID, "action_key": actionKey, "mode": mode,
		}, now)
	})
	if guardErr != nil {
		return run, guardErr
	}
	if run.Status == domain.RunbookRunRejected {
		return run, fmt.Errorf("%w: %s", ErrGuardRejected, run.Error)
	}

	result, executionErr := s.executor.Execute(ctx, ExecutionRequest{
		RunID: run.ID, IncidentID: incidentID, ActionKey: actionKey,
		Mode: run.Mode, Parameters: parameters,
	})
	return s.completeAfterExternalExecution(ctx, run, actor, result, executionErr)
}

func (s *Service) completeAfterExternalExecution(requestCtx context.Context, run domain.RunbookRun, actor incidentservice.Actor, result map[string]any, executionErr error) (domain.RunbookRun, error) {
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), 10*time.Second)
	defer cancel()
	return s.completeExecution(completionCtx, run, actor, result, executionErr)
}

func (s *Service) completeExecution(ctx context.Context, run domain.RunbookRun, actor incidentservice.Actor, result map[string]any, executionErr error) (domain.RunbookRun, error) {
	status := domain.RunbookRunSucceeded
	errorText := ""
	if executionErr != nil {
		status = domain.RunbookRunFailed
		errorText = executionErr.Error()
	}
	if result == nil {
		result = map[string]any{}
	}
	var completed domain.RunbookRun
	completeAt := s.now()
	err := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var updateErr error
		completed, updateErr = s.runs.CompleteRun(ctx, tx, run.ID, status, actor.UserID,
			nil, "", time.Time{}, result, errorText, completeAt)
		if updateErr != nil {
			return updateErr
		}
		summary := "Runbook 执行完成"
		if executionErr != nil {
			summary = "Runbook 执行失败"
		}
		return s.appendTimeline(ctx, tx, run.IncidentID, actor, summary, map[string]any{
			"run_id": run.ID, "action_key": run.ActionKey, "status": status, "error": errorText,
		}, completeAt)
	})
	if err != nil {
		return completed, err
	}
	if executionErr != nil {
		return completed, executionErr
	}
	return completed, nil
}

func (s *Service) createRejected(ctx context.Context, tx pgx.Tx, incidentID, actionKey string, parameters map[string]any, actor incidentservice.Actor, reason string, at time.Time, target *domain.RunbookRun) error {
	run, err := s.runs.CreateRun(ctx, tx, domain.RunbookRun{
		IncidentID: incidentID, ActionKey: actionKey, Mode: domain.RunbookModeApply,
		Status: domain.RunbookRunRejected, Parameters: sanitizeAuditParameters(parameters),
		RequestedBy: actor.UserID, RequestedAt: at, CompletedAt: at, Error: reason,
	})
	if err != nil {
		return err
	}
	*target = run
	return s.appendTimeline(ctx, tx, incidentID, actor, "Runbook 安全护栏拒绝操作", map[string]any{
		"run_id": run.ID, "action_key": actionKey, "reason": reason,
	}, at)
}

func (s *Service) appendTimeline(ctx context.Context, tx pgx.Tx, incidentID string, actor incidentservice.Actor, summary string, detail map[string]any, at time.Time) error {
	_, err := s.incidents.AppendIncidentTimeline(ctx, tx, domain.IncidentTimelineEntry{
		IncidentID: incidentID, ActorUserID: actor.UserID, ActorDisplay: actor.Display,
		Kind: domain.IncidentTimelineAutomation, Summary: summary, Detail: detail, OccurredAt: at,
	})
	return err
}

func modeForDefinition(definition domain.RunbookDefinition) domain.RunbookRunMode {
	switch definition.RiskLevel {
	case domain.RunbookRiskCoordination:
		return domain.RunbookModeCoordination
	case domain.RunbookRiskMutating:
		return domain.RunbookModeApply
	default:
		return domain.RunbookModeReadonly
	}
}

type availabilityExecutor interface {
	Availability(actionKey string) (bool, string)
}

func executorAvailability(executor Executor, actionKey string) (bool, string) {
	if provider, ok := executor.(availabilityExecutor); ok {
		return provider.Availability(actionKey)
	}
	return true, ""
}

func validateActionParameters(parameters map[string]any) error {
	raw, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("%w: 参数无法序列化", ErrInvalidParameters)
	}
	if len(raw) > 64*1024 {
		return fmt.Errorf("%w: 参数过大", ErrInvalidParameters)
	}
	for key := range parameters {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "command") || strings.Contains(lower, "shell") {
			return fmt.Errorf("%w: 参数 %s 不在白名单", ErrInvalidParameters, key)
		}
	}
	return nil
}

func hasAnyIncidentRole(roles []domain.IncidentRoleAssignment, userID string) bool {
	for _, assignment := range roles {
		if assignment.UserID == userID {
			return true
		}
	}
	return false
}

func canExecuteMutatingRunbook(roles []domain.IncidentRoleAssignment, userID string) bool {
	for _, assignment := range roles {
		if assignment.UserID == userID &&
			(assignment.Role == domain.IncidentRoleCommander || assignment.Role == domain.IncidentRoleTechnicalLead) {
			return true
		}
	}
	return false
}

func sanitizeAuditParameters(parameters map[string]any) map[string]any {
	result := make(map[string]any, len(parameters))
	for key, value := range parameters {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "secret") || strings.Contains(lower, "token") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "api_key") ||
			strings.Contains(lower, "command") || strings.Contains(lower, "shell") {
			result[key] = "[redacted]"
			continue
		}
		result[key] = value
	}
	return result
}
