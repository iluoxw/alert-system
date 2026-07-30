package app

import (
	"context"

	"github.com/jackc/pgx/v5"

	"alert-system/internal/notify"
	"alert-system/internal/oncall"
)

// CompositeStopper 同时满足 api.EscalationStopper 与 internal/notify 包内未导出的
// lifecycleEscalationStopper（结构性满足，两者签名相同）：把 oncall.Chain.Stop 与
// notify.Engine.Stop 组合成一次调用——Ack/Resolve 只停人员升级链会留下渠道升级的
// pending 定时器（虽然 advance 内部的 shouldContinueEscalation 会让它到期时幂等
// no-op，不会误发，但应当主动停干净，不依赖"到期后自愈"）。两者的 Stop 方法签名
// 完全一致，都是幂等的。
type CompositeStopper struct {
	chain  *oncall.Chain
	engine *notify.Engine
}

// NewCompositeStopper 构造 CompositeStopper。
func NewCompositeStopper(chain *oncall.Chain, engine *notify.Engine) CompositeStopper {
	return CompositeStopper{chain: chain, engine: engine}
}

// Stop 依次停止人员升级链与渠道升级序列，两者均幂等。
func (s CompositeStopper) Stop(ctx context.Context, tx pgx.Tx, alertID string) error {
	if err := s.chain.Stop(ctx, tx, alertID); err != nil {
		return err
	}
	return s.engine.Stop(ctx, tx, alertID)
}
