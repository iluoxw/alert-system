// Package incident 实现事故响应协作平面。它刻意位于自动告警关联之后：相关性引擎
// 可以生成/升级 Fault，但只有带 Actor 的显式调用才能宣布 Incident。
package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

var (
	ErrIncidentNotReady          = errors.New("incident: Fault 尚不满足事故宣布条件")
	ErrInvalidInput              = errors.New("incident: 输入非法")
	ErrIncidentClosed            = errors.New("incident: 已关闭 Incident 不允许修改 Fault 关系")
	ErrPrimaryFaultCannotExclude = errors.New("incident: primary Fault 不能直接排除，请先切换主 Fault")
	ErrIncidentFaultExcluded     = errors.New("incident: 已排除 Fault 不能作为主 Fault 或推进生命周期")
)

type Actor struct {
	UserID  string
	Display string
}

func (a Actor) valid() bool { return a.UserID != "" && strings.TrimSpace(a.Display) != "" }

type NotificationOutbox interface {
	QueueIncidentNotification(ctx context.Context, tx pgx.Tx, notification domain.IncidentNotification) error
}

type Service struct {
	tx        *store.Store
	faults    *store.FaultRepo
	incidents *store.IncidentRepo
	notifier  NotificationOutbox
	now       func() time.Time
}

func NewService(tx *store.Store, faults *store.FaultRepo, incidents *store.IncidentRepo, notifier NotificationOutbox) *Service {
	return &Service{tx: tx, faults: faults, incidents: incidents, notifier: notifier, now: time.Now}
}

type DeclareCommand struct {
	FaultID                string
	Title                  string
	Summary                string
	Severity               domain.Severity
	CommunicationChannel   string
	CommunicationTargetRef string
}

// Declare 是唯一的 Incident 创建入口。重复宣布同一个 Fault 返回既有 Incident，
// created=false；数据库唯一约束同时兜住并发双击。
func (s *Service) Declare(ctx context.Context, command DeclareCommand, actor Actor) (incident domain.Incident, created bool, err error) {
	if !actor.valid() || command.FaultID == "" || strings.TrimSpace(command.Title) == "" ||
		!command.Severity.IsValid() || strings.TrimSpace(command.CommunicationChannel) == "" ||
		strings.TrimSpace(command.CommunicationTargetRef) == "" {
		return domain.Incident{}, false, ErrInvalidInput
	}

	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		existing, getErr := s.incidents.GetByFaultID(ctx, tx, command.FaultID)
		if getErr == nil {
			incident = existing
			return nil
		}
		if !errors.Is(getErr, store.ErrIncidentNotFound) {
			return getErr
		}

		fault, getErr := s.faults.GetByID(ctx, tx, command.FaultID)
		if getErr != nil {
			return getErr
		}
		if fault.Status != domain.FaultStatusActive {
			return fmt.Errorf("%w: 仅 active Fault 可宣布", ErrIncidentNotReady)
		}
		impacted, impactErr := s.faults.HasConfirmedBusinessImpact(ctx, tx, fault.ID)
		if impactErr != nil {
			return impactErr
		}
		if !impacted {
			return fmt.Errorf("%w: 尚无 confirmed business_system 影响", ErrIncidentNotReady)
		}

		now := s.now()
		incident, getErr = s.incidents.Create(ctx, tx, domain.Incident{
			Status: domain.IncidentStatusDeclared, Severity: command.Severity,
			Title: strings.TrimSpace(command.Title), Summary: strings.TrimSpace(command.Summary),
			DeclaredBy: actor.UserID, CommunicationChannel: strings.TrimSpace(command.CommunicationChannel),
			CommunicationTargetRef: strings.TrimSpace(command.CommunicationTargetRef), DeclaredAt: now,
		})
		if getErr != nil {
			return getErr
		}
		if _, err := s.incidents.LinkFault(ctx, tx, incident.ID, fault.ID, domain.IncidentFaultPrimary, now); err != nil {
			return err
		}
		if _, err := s.incidents.UpsertRole(ctx, tx, domain.IncidentRoleAssignment{
			IncidentID: incident.ID, Role: domain.IncidentRoleCommander,
			UserID: actor.UserID, AssignedBy: actor.UserID, AssignedAt: now,
		}); err != nil {
			return err
		}
		criteria := domain.DefaultRecoveryCriteriaForFault(incident.ID, fault)
		for _, criterion := range criteria {
			if err := s.incidents.CreateRecoveryCriterion(ctx, tx, criterion); err != nil {
				return err
			}
		}
		if err := s.appendTimeline(ctx, tx, incident.ID, actor, domain.IncidentTimelineStatusChange,
			"人工宣布 Incident", map[string]any{"from": nil, "to": incident.Status, "fault_id": fault.ID}, now); err != nil {
			return err
		}
		if err := s.appendTimeline(ctx, tx, incident.ID, actor, domain.IncidentTimelineRoleAssignment,
			"设置 Incident Commander", map[string]any{"role": domain.IncidentRoleCommander, "user_id": actor.UserID}, now); err != nil {
			return err
		}
		if err := s.appendTimeline(ctx, tx, incident.ID, Actor{Display: "correlation-engine"}, domain.IncidentTimelineAutomation,
			"从 Fault 类型初始化恢复判据", map[string]any{"fault_id": fault.ID, "fault_kind": fault.Kind, "criteria_count": len(criteria)}, now); err != nil {
			return err
		}
		if err := s.queueNotification(ctx, tx, incident, domain.IncidentNotificationDeclared, now); err != nil {
			return err
		}
		created = true
		return nil
	})
	if errors.Is(err, store.ErrIncidentForFaultExists) {
		readErr := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
			var getErr error
			incident, getErr = s.incidents.GetByFaultID(ctx, tx, command.FaultID)
			return getErr
		})
		return incident, false, readErr
	}
	return incident, created, err
}

func (s *Service) List(ctx context.Context, status domain.IncidentStatus, limit int) (result []domain.Incident, err error) {
	if status != "" && !status.IsValid() {
		return nil, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		result, listErr = s.incidents.List(ctx, tx, status, limit)
		return listErr
	})
	return result, err
}

func (s *Service) Detail(ctx context.Context, id string) (detail domain.IncidentDetail, err error) {
	if id == "" {
		return detail, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var detailErr error
		detail, detailErr = s.incidents.Detail(ctx, tx, id)
		return detailErr
	})
	return detail, err
}

func (s *Service) lockOpenIncident(ctx context.Context, tx pgx.Tx, incidentID string) (domain.Incident, error) {
	incident, err := s.incidents.GetByIDForUpdate(ctx, tx, incidentID)
	if err != nil {
		return domain.Incident{}, err
	}
	if !incident.Status.IsOpen() {
		return domain.Incident{}, ErrIncidentClosed
	}
	return incident, nil
}

// LinkFault 把一个既有 Fault 纳入响应容器。重复关联同一 Incident 幂等；跨
// Incident 关联由全局唯一约束拒绝，避免两个响应团队同时拥有同一 Fault。
func (s *Service) LinkFault(ctx context.Context, incidentID, faultID string, actor Actor) (link domain.IncidentFaultLink, created bool, err error) {
	if !actor.valid() || incidentID == "" || faultID == "" {
		return link, false, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.lockOpenIncident(ctx, tx, incidentID); err != nil {
			return err
		}
		if _, err := s.faults.GetByID(ctx, tx, faultID); err != nil {
			return err
		}
		existingIncident, existingErr := s.incidents.GetByFaultID(ctx, tx, faultID)
		if existingErr == nil {
			if existingIncident.ID != incidentID {
				return store.ErrIncidentForFaultExists
			}
			var linkErr error
			link, linkErr = s.incidents.GetFaultLink(ctx, tx, incidentID, faultID)
			return linkErr
		}
		if !errors.Is(existingErr, store.ErrIncidentNotFound) {
			return existingErr
		}

		now := s.now()
		var linkErr error
		link, linkErr = s.incidents.LinkFault(ctx, tx, incidentID, faultID, domain.IncidentFaultSupporting, now)
		if linkErr != nil {
			return linkErr
		}
		if err := s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineDecision,
			"关联 Supporting Fault", map[string]any{"fault_id": faultID, "relation": link.Relation, "disposition": link.Disposition}, now); err != nil {
			return err
		}
		created = true
		return nil
	})
	return link, created, err
}

func (s *Service) SetPrimaryFault(ctx context.Context, incidentID, faultID string, actor Actor) (link domain.IncidentFaultLink, err error) {
	if !actor.valid() || incidentID == "" || faultID == "" {
		return link, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.lockOpenIncident(ctx, tx, incidentID); err != nil {
			return err
		}
		target, err := s.incidents.GetFaultLink(ctx, tx, incidentID, faultID)
		if err != nil {
			return err
		}
		if target.Disposition != domain.IncidentFaultIncluded {
			return ErrIncidentFaultExcluded
		}
		var previous string
		link, previous, err = s.incidents.SetPrimaryFault(ctx, tx, incidentID, faultID)
		if err != nil || previous == faultID {
			return err
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineDecision,
			"切换 Primary Fault", map[string]any{"from_fault_id": previous, "to_fault_id": faultID}, s.now())
	})
	return link, err
}

func (s *Service) SetFaultDisposition(ctx context.Context, incidentID, faultID string, disposition domain.IncidentFaultDisposition, rationale string, actor Actor) (link domain.IncidentFaultLink, err error) {
	rationale = strings.TrimSpace(rationale)
	if !actor.valid() || incidentID == "" || faultID == "" || !disposition.IsValid() || rationale == "" {
		return link, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.lockOpenIncident(ctx, tx, incidentID); err != nil {
			return err
		}
		current, err := s.incidents.GetFaultLink(ctx, tx, incidentID, faultID)
		if err != nil {
			return err
		}
		if current.Relation == domain.IncidentFaultPrimary && disposition == domain.IncidentFaultExcluded {
			return ErrPrimaryFaultCannotExclude
		}
		now := s.now()
		link, err = s.incidents.UpdateFaultDisposition(ctx, tx, incidentID, faultID, disposition, rationale, actor.UserID, now)
		if err != nil {
			return err
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineDecision,
			"裁决 Incident Fault 范围", map[string]any{
				"fault_id": faultID, "from": current.Disposition, "to": disposition, "rationale": rationale,
			}, now)
	})
	return link, err
}

func (s *Service) TransitionLinkedFault(ctx context.Context, incidentID, faultID string, to domain.FaultStatus, rationale string, actor Actor) (fault domain.Fault, err error) {
	rationale = strings.TrimSpace(rationale)
	if !actor.valid() || incidentID == "" || faultID == "" || rationale == "" ||
		(to != domain.FaultStatusMonitoring && to != domain.FaultStatusResolved) {
		return fault, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.lockOpenIncident(ctx, tx, incidentID); err != nil {
			return err
		}
		link, err := s.incidents.GetFaultLink(ctx, tx, incidentID, faultID)
		if err != nil {
			return err
		}
		if link.Disposition != domain.IncidentFaultIncluded {
			return ErrIncidentFaultExcluded
		}
		current, err := s.faults.GetByID(ctx, tx, faultID)
		if err != nil {
			return err
		}
		now := s.now()
		fault, err = s.faults.UpdateLifecycleStatus(ctx, tx, faultID, current.Status, to, now)
		if err != nil {
			return err
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineDecision,
			"推进关联 Fault 生命周期", map[string]any{
				"fault_id": faultID, "from": current.Status, "to": to, "rationale": rationale,
			}, now)
	})
	return fault, err
}

func (s *Service) AssignRole(ctx context.Context, incidentID string, role domain.IncidentRole, userID string, actor Actor) (assignment domain.IncidentRoleAssignment, err error) {
	if !actor.valid() || incidentID == "" || userID == "" || !role.IsValid() {
		return assignment, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.incidents.GetByID(ctx, tx, incidentID); err != nil {
			return err
		}
		now := s.now()
		var assignErr error
		assignment, assignErr = s.incidents.UpsertRole(ctx, tx, domain.IncidentRoleAssignment{
			IncidentID: incidentID, Role: role, UserID: userID,
			AssignedBy: actor.UserID, AssignedAt: now,
		})
		if assignErr != nil {
			return assignErr
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineRoleAssignment,
			"分配 Incident 角色", map[string]any{"role": role, "user_id": userID}, now)
	})
	return assignment, err
}

type CreateTaskCommand struct {
	Title       string
	Description string
	OwnerUserID string
	DueAt       time.Time
}

func (s *Service) CreateTask(ctx context.Context, incidentID string, command CreateTaskCommand, actor Actor) (task domain.IncidentTask, err error) {
	if !actor.valid() || incidentID == "" || strings.TrimSpace(command.Title) == "" {
		return task, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.incidents.GetByID(ctx, tx, incidentID); err != nil {
			return err
		}
		var createErr error
		task, createErr = s.incidents.CreateTask(ctx, tx, domain.IncidentTask{
			IncidentID: incidentID, Title: strings.TrimSpace(command.Title),
			Description: strings.TrimSpace(command.Description), Status: domain.IncidentTaskOpen,
			OwnerUserID: command.OwnerUserID, CreatedBy: actor.UserID, DueAt: command.DueAt,
		})
		if createErr != nil {
			return createErr
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineTaskUpdate,
			"创建响应任务", map[string]any{"task_id": task.ID, "title": task.Title, "owner_user_id": task.OwnerUserID}, s.now())
	})
	return task, err
}

func (s *Service) UpdateTask(ctx context.Context, incidentID, taskID string, status domain.IncidentTaskStatus, ownerUserID string, actor Actor) (task domain.IncidentTask, err error) {
	if !actor.valid() || incidentID == "" || taskID == "" || !status.IsValid() {
		return task, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var updateErr error
		now := s.now()
		task, updateErr = s.incidents.UpdateTask(ctx, tx, incidentID, taskID, status, ownerUserID, now)
		if updateErr != nil {
			return updateErr
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineTaskUpdate,
			"更新响应任务", map[string]any{"task_id": task.ID, "status": task.Status, "owner_user_id": task.OwnerUserID}, now)
	})
	return task, err
}

func (s *Service) AddHumanTimeline(ctx context.Context, incidentID string, kind domain.IncidentTimelineKind, summary string, detail map[string]any, occurredAt time.Time, actor Actor) (entry domain.IncidentTimelineEntry, err error) {
	if !actor.valid() || incidentID == "" || strings.TrimSpace(summary) == "" ||
		(kind != domain.IncidentTimelineFact && kind != domain.IncidentTimelineHypothesis && kind != domain.IncidentTimelineDecision) {
		return entry, ErrInvalidInput
	}
	if occurredAt.IsZero() {
		occurredAt = s.now()
	}
	entry = domain.IncidentTimelineEntry{IncidentID: incidentID, ActorUserID: actor.UserID,
		ActorDisplay: actor.Display, Kind: kind, Summary: strings.TrimSpace(summary), Detail: detail, OccurredAt: occurredAt}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.incidents.GetByID(ctx, tx, incidentID); err != nil {
			return err
		}
		var appendErr error
		entry.ID, appendErr = s.incidents.AppendIncidentTimeline(ctx, tx, entry)
		return appendErr
	})
	return entry, err
}

func (s *Service) UpdateRecoveryCriterion(ctx context.Context, incidentID, key string, status domain.RecoveryCriterionStatus, evidence map[string]any, observedAt time.Time, actor Actor) (criterion domain.RecoveryCriterion, err error) {
	if !actor.valid() || incidentID == "" || key == "" || !status.IsValid() ||
		(status == domain.RecoveryCriterionPassed && (len(evidence) == 0 || observedAt.IsZero())) {
		return criterion, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.incidents.GetByID(ctx, tx, incidentID); err != nil {
			return err
		}
		var updateErr error
		criterion, updateErr = s.incidents.UpdateRecoveryCriterion(ctx, tx, incidentID, key, status, evidence, actor.UserID, observedAt)
		if updateErr != nil {
			return updateErr
		}
		return s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineAutomation,
			"更新恢复判据", map[string]any{"criterion_key": key, "status": status, "evidence": evidence}, s.now())
	})
	return criterion, err
}

func (s *Service) Transition(ctx context.Context, incidentID string, to domain.IncidentStatus, actor Actor) (incident domain.Incident, err error) {
	if !actor.valid() || incidentID == "" || !to.IsValid() {
		return incident, ErrInvalidInput
	}
	err = s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		current, getErr := s.incidents.GetByIDForUpdate(ctx, tx, incidentID)
		if getErr != nil {
			return getErr
		}
		criteria, listErr := s.incidents.ListRecoveryCriteria(ctx, tx, incidentID)
		if listErr != nil {
			return listErr
		}
		tasks, listErr := s.incidents.ListTasks(ctx, tx, incidentID)
		if listErr != nil {
			return listErr
		}
		if err := domain.ValidateIncidentTransition(current.Status, to, criteria, tasks); err != nil {
			return err
		}

		links, listErr := s.incidents.ListFaultLinks(ctx, tx, incidentID)
		if listErr != nil {
			return listErr
		}
		linkedFaults := make([]domain.Fault, 0, len(links))
		lifecycles := make([]domain.IncidentFaultLifecycle, 0, len(links))
		for _, link := range links {
			fault, faultErr := s.faults.GetByID(ctx, tx, link.FaultID)
			if faultErr != nil {
				return faultErr
			}
			linkedFaults = append(linkedFaults, fault)
			lifecycles = append(lifecycles, domain.IncidentFaultLifecycle{
				FaultID: fault.ID, Disposition: link.Disposition, Status: fault.Status,
			})
		}
		if to == domain.IncidentStatusResolved {
			if err := domain.ValidateIncidentFaultsForResolution(lifecycles); err != nil {
				return err
			}
		}

		now := s.now()
		incident, getErr = s.incidents.UpdateStatus(ctx, tx, incidentID, current.Status, to, now)
		if getErr != nil {
			return getErr
		}
		transitionedFaultIDs := make([]string, 0, len(links))
		for index, link := range links {
			if link.Disposition != domain.IncidentFaultIncluded {
				continue
			}
			fault := linkedFaults[index]
			var target domain.FaultStatus
			switch {
			case to == domain.IncidentStatusMonitoring && fault.Status == domain.FaultStatusActive:
				target = domain.FaultStatusMonitoring
			case to == domain.IncidentStatusResolved && fault.Status == domain.FaultStatusMonitoring:
				target = domain.FaultStatusResolved
			default:
				continue
			}
			if _, faultErr := s.faults.UpdateLifecycleStatus(ctx, tx, fault.ID, fault.Status, target, now); faultErr != nil {
				return faultErr
			}
			transitionedFaultIDs = append(transitionedFaultIDs, fault.ID)
		}
		if err := s.appendTimeline(ctx, tx, incidentID, actor, domain.IncidentTimelineStatusChange,
			"推进 Incident 状态", map[string]any{"from": current.Status, "to": to, "transitioned_fault_ids": transitionedFaultIDs}, now); err != nil {
			return err
		}
		switch to {
		case domain.IncidentStatusMonitoring:
			return s.queueNotification(ctx, tx, incident, domain.IncidentNotificationMitigated, now)
		case domain.IncidentStatusResolved:
			return s.queueNotification(ctx, tx, incident, domain.IncidentNotificationResolved, now)
		default:
			return nil
		}
	})
	return incident, err
}

func (s *Service) queueNotification(ctx context.Context, tx pgx.Tx, incident domain.Incident, event domain.IncidentNotificationEvent, at time.Time) error {
	if s.notifier == nil {
		return fmt.Errorf("incident: 未配置通知 outbox")
	}
	if err := s.notifier.QueueIncidentNotification(ctx, tx, domain.IncidentNotification{Incident: incident, Event: event, OccurredAt: at}); err != nil {
		return err
	}
	return s.appendTimeline(ctx, tx, incident.ID, Actor{Display: "incident-outbox"}, domain.IncidentTimelineCommunication,
		"已排队 Incident 状态通报", map[string]any{"event": event, "channel": incident.CommunicationChannel}, at)
}

func (s *Service) appendTimeline(ctx context.Context, tx pgx.Tx, incidentID string, actor Actor, kind domain.IncidentTimelineKind, summary string, detail map[string]any, at time.Time) error {
	_, err := s.incidents.AppendIncidentTimeline(ctx, tx, domain.IncidentTimelineEntry{
		IncidentID: incidentID, ActorUserID: actor.UserID, ActorDisplay: actor.Display,
		Kind: kind, Summary: summary, Detail: detail, OccurredAt: at,
	})
	return err
}
