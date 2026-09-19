package pipeline

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

type AlertProjector interface {
	Project(ctx context.Context, tx pgx.Tx, alert domain.Alert) (domain.AlertMappingResult, error)
}

// MappingStage is intentionally authoritative-safe: it projects facts into the
// SRE domain but never routes, suppresses or sends Alert-level notifications.
type MappingStage struct {
	projector AlertProjector
	// faultActivation 在 Project 返回后被无条件调用（若已配置），由调用方
	// （internal/app.Build）决定 result.Transition 是否需要触发 Fault 通知
	// （R5.10）——判定逻辑刻意不下沉进本 stage 或 projector：projector 当前零
	// 通知依赖是不变量 4/9 成立的基础，本 stage 也不应替代 app 层做通知门槛判断。
	faultActivation func(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error
}

func NewMappingStage(projector AlertProjector) *MappingStage {
	return &MappingStage{projector: projector}
}

// WithFaultActivation 注入 Fault 激活通知的触发钩子（R5.10：webhook 实时入队路径）。
func (s *MappingStage) WithFaultActivation(fn func(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error) *MappingStage {
	s.faultActivation = fn
	return s
}

func (s *MappingStage) Name() string                    { return "alert_mapping" }
func (s *MappingStage) RunForAuthoritativeSource() bool { return true }

func (s *MappingStage) Process(ctx context.Context, pctx *PipelineContext) error {
	if s.projector == nil || pctx.Alert.ID == "" {
		return nil
	}
	result, err := s.projector.Project(ctx, pctx.Tx, pctx.Alert)
	if err != nil {
		return fmt.Errorf("alert mapping: %w", err)
	}
	if s.faultActivation != nil {
		if err := s.faultActivation(ctx, pctx.Tx, result); err != nil {
			return fmt.Errorf("fault activation: %w", err)
		}
	}
	return nil
}
