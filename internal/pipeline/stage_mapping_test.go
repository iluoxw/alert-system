package pipeline

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
)

// fakeAlertProjector 是 AlertProjector 的可编程测试替身，固定返回预设结果。
type fakeAlertProjector struct {
	result domain.AlertMappingResult
	err    error
	calls  int
}

func (f *fakeAlertProjector) Project(ctx context.Context, tx pgx.Tx, alert domain.Alert) (domain.AlertMappingResult, error) {
	f.calls++
	return f.result, f.err
}

// TestMappingStage_FaultActivationTriggered 覆盖 R5.10 的 webhook 实时路径：
// Project 返回 CreatedActive/EnteredActive 时必须调用 faultActivation 钩子。
func TestMappingStage_FaultActivationTriggered(t *testing.T) {
	for _, transition := range []domain.FaultTransition{domain.FaultTransitionCreatedActive, domain.FaultTransitionEnteredActive} {
		t.Run(string(transition), func(t *testing.T) {
			projector := &fakeAlertProjector{result: domain.AlertMappingResult{FaultID: "fault-1", Transition: transition}}
			var triggered []domain.AlertMappingResult
			stage := NewMappingStage(projector).WithFaultActivation(func(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error {
				triggered = append(triggered, result)
				return nil
			})
			pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-1"}}
			if err := stage.Process(context.Background(), pctx); err != nil {
				t.Fatalf("Process 失败: %v", err)
			}
			if len(triggered) != 1 || triggered[0].FaultID != "fault-1" {
				t.Fatalf("faultActivation 钩子应被调用一次并收到 Project 的结果，got %+v", triggered)
			}
		})
	}
}

// TestMappingStage_FaultActivationNotTriggeredOnNoneTransition 覆盖 Transition=None
// 场景（已经是 active 的重复评估、或结果不是 active）：钩子仍会被调用（判定逻辑
// 集中在钩子内部，见 internal/app.Build 的 faultActivationTrigger），但钩子内部
// 应该据 Transition 判断是否真的触发通知——本测试验证 stage 层无条件透传，不做
// 任何过滤。
func TestMappingStage_FaultActivationCalledEvenOnNoneTransition(t *testing.T) {
	projector := &fakeAlertProjector{result: domain.AlertMappingResult{FaultID: "fault-2", Transition: domain.FaultTransitionNone}}
	calls := 0
	stage := NewMappingStage(projector).WithFaultActivation(func(ctx context.Context, tx pgx.Tx, result domain.AlertMappingResult) error {
		calls++
		return nil
	})
	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-2"}}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("Process 失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("stage 应无条件调用钩子一次，把过滤判断留给钩子实现，got %d", calls)
	}
}

// TestMappingStage_NoFaultActivationConfigured 覆盖未注入钩子时的默认行为：
// 不 panic，Process 正常返回。
func TestMappingStage_NoFaultActivationConfigured(t *testing.T) {
	projector := &fakeAlertProjector{result: domain.AlertMappingResult{FaultID: "fault-3", Transition: domain.FaultTransitionCreatedActive}}
	stage := NewMappingStage(projector)
	pctx := &PipelineContext{Alert: domain.Alert{ID: "alert-3"}}
	if err := stage.Process(context.Background(), pctx); err != nil {
		t.Fatalf("未配置钩子时 Process 不应报错: %v", err)
	}
}
