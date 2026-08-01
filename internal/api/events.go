package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"alert-system/internal/events"
)

// eventsHeartbeatInterval 是 SSE 心跳周期，避免反向代理（Nginx 等）因连接长期无
// 数据流动而按 idle 超时主动断开。
const eventsHeartbeatInterval = 25 * time.Second

// RegisterEventRoutes 挂载 GET /api/v1/events（R：告警变更实时推送）。鉴权口径与
// 告警读接口一致——只需登录，不要求 admin（推送的只是 alert_id 信号，具体数据仍要
// 走 REST /alerts/:id 重拉，鉴权与数据过滤全部落在那条既有路径上，这里不重复判断）。
func (r *Router) RegisterEventRoutes(bus *events.Bus) *Router {
	authed := r.engine.Group("/api/v1")
	authed.Use(r.sessionAuth())
	authed.GET("/events", func(c *gin.Context) { r.handleEvents(c, bus) })
	return r
}

// handleEvents 处理 SSE 长连接：订阅 bus，把每条 alert_changed 信号转成一行 SSE
// event 推给客户端；定期心跳防代理断连；客户端断开（请求 ctx done）时退订退出。
// 不逐事件记日志——alert_id 无敏感内容，但事件量可能很大，只在连接建立/断开各记
// 一条，避免日志刷屏。
func (r *Router) handleEvents(c *gin.Context, bus *events.Bus) {
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 先写一条注释行并 flush，让 EventSource 尽快进入 open 态（浏览器在收到首个
	// 字节前不会触发 onopen）。
	fmt.Fprint(w, ": connected\n\n")
	w.Flush()

	r.logger.Info("api: SSE 连接建立", "remote", c.Request.RemoteAddr)
	defer r.logger.Info("api: SSE 连接断开", "remote", c.Request.RemoteAddr)

	ticker := time.NewTicker(eventsHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case alertID := <-ch:
			fmt.Fprintf(w, "event: alert_changed\ndata: {\"alert_id\":%q}\n\n", alertID)
			w.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			w.Flush()
		}
	}
}
