// Package runbook 实现 Incident 受控操作。执行器只接收已注册 action key 和类型化
// 参数，不存在 shell/command 字段；高风险 apply 还必须消费短时效计划与独立审批。
package runbook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	incidentservice "alert-system/internal/incident"
	"alert-system/internal/store"
)

const (
	ActionStopInvalidRetries = "e2b.stop_invalid_retries"
	ActionSnapshotAudit      = "e2b.snapshot_audit"
	ActionFCAudit            = "e2b.fc_audit"
	ActionCleanupDryRun      = "e2b.cleanup_dry_run"
	ActionCleanupApply       = "e2b.cleanup_apply"
	ActionLifecycleVerify    = "e2b.lifecycle_verify"
)

var (
	ErrActionNotAllowed         = errors.New("runbook: 动作不在白名单")
	ErrActionDisabled           = errors.New("runbook: 动作未启用")
	ErrUATAuthorizationRequired = errors.New("runbook: 需要单独的 UAT 授权")
	ErrGuardRejected            = errors.New("runbook: 安全护栏拒绝执行")
	ErrInvalidParameters        = errors.New("runbook: 参数非法")
	ErrExecutorDisabled         = errors.New("runbook: 当前环境未配置执行器")
)

type ExecutionRequest struct {
	RunID      string
	IncidentID string
	ActionKey  string
	Mode       domain.RunbookRunMode
	Parameters map[string]any
	Plan       map[string]any
}

type Executor interface {
	Execute(ctx context.Context, request ExecutionRequest) (map[string]any, error)
}

// DisabledExecutor 是生产默认值：在尚未显式配置受支持 E2B API 适配器前，任何
// Runbook 都不会退化为本机命令或 shell。请求仍会落审计为 failed。
type DisabledExecutor struct{}

func (DisabledExecutor) Execute(context.Context, ExecutionRequest) (map[string]any, error) {
	return nil, ErrExecutorDisabled
}

type RecordedExecution struct {
	Request ExecutionRequest
}

// FakeExecutor 是本地 E2E 的确定性实现。它只模拟精确目标的审计/删除结果，不连接
// 真实 E2B，不读取密钥，也不修改外部状态。
type FakeExecutor struct {
	mu         sync.Mutex
	executions []RecordedExecution
}

func NewFakeExecutor() *FakeExecutor { return &FakeExecutor{} }

func (f *FakeExecutor) Execute(_ context.Context, request ExecutionRequest) (map[string]any, error) {
	switch request.ActionKey {
	case ActionStopInvalidRetries, ActionSnapshotAudit, ActionFCAudit, ActionCleanupDryRun, ActionCleanupApply:
	default:
		return nil, ErrActionNotAllowed
	}
	f.mu.Lock()
	f.executions = append(f.executions, RecordedExecution{Request: request})
	f.mu.Unlock()

	targets, _ := targetRefs(request.Parameters)
	if request.ActionKey == ActionCleanupApply {
		targets = planTargetRefs(request.Plan)
		return map[string]any{
			"executor": "fake", "external_mutation": false,
			"deleted_target_refs": stringsToAny(targets), "verified_absent": true,
		}, nil
	}
	candidates := make([]any, 0, len(targets))
	for _, target := range targets {
		candidates = append(candidates, map[string]any{
			"target_ref": target, "eligible_for_cleanup": true,
			"snapshot_last_exists": true, "catalog_exists": false,
			"storage_exists": false, "ttl_refresh_observed": false,
		})
	}
	return map[string]any{"executor": "fake", "candidates": candidates}, nil
}

func (f *FakeExecutor) Executions() []RecordedExecution {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]RecordedExecution, len(f.executions))
	copy(result, f.executions)
	return result
}

type Service struct {
	tx        *store.Store
	incidents *store.IncidentRepo
	runs      *store.RunbookRepo
	executor  Executor
	uatPolicy UATPolicy
	now       func() time.Time
	planTTL   time.Duration
}

type ServiceOption func(*Service)

func WithUATPolicy(policy UATPolicy) ServiceOption {
	return func(service *Service) { service.uatPolicy = policy }
}

func NewService(tx *store.Store, incidents *store.IncidentRepo, runs *store.RunbookRepo, executor Executor, options ...ServiceOption) *Service {
	if executor == nil {
		executor = DisabledExecutor{}
	}
	service := &Service{tx: tx, incidents: incidents, runs: runs, executor: executor, now: time.Now, planTTL: 15 * time.Minute}
	for _, option := range options {
		option(service)
	}
	return service
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
			if items[index].UATOnly {
				if available, reason := s.uatPolicy.Availability(s.now()); !available {
					items[index].RuntimeAvailable, items[index].RuntimeReason = false, reason
				}
			}
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
	var definition domain.RunbookDefinition
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
		definition, err = s.runs.GetDefinition(ctx, tx, actionKey)
		if errors.Is(err, store.ErrRunbookNotFound) {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, ErrActionNotAllowed.Error(), now, &run)
		}
		if err != nil {
			return err
		}
		if !definition.Enabled {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, ErrActionDisabled.Error(), now, &run)
		}
		if actionKey == ActionCleanupApply {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, "cleanup_apply 必须使用受保护 apply 入口", now, &run)
		}
		if err := validateActionParameters(actionKey, parameters); err != nil {
			return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, err.Error(), now, &run)
		}
		if definition.UATOnly {
			if !canExecuteMutatingRunbook(roles, actor.UserID) {
				return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, "UAT 变更执行人必须是 Incident Commander 或 Technical Lead", now, &run)
			}
			if authorizeErr := s.uatPolicy.Authorize(parameters, now); authorizeErr != nil {
				return s.createRejected(ctx, tx, incidentID, actionKey, parameters, actor, authorizeErr.Error(), now, &run)
			}
		}
		mode := modeForDefinition(definition, actionKey)
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

func (s *Service) Approve(ctx context.Context, incidentID, planRunID, planSHA string, decision domain.RunbookApprovalDecision, comment string, actor incidentservice.Actor) (approval domain.RunbookApproval, err error) {
	if actor.UserID == "" || incidentID == "" || planRunID == "" || planSHA == "" ||
		(decision != domain.RunbookApprovalApproved && decision != domain.RunbookApprovalRejected) {
		return approval, ErrInvalidParameters
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		planRun, getErr := s.runs.GetRun(ctx, tx, planRunID, true)
		if getErr != nil {
			return getErr
		}
		if planRun.IncidentID != incidentID || planRun.ActionKey != ActionCleanupDryRun ||
			planRun.Status != domain.RunbookRunSucceeded || planRun.PlanSHA256 != planSHA {
			return fmt.Errorf("%w: 审批对象与有效 dry-run 计划不一致", ErrGuardRejected)
		}
		if canonicalSHA256(planRun.Plan) != planRun.PlanSHA256 {
			return fmt.Errorf("%w: 计划内容哈希校验失败", ErrGuardRejected)
		}
		roles, roleErr := s.incidents.ListRoles(ctx, tx, incidentID)
		if roleErr != nil {
			return roleErr
		}
		if !hasAnyIncidentRole(roles, actor.UserID) {
			return fmt.Errorf("%w: 审批人未被分配 Incident 响应角色", ErrGuardRejected)
		}
		if planRun.PlanExpiresAt.IsZero() || !s.now().Before(planRun.PlanExpiresAt) {
			return fmt.Errorf("%w: 计划已过期", ErrGuardRejected)
		}
		approval, getErr = s.runs.UpsertApproval(ctx, tx, domain.RunbookApproval{
			PlanRunID: planRunID, PlanSHA256: planSHA, Decision: decision,
			ApprovedBy: actor.UserID, Comment: strings.TrimSpace(comment), ApprovedAt: s.now(),
		})
		if getErr != nil {
			return getErr
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, "Runbook 计划审批", map[string]any{
			"plan_run_id": planRunID, "plan_sha256": planSHA, "decision": decision,
		}, approval.ApprovedAt)
	})
	return approval, err
}

type ApplyCommand struct {
	PlanRunID         string
	ConfirmPlanSHA256 string
	RetriesStopped    bool
}

func (s *Service) Apply(ctx context.Context, incidentID string, command ApplyCommand, actor incidentservice.Actor) (run domain.RunbookRun, err error) {
	if actor.UserID == "" || actor.Display == "" || incidentID == "" {
		return run, ErrInvalidParameters
	}
	now := s.now()
	var planRun domain.RunbookRun
	var approval domain.RunbookApproval
	var rejectionReason string
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		incident, getErr := s.incidents.GetByID(ctx, tx, incidentID)
		if getErr != nil {
			return getErr
		}
		if incident.Status == domain.IncidentStatusResolved {
			rejectionReason = "Incident 已解决"
		}
		roles, roleErr := s.incidents.ListRoles(ctx, tx, incidentID)
		if roleErr != nil {
			return roleErr
		}
		if rejectionReason == "" && !canExecuteMutatingRunbook(roles, actor.UserID) {
			rejectionReason = "执行人必须是 Incident Commander 或 Technical Lead"
		}
		if rejectionReason == "" && command.PlanRunID == "" {
			rejectionReason = "缺少 dry-run 计划"
		}
		if rejectionReason == "" {
			planRun, getErr = s.runs.GetRun(ctx, tx, command.PlanRunID, true)
			if errors.Is(getErr, store.ErrRunbookRunNotFound) {
				rejectionReason = "dry-run 计划不存在"
			} else if getErr != nil {
				return getErr
			}
		}
		if rejectionReason == "" && (planRun.IncidentID != incidentID || planRun.ActionKey != ActionCleanupDryRun || planRun.Status != domain.RunbookRunSucceeded) {
			rejectionReason = "引用的记录不是已成功的本 Incident cleanup dry-run"
		}
		if rejectionReason == "" && (command.ConfirmPlanSHA256 == "" || command.ConfirmPlanSHA256 != planRun.PlanSHA256) {
			rejectionReason = "确认 SHA256 与计划不一致"
		}
		if rejectionReason == "" && canonicalSHA256(planRun.Plan) != planRun.PlanSHA256 {
			rejectionReason = "计划内容哈希校验失败"
		}
		if rejectionReason == "" && (planRun.PlanExpiresAt.IsZero() || !now.Before(planRun.PlanExpiresAt)) {
			rejectionReason = "计划已过期"
		}
		if rejectionReason == "" && (!command.RetriesStopped || !planRetriesStopped(planRun.Plan)) {
			rejectionReason = "调用方重试尚未停止或未再次确认"
		}
		if rejectionReason == "" {
			approval, getErr = s.runs.LatestApproved(ctx, tx, planRun.ID, planRun.PlanSHA256)
			if errors.Is(getErr, store.ErrRunbookApprovalAbsent) {
				rejectionReason = "计划缺少有效审批"
			} else if getErr != nil {
				return getErr
			}
		}
		if rejectionReason == "" && approval.ApprovedBy == actor.UserID {
			rejectionReason = "审批人与执行人必须分离"
		}

		parameters := map[string]any{
			"plan_run_id":         command.PlanRunID,
			"confirm_plan_sha256": command.ConfirmPlanSHA256,
			"retries_stopped":     command.RetriesStopped,
		}
		if rejectionReason != "" {
			return s.createRejected(ctx, tx, incidentID, ActionCleanupApply, parameters, actor, rejectionReason, now, &run)
		}
		run, getErr = s.runs.CreateRun(ctx, tx, domain.RunbookRun{
			IncidentID: incidentID, ActionKey: ActionCleanupApply, Mode: domain.RunbookModeApply,
			Status: domain.RunbookRunRunning, Parameters: parameters,
			SourcePlanRunID: planRun.ID, RequestedBy: actor.UserID, ExecutedBy: actor.UserID,
			RequestedAt: now, StartedAt: now,
		})
		if getErr != nil {
			return getErr
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, "已通过护栏，开始执行 cleanup apply", map[string]any{
			"run_id": run.ID, "plan_run_id": planRun.ID, "plan_sha256": planRun.PlanSHA256,
			"approved_by": approval.ApprovedBy,
		}, now)
	})
	if err != nil {
		return run, err
	}
	if run.Status == domain.RunbookRunRejected {
		return run, fmt.Errorf("%w: %s", ErrGuardRejected, run.Error)
	}
	result, executionErr := s.executor.Execute(ctx, ExecutionRequest{
		RunID: run.ID, IncidentID: incidentID, ActionKey: ActionCleanupApply,
		Mode: domain.RunbookModeApply, Parameters: run.Parameters, Plan: planRun.Plan,
	})
	return s.completeAfterExternalExecution(ctx, run, actor, result, executionErr)
}

func (s *Service) completeAfterExternalExecution(requestCtx context.Context, run domain.RunbookRun, actor incidentservice.Actor, result map[string]any, executionErr error) (domain.RunbookRun, error) {
	// HTTP 客户端可能在长时间 UAT resume 期间断开。外部动作一旦开始，完成状态和
	// cleanup 结果就必须落审计，不能继续复用已取消的 request context。
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
	plan := map[string]any{}
	planSHA := ""
	var expiresAt time.Time
	if run.ActionKey == ActionCleanupDryRun && executionErr == nil {
		plan = buildCleanupPlan(run.Parameters, result, s.now())
		planSHA = canonicalSHA256(plan)
		expiresAt = s.now().Add(s.planTTL)
	}
	var completed domain.RunbookRun
	completeAt := s.now()
	err := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var updateErr error
		completed, updateErr = s.runs.CompleteRun(ctx, tx, run.ID, status, actor.UserID,
			plan, planSHA, expiresAt, result, errorText, completeAt)
		if updateErr != nil {
			return updateErr
		}
		summary := "Runbook 执行完成"
		if executionErr != nil {
			summary = "Runbook 执行失败"
		}
		return s.appendTimeline(ctx, tx, run.IncidentID, actor, summary, map[string]any{
			"run_id": run.ID, "action_key": run.ActionKey, "status": status,
			"plan_sha256": planSHA, "error": errorText,
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

func modeForDefinition(definition domain.RunbookDefinition, actionKey string) domain.RunbookRunMode {
	if actionKey == ActionCleanupDryRun {
		return domain.RunbookModeDryRun
	}
	if definition.RiskLevel == domain.RunbookRiskCoordination {
		return domain.RunbookModeCoordination
	}
	if definition.RiskLevel == domain.RunbookRiskMutating {
		return domain.RunbookModeApply
	}
	return domain.RunbookModeReadonly
}

var targetRefPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{5,127}$`)
var uatReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{1,127}$`)

func validateActionParameters(actionKey string, parameters map[string]any) error {
	switch actionKey {
	case ActionCleanupDryRun, ActionSnapshotAudit, ActionFCAudit:
		targets, err := targetRefs(parameters)
		if err != nil || len(targets) == 0 || len(targets) > 50 {
			return fmt.Errorf("%w: target_refs 必须包含 1..50 个精确目标", ErrInvalidParameters)
		}
		for _, target := range targets {
			if !targetRefPattern.MatchString(target) {
				return fmt.Errorf("%w: 非法 target_ref", ErrInvalidParameters)
			}
		}
		if actionKey == ActionCleanupDryRun {
			if value, _ := parameters["retries_stopped"].(bool); !value {
				return fmt.Errorf("%w: cleanup dry-run 要求已记录停止重试", ErrInvalidParameters)
			}
			for _, key := range []string{"retries_stopped_at", "retries_stopped_by", "ticket_ref"} {
				if value, _ := parameters[key].(string); strings.TrimSpace(value) == "" {
					return fmt.Errorf("%w: 缺少 %s", ErrInvalidParameters, key)
				}
			}
		}
	case ActionStopInvalidRetries:
		for _, key := range []string{"retries_stopped_at", "retries_stopped_by", "ticket_ref"} {
			if value, _ := parameters[key].(string); strings.TrimSpace(value) == "" {
				return fmt.Errorf("%w: 缺少 %s", ErrInvalidParameters, key)
			}
		}
	case ActionLifecycleVerify:
		allowedKeys := map[string]struct{}{
			"environment_id": {}, "authorization_ref": {}, "template_id": {},
			"template_alias": {}, "confirm_cleanup": {},
		}
		for key := range parameters {
			if _, ok := allowedKeys[key]; !ok {
				return fmt.Errorf("%w: lifecycle 参数 %s 不在白名单", ErrInvalidParameters, key)
			}
		}
		for _, key := range []string{"environment_id", "authorization_ref", "template_id"} {
			value, _ := parameters[key].(string)
			if !uatReferencePattern.MatchString(strings.TrimSpace(value)) {
				return fmt.Errorf("%w: 非法 %s", ErrInvalidParameters, key)
			}
		}
		if alias, exists := parameters["template_alias"]; exists {
			value, ok := alias.(string)
			if !ok || strings.TrimSpace(value) == "" || len(value) > 64 || strings.ContainsAny(value, "\r\n\t") {
				return fmt.Errorf("%w: 非法 template_alias", ErrInvalidParameters)
			}
		}
		if confirmed, _ := parameters["confirm_cleanup"].(bool); !confirmed {
			return fmt.Errorf("%w: 必须确认生命周期探针自动清理", ErrInvalidParameters)
		}
	default:
		return ErrActionNotAllowed
	}
	return nil
}

func targetRefs(parameters map[string]any) ([]string, error) {
	value, ok := parameters["target_refs"]
	if !ok {
		return nil, ErrInvalidParameters
	}
	var targets []string
	switch raw := value.(type) {
	case []string:
		targets = append(targets, raw...)
	case []any:
		for _, item := range raw {
			text, ok := item.(string)
			if !ok {
				return nil, ErrInvalidParameters
			}
			targets = append(targets, text)
		}
	default:
		return nil, ErrInvalidParameters
	}
	return targets, nil
}

func buildCleanupPlan(parameters, executorResult map[string]any, now time.Time) map[string]any {
	return map[string]any{
		"schema_version": 1,
		"incident":       "failed-to-place-sandbox",
		"created_at":     now.UTC().Format(time.RFC3339Nano),
		"created_unix":   now.Unix(),
		"retries_stopped": map[string]any{
			"confirmed":  true,
			"at":         parameters["retries_stopped_at"],
			"by":         parameters["retries_stopped_by"],
			"ticket_ref": parameters["ticket_ref"],
		},
		"candidates": executorResult["candidates"],
	}
}

func canonicalSHA256(plan map[string]any) string {
	raw, _ := json.Marshal(plan)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func planRetriesStopped(plan map[string]any) bool {
	value, ok := plan["retries_stopped"].(map[string]any)
	if !ok {
		return false
	}
	confirmed, _ := value["confirmed"].(bool)
	return confirmed
}

func planTargetRefs(plan map[string]any) []string {
	items, _ := plan["candidates"].([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		candidate, _ := item.(map[string]any)
		target, _ := candidate["target_ref"].(string)
		if target != "" {
			result = append(result, target)
		}
	}
	return result
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
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
