package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// corsMiddleware 仅允许配置中列出的精确 origin，并允许浏览器携带 session cookie。
// 不支持通配符；未配置时完全不添加 CORS 响应头，保持同源默认策略。
func corsMiddleware(allowedOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if _, ok := allowed[origin]; !ok {
			c.Next()
			return
		}

		headers := c.Writer.Header()
		headers.Set("Access-Control-Allow-Origin", origin)
		headers.Set("Access-Control-Allow-Credentials", "true")
		headers.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		headers.Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
		headers.Set("Access-Control-Max-Age", "600")
		headers.Add("Vary", "Origin")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
