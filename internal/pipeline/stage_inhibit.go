package pipeline

import "context"

// InhibitStage 是管道的抑制 stage 占位实现（R5 抑制规则，二期功能）。本期恒返回
// nil（不抑制任何告警），只把接口占位卡进管道顺序里，使编排顺序（去重->路由->分组
// ->静默->抑制）在二期真正实现抑制规则求值前就已经固定，届时只需替换本文件内容，
// 不需要改动编排器或其他 stage。
type InhibitStage struct{}

// NewInhibitStage 构造 InhibitStage。
func NewInhibitStage() *InhibitStage {
	return &InhibitStage{}
}

// Name 返回 "inhibit"。
func (s *InhibitStage) Name() string { return "inhibit" }

// Process 实现 Stage 接口：本期空实现，恒不中断管道、不修改 pctx。
func (s *InhibitStage) Process(ctx context.Context, pctx *PipelineContext) error {
	return nil
}
