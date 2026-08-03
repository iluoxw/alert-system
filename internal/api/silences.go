// 静默（silence）的 admin CRUD（任务 27，R10、R10.5）。挂载入口是
// configHandlers.registerSilenceRoutes，由 config.go 的 RegisterConfigRoutes 调用，
// 与 integrations/teams/... 共用同一个 "/api/v1/admin" 分组与鉴权链。
package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"alert-system/internal/domain"
	"alert-system/internal/store"
)

func (h *configHandlers) registerSilenceRoutes(admin gin.IRoutes) {
	admin.POST("/silences", h.createSilence)
	admin.GET("/silences", h.listSilences)
	admin.GET("/silences/:id", h.getSilence)
	admin.PATCH("/silences/:id", h.updateSilence)
	admin.DELETE("/silences/:id", h.deleteSilence)
}

func silenceJSON(s store.Silence) gin.H {
	return gin.H{
		"id":         s.ID,
		"matchers":   s.Matchers,
		"starts_at":  s.StartsAt,
		"ends_at":    s.EndsAt,
		"created_by": s.CreatedBy,
		"comment":    s.Comment,
		"created_at": s.CreatedAt,
		"updated_at": s.UpdatedAt,
	}
}

type silenceRequest struct {
	Matchers domain.MatcherSet `json:"matchers"`
	StartsAt time.Time         `json:"starts_at"`
	EndsAt   time.Time         `json:"ends_at"`
	Comment  string            `json:"comment"`
}

// validateSilence 校验时间窗合法性（ends_at 严格晚于 starts_at，与 silences 表的
// CHECK (ends_at > starts_at) 对齐，API 层提前拒绝以给出明确错误信息而不是等 DB
// 报约束错误）与 matchers 非空（留白 matchers 会静默一切告警，属于误操作风险极高
// 的配置，本任务在 silences 上专门拒绝——不同于 routes/notification policies，
// 后者的空 matchers 是"全局默认"的合法语义，两者不能一概而论）。
func validateSilence(req silenceRequest) (string, bool) {
	if len(req.Matchers) == 0 {
		return "matchers 不能为空", false
	}
	if err := req.Matchers.Validate(); err != nil {
		return "matchers 不合法: " + err.Error(), false
	}
	if !req.EndsAt.After(req.StartsAt) {
		return "ends_at 必须晚于 starts_at", false
	}
	return "", true
}

func (h *configHandlers) createSilence(c *gin.Context) {
	var req silenceRequest
	if !bindJSON(c, &req) {
		return
	}
	if msg, ok := validateSilence(req); !ok {
		unprocessable(c, msg)
		return
	}
	actor, ok := UserFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	ctx := c.Request.Context()
	var out *store.Silence
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var createErr error
		out, createErr = h.deps.Silences.Create(ctx, tx, store.Silence{
			Matchers:  req.Matchers,
			StartsAt:  req.StartsAt,
			EndsAt:    req.EndsAt,
			CreatedBy: actor.UserID,
			Comment:   req.Comment,
		})
		return createErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "创建静默")
		return
	}
	h.invalidateSilences()
	c.JSON(http.StatusCreated, silenceJSON(*out))
}

func (h *configHandlers) listSilences(c *gin.Context) {
	ctx := c.Request.Context()
	var list []store.Silence
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var listErr error
		list, listErr = h.deps.Silences.List(ctx, tx, queryLimit(c))
		return listErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "列出静默")
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, s := range list {
		items = append(items, silenceJSON(s))
	}
	c.JSON(http.StatusOK, gin.H{"silences": items})
}

func (h *configHandlers) getSilence(c *gin.Context) {
	ctx := c.Request.Context()
	var out *store.Silence
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var getErr error
		out, getErr = h.deps.Silences.GetByID(ctx, tx, c.Param("id"))
		return getErr
	})
	if err != nil {
		h.handleStoreErr(c, err, "查询静默")
		return
	}
	c.JSON(http.StatusOK, silenceJSON(*out))
}

func (h *configHandlers) updateSilence(c *gin.Context) {
	var req silenceRequest
	if !bindJSON(c, &req) {
		return
	}
	if msg, ok := validateSilence(req); !ok {
		unprocessable(c, msg)
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return h.deps.Silences.Update(ctx, tx, store.Silence{
			ID:       id,
			Matchers: req.Matchers,
			StartsAt: req.StartsAt,
			EndsAt:   req.EndsAt,
			Comment:  req.Comment,
		})
	})
	if err != nil {
		h.handleStoreErr(c, err, "更新静默")
		return
	}
	h.invalidateSilences()
	c.Status(http.StatusNoContent)
}

// deleteSilence 实现 R10.5：提前删除静默必须立即恢复被它标记静默的告警的通知——
// 在同一事务内先清理 alerts.silenced_by 里对该静默 ID 的引用，再删除静默本身
// （顺序其实无所谓，反正同事务原子提交/回滚；这里先 detach 再 delete，便于阅读：
// "先解除引用，再删除被引用的规则"）。已知可接受缝隙：SilenceStage 的 10s TTL
// 规则缓存内，尚未到达的新事件仍可能被这条已删除的静默重新标记，最长 10s 自愈，
// 不在本任务处理范围（见 store.AlertRepo.DetachSilence 的注释）。
func (h *configHandlers) deleteSilence(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.tx.WithTx(ctx, func(tx pgx.Tx) error {
		if err := h.deps.AlertSilences.DetachSilence(ctx, tx, id); err != nil {
			return err
		}
		return h.deps.Silences.Delete(ctx, tx, id)
	})
	if err != nil {
		h.handleStoreErr(c, err, "删除静默")
		return
	}
	h.invalidateSilences()
	c.Status(http.StatusNoContent)
}
