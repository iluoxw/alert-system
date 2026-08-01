package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"alert-system/internal/events"
)

func newEventsTestRouter(t *testing.T) (router *Router, bus *events.Bus, cookie *http.Cookie) {
	t.Helper()
	router, _, users, _, _ := newTestRouter(time.Hour, time.Hour)
	addTestUser(users, "u1", "member1", "correct-password", true)
	cookie = doLogin(t, router, "member1", "correct-password")

	bus = events.NewBus()
	router.RegisterEventRoutes(bus)
	return router, bus, cookie
}

func TestEvents_Unauthenticated_Returns401(t *testing.T) {
	router, _, _ := newEventsTestRouter(t)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestEvents_Authenticated_StreamsAlertChanged 验证登录后的 SSE 长连接：需要真实
// HTTP 连接（而非 httptest.ResponseRecorder）才能在 handler 阻塞期间并发读取已写
// 出的字节，所以用 httptest.NewServer 包 router。
func TestEvents_Authenticated_StreamsAlertChanged(t *testing.T) {
	router, bus, cookie := newEventsTestRouter(t)

	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)

	// 先读掉连接建立时的 ": connected" 注释行与空行。
	readLineOrFatal(t, reader, ": connected")
	readLineOrFatal(t, reader, "")

	// 给 handler 一点时间完成 Subscribe（在处理函数最开头执行），避免 Publish
	// 发生在 Subscribe 之前而丢失这条信号。
	time.Sleep(100 * time.Millisecond)
	bus.Publish("alert-abc-123")

	readLineOrFatal(t, reader, "event: alert_changed")
	readLineOrFatal(t, reader, `data: {"alert_id":"alert-abc-123"}`)
	readLineOrFatal(t, reader, "")
}

func readLineOrFatal(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取行失败: %v", err)
	}
	got := line
	if len(got) > 0 && got[len(got)-1] == '\n' {
		got = got[:len(got)-1]
	}
	if got != want {
		t.Fatalf("读到行 %q，期望 %q", got, want)
	}
}
