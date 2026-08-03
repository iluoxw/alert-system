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
type MappingStage struct{ projector AlertProjector }

func NewMappingStage(projector AlertProjector) *MappingStage {
	return &MappingStage{projector: projector}
}
func (s *MappingStage) Name() string                    { return "alert_mapping" }
func (s *MappingStage) RunForAuthoritativeSource() bool { return true }

func (s *MappingStage) Process(ctx context.Context, pctx *PipelineContext) error {
	if s.projector == nil || pctx.Alert.ID == "" {
		return nil
	}
	if _, err := s.projector.Project(ctx, pctx.Tx, pctx.Alert); err != nil {
		return fmt.Errorf("alert mapping: %w", err)
	}
	return nil
}
