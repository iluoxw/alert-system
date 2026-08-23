package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- endpoint 校验 ----

func TestParseEndpointAcceptsEmptyAsNotConnected(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t"} {
		endpoint, err := ParseEndpoint(raw, ModeProduction)
		if err != nil {
			t.Fatalf("空 endpoint 应合法（未连接），得到 %v", err)
		}
		if !endpoint.IsZero() {
			t.Fatal("空 endpoint 应是零值")
		}
		if endpoint.String() != "<not-configured>" {
			t.Fatalf("String()=%q", endpoint.String())
		}
	}
}

func TestParseEndpointRejectsUnsafeShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"非 http scheme", "ftp://metrics.internal"},
		{"file scheme", "file:///etc/passwd"},
		{"缺 host", "https:///api"},
		{"只有 scheme", "https://"},
		{"userinfo", "https://user:pass@metrics.internal"},
		{"仅用户名的 userinfo", "https://user@metrics.internal"},
		{"带 query", "https://metrics.internal/api?org=default"},
		{"强制 query", "https://metrics.internal/api?"},
		{"带 fragment", "https://metrics.internal/api#frag"},
		{"控制字符", "https://metrics.internal/\napi"},
		{"NUL", "https://metrics.internal/\x00api"},
		{"生产模式的 http", "http://metrics.internal"},
		{"无 scheme", "metrics.internal"},
		{"scheme-relative", "//metrics.internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEndpoint(tc.raw, ModeProduction); err == nil {
				t.Fatalf("%q 应被拒绝", tc.raw)
			}
		})
	}
}

func TestParseEndpointAcceptsSafeProductionShapes(t *testing.T) {
	for _, raw := range []string{
		"https://metrics.internal",
		"https://metrics.internal/",
		"https://metrics.internal:9090",
		"https://metrics.internal/prometheus",
		"https://metrics.internal/prometheus/",
		"HTTPS://Metrics.Internal/prometheus",
	} {
		t.Run(raw, func(t *testing.T) {
			endpoint, err := ParseEndpoint(raw, ModeProduction)
			if err != nil {
				t.Fatalf("%q 应被接受: %v", raw, err)
			}
			if endpoint.IsZero() {
				t.Fatal("应是已配置")
			}
			// String 只暴露 scheme+host，不带 path：path 可能含 org/stream 拓扑信息。
			if strings.Contains(endpoint.String(), "prometheus") {
				t.Fatalf("String()=%q 泄露了 path", endpoint.String())
			}
		})
	}
}

// 测试模式只额外允许 loopback 上的 http，且必须由服务端显式构造。
func TestLoopbackTestModeOnlyAllowsLoopbackHTTP(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8080", "http://localhost:9090", "http://[::1]:9090"} {
		if _, err := ParseEndpoint(raw, ModeLoopbackTest); err != nil {
			t.Fatalf("%q 在测试模式应被接受: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://metrics.internal", "http://10.0.0.1", "http://example.com"} {
		if _, err := ParseEndpoint(raw, ModeLoopbackTest); err == nil {
			t.Fatalf("%q 即使在测试模式也不应被接受（非 loopback 明文）", raw)
		}
	}
	// 生产模式下 loopback http 同样被拒绝：模式是服务端决定的，不能被"反正是本机"绕过。
	if _, err := ParseEndpoint("http://127.0.0.1:9090", ModeProduction); err == nil {
		t.Fatal("生产模式不应接受任何明文 endpoint")
	}
}

// ---- 凭据脱敏 ----

func TestSecretNeverLeaksThroughAnyFormatting(t *testing.T) {
	const plaintext = "super-secret-token-value"
	secret := NewSecret(plaintext)

	if secret.Reveal() != plaintext {
		t.Fatal("Reveal 应返回明文")
	}
	renderings := map[string]string{
		"String":   secret.String(),
		"GoString": secret.GoString(),
		"%v":       fmt.Sprintf("%v", secret),
		"%s":       fmt.Sprintf("%s", secret),
		"%q":       fmt.Sprintf("%q", secret),
		"%+v":      fmt.Sprintf("%+v", secret),
		"%#v":      fmt.Sprintf("%#v", secret),
		// 最现实的泄露路径：把整个配置结构体打进日志。
		"struct %+v": fmt.Sprintf("%+v", struct {
			Endpoint string
			Token    Secret
		}{Endpoint: "https://metrics.internal", Token: secret}),
		"struct %#v": fmt.Sprintf("%#v", struct{ Token Secret }{Token: secret}),
	}
	for name, rendered := range renderings {
		if strings.Contains(rendered, plaintext) {
			t.Fatalf("%s 泄露了明文: %s", name, rendered)
		}
	}

	// JSON 序列化（配置结构体被整体序列化时最容易出事）。
	encoded, err := json.Marshal(struct{ Token Secret }{Token: secret})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), plaintext) {
		t.Fatalf("JSON 泄露了明文: %s", encoded)
	}

	// slog 结构化输出。
	var builder strings.Builder
	logger := slog.New(slog.NewTextHandler(&builder, nil))
	logger.Info("config", "token", secret)
	if strings.Contains(builder.String(), plaintext) {
		t.Fatalf("slog 泄露了明文: %s", builder.String())
	}

	if !NewSecret("").IsZero() {
		t.Fatal("空 secret 应是零值")
	}
	if NewSecret("").String() != "<empty>" {
		t.Fatalf("空 secret 的 String()=%q", NewSecret("").String())
	}
}

// ---- 客户端行为 ----

func newTestClient(t *testing.T, server *httptest.Server, mutate func(*ClientOptions)) *Client {
	t.Helper()
	endpoint, err := ParseEndpoint(server.URL, ModeLoopbackTest)
	if err != nil {
		t.Fatalf("解析测试 endpoint 失败: %v", err)
	}
	opts := ClientOptions{
		Endpoint:         endpoint,
		Timeout:          2 * time.Second,
		MaxResponseBytes: 1024,
		MaxConcurrent:    4,
		Header:           http.Header{"Authorization": []string{"Bearer test-token"}},
	}
	if mutate != nil {
		mutate(&opts)
	}
	client, err := NewClient(opts)
	if err != nil {
		t.Fatalf("构造 client 失败: %v", err)
	}
	if client == nil {
		t.Fatal("已配置 endpoint 时不应返回 nil client")
	}
	return client
}

func TestNewClientReturnsNilWhenNotConfigured(t *testing.T) {
	client, err := NewClient(ClientOptions{})
	if err != nil {
		t.Fatalf("未配置不应报错: %v", err)
	}
	if client != nil {
		t.Fatal("未配置时应返回 nil client，让调用方判定为未连接")
	}
}

// 固定 header（含凭据）必须被注入，且只走 header 这一条通道。
func TestFixedHeaderIsInjected(t *testing.T) {
	var gotAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, nil)
	if _, err := client.Get(context.Background(), "/api/v1/query", url.Values{"query": {"up"}}); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if got := gotAuth.Load(); got != "Bearer test-token" {
		t.Fatalf("Authorization=%v", got)
	}
}

// 表单/查询参数必须走 encoder：值里的 & 与 = 不得改变请求语义。
func TestParametersAreEncodedNotConcatenated(t *testing.T) {
	var gotQuery atomic.Value
	var gotBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			buf := make([]byte, 512)
			n, _ := r.Body.Read(buf)
			gotBody.Store(string(buf[:n]))
		} else {
			gotQuery.Store(r.URL.Query().Get("query"))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, nil)
	hostile := `up{job="x"} & other=1`
	if _, err := client.Get(context.Background(), "/api/v1/query", url.Values{"query": {hostile}}); err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	if got := gotQuery.Load(); got != hostile {
		t.Fatalf("查询参数被改写: %v", got)
	}
	if _, err := client.PostForm(context.Background(), "/api/v1/query", url.Values{"query": {hostile}}); err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	body, _ := gotBody.Load().(string)
	if !strings.Contains(body, url.QueryEscape(hostile)) {
		t.Fatalf("form body 未正确编码: %q", body)
	}
}

// 重定向一律拒绝：跨 host 是 SSRF，同 host 换路径是语义漂移。
func TestRedirectsAreBlocked(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"leaked":true}`))
	}))
	defer target.Close()

	cases := map[string]string{
		"跨 host":     target.URL + "/api",
		"同 host 换路径": "/somewhere-else",
		"协议相对":       "//evil.example.com/x",
	}
	for name, location := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusFound)
			}))
			defer server.Close()

			client := newTestClient(t, server, nil)
			_, err := client.Get(context.Background(), "/api/v1/query", nil)
			if !errors.Is(err, ErrRedirectBlocked) {
				t.Fatalf("err=%v，want ErrRedirectBlocked", err)
			}
			// 错误信息只带 host，不带完整 URL 或 Authorization。
			if strings.Contains(err.Error(), "Bearer") || strings.Contains(err.Error(), "query=") {
				t.Fatalf("错误信息泄露了敏感内容: %v", err)
			}
		})
	}
}

// 响应体超上界必须显式失败，而不是截断出一个"看起来完整"的结果。
func TestOversizedResponseFailsExplicitly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 2048)))
	}))
	defer server.Close()

	client := newTestClient(t, server, func(opts *ClientOptions) { opts.MaxResponseBytes = 1024 })
	_, err := client.Get(context.Background(), "/api/v1/query", nil)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err=%v，want ErrResponseTooLarge", err)
	}
}

// 恰好等于上界必须被接受：多读一个字节判断"严格超过"。
func TestResponseExactlyAtLimitIsAccepted(t *testing.T) {
	const limit = 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", limit)))
	}))
	defer server.Close()

	client := newTestClient(t, server, func(opts *ClientOptions) { opts.MaxResponseBytes = limit })
	response, err := client.Get(context.Background(), "/api/v1/query", nil)
	if err != nil {
		t.Fatalf("恰好等于上界应被接受: %v", err)
	}
	if len(response.Body) != limit {
		t.Fatalf("body 长度=%d，want %d", len(response.Body), limit)
	}
}

// context 取消必须立刻终止在途请求并返回 context 错误。
func TestContextCancellationTerminatesRequest(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		case <-time.After(3 * time.Second):
		}
	}))
	defer func() {
		close(released)
		server.Close()
	}()

	client := newTestClient(t, server, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Get(ctx, "/api/v1/query", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v，want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("取消没有立刻生效，用了 %s", elapsed)
	}
}

// 并发闸门必须真的限制在途请求数，且排队本身可取消。
func TestConcurrencyIsBounded(t *testing.T) {
	var inFlight atomic.Int64
	var peak atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := inFlight.Add(1)
		for {
			observed := peak.Load()
			if current <= observed || peak.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, func(opts *ClientOptions) { opts.MaxConcurrent = 2 })
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Get(context.Background(), "/api/v1/query", nil); err != nil {
				t.Errorf("请求失败: %v", err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("在途请求峰值=%d，超过 MaxConcurrent=2", peak.Load())
	}
}

func TestQueuedRequestIsCancellable(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{}`))
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := newTestClient(t, server, func(opts *ClientOptions) { opts.MaxConcurrent = 1 })
	// 占住唯一的 slot。
	go func() { _, _ = client.Get(context.Background(), "/api/v1/query", nil) }()
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Get(ctx, "/api/v1/query", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v，排队中的请求应可取消", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("排队取消没有立刻生效，用了 %s", elapsed)
	}
}

// 上游返回 5xx 时错误信息不得回显 URL/凭据；状态码原样交给调用方判断。
func TestUpstreamStatusIsReturnedWithoutLeakingRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, nil)
	response, err := client.Get(context.Background(), "/api/v1/query", url.Values{"query": {"secret-focus-value"}})
	if err != nil {
		t.Fatalf("5xx 不应变成传输错误: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode=%d", response.StatusCode)
	}
}

// 连接失败的错误信息只带 host，不带完整 URL、query 或 Authorization。
func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	endpoint, err := ParseEndpoint("http://127.0.0.1:1", ModeLoopbackTest)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	client, err := NewClient(ClientOptions{
		Endpoint:         endpoint,
		Timeout:          200 * time.Millisecond,
		MaxResponseBytes: 1024,
		MaxConcurrent:    1,
		Header:           http.Header{"Authorization": []string{"Bearer leaky-token"}},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	_, err = client.Get(context.Background(), "/api/v1/query", url.Values{"query": {"sbx_secret"}})
	if err == nil {
		t.Fatal("应连接失败")
	}
	for _, leak := range []string{"leaky-token", "sbx_secret", "query=", "/api/v1/query"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误信息泄露 %q: %v", leak, err)
		}
	}
}

// 默认 Transport 不继承环境代理：代理属于部署配置，不该被环境变量隐式改写。
func TestDefaultTransportIgnoresEnvironmentProxy(t *testing.T) {
	transport := defaultTransport()
	if transport.Proxy != nil {
		t.Fatal("默认 Transport 不应设置 Proxy")
	}
	if transport.MaxConnsPerHost <= 0 {
		t.Fatal("默认 Transport 必须限制每 host 连接数")
	}
}

// PostJSON 走与 PostForm/Get 相同的安全通道：Content-Type 固定、context 可取消、错误不回显 body。
func TestPostJSONUsesJSONContentTypeAndRedactsErrors(t *testing.T) {
	const wantBody = `{"query":"safe"}`
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s", r.Method)
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	endpoint, err := ParseEndpoint(server.URL, ModeLoopbackTest)
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	client, err := NewClient(ClientOptions{Endpoint: endpoint, Header: http.Header{
		"Authorization": []string{"Bearer secret-token"},
	}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.PostJSON(ctx, "/api/v1/query", []byte(wantBody)); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后 err=%v", err)
	}

	response, err := client.PostJSON(context.Background(), "/api/v1/query", []byte(wantBody))
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type=%q", gotContentType)
	}
}

// Secret 必须**真的**实现 slog.LogValuer。
//
// 早先 LogValue 返回 string，因此它并没有满足接口：掩码其实是靠 MarshalText 侥幸成立的。
// 换一个不走 TextMarshaler 的 handler 就会泄露，所以这里同时断言接口与两种 handler。
func TestSecretImplementsLogValuer(t *testing.T) {
	var _ slog.LogValuer = Secret{}

	const plaintext = "slog-secret-value"
	secret := NewSecret(plaintext)
	value := secret.LogValue()
	if value.Kind() != slog.KindString {
		t.Fatalf("LogValue kind=%v", value.Kind())
	}
	if strings.Contains(value.String(), plaintext) {
		t.Fatalf("LogValue 泄露明文: %s", value.String())
	}

	for name, newHandler := range map[string]func(*strings.Builder) slog.Handler{
		"text": func(b *strings.Builder) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *strings.Builder) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var builder strings.Builder
			slog.New(newHandler(&builder)).Info("cfg", "token", secret,
				"nested", struct{ Token Secret }{Token: secret})
			if strings.Contains(builder.String(), plaintext) {
				t.Fatalf("%s handler 泄露明文: %s", name, builder.String())
			}
		})
	}
}

// Endpoint 的 %#v 与 slog 也不得泄露 path：它可能含 org/stream 这类部署拓扑。
func TestEndpointGoStringAndLogValueHidePath(t *testing.T) {
	var _ slog.LogValuer = Endpoint{}

	endpoint, err := ParseEndpoint("https://metrics.internal/prometheus/org-secret", ModeProduction)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	renderings := []string{
		endpoint.String(),
		endpoint.GoString(),
		fmt.Sprintf("%v", endpoint),
		fmt.Sprintf("%+v", endpoint),
		fmt.Sprintf("%#v", endpoint),
		endpoint.LogValue().String(),
	}
	var builder strings.Builder
	slog.New(slog.NewJSONHandler(&builder, nil)).Info("cfg", "endpoint", endpoint)
	renderings = append(renderings, builder.String())

	for i, text := range renderings {
		for _, leak := range []string{"prometheus", "org-secret"} {
			if strings.Contains(text, leak) {
				t.Fatalf("渲染 %d 泄露了 %q: %s", i, leak, text)
			}
		}
		if !strings.Contains(text, "metrics.internal") {
			t.Fatalf("渲染 %d 应保留 host 以便排障: %s", i, text)
		}
	}
}
