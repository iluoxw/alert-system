// Package provider 是只读遥测 provider 的出站 HTTP 基线。
//
// 它不含任何 provider 语义（没有 PromQL、没有 SQL、不知道 Prometheus 或 OpenObserve），
// 只负责把"出站请求"这件事做安全。真正的查询构造属于 Phase 3 Task 4/5 的 adapter。
//
// 安全口径（每条都有测试）：
//
//   - endpoint 只来自服务端配置：拒绝 userinfo、query、fragment、非 http(s) scheme 与空 host；
//   - 生产默认 HTTPS；只有显式的测试模式才允许 loopback HTTP，且 caller 无法自报环境；
//   - 禁止跨 host 重定向（同 host 重定向也一并禁止，避免被上游用 3xx 引到别的路径）；
//   - 响应体有字节上界，超限显式失败而不是截断出一个"看起来完整"的结果；
//   - 凭据只在 request header 注入，且 String/日志/stats 永不泄露；
//   - 共享 Transport，但每次请求都绑定调用方的 context，取消立刻终止在途连接。
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrEndpointInvalid 表示配置的 endpoint 不满足安全形状。它是**配置错误**，
	// 必须让启动失败，而不是等第一次查询才炸。
	ErrEndpointInvalid = errors.New("provider: endpoint 配置非法")
	// ErrInsecureEndpoint 表示生产模式下配了 http。
	ErrInsecureEndpoint = errors.New("provider: 生产模式只允许 https endpoint")
	// ErrRedirectBlocked 表示上游试图重定向。
	ErrRedirectBlocked = errors.New("provider: 拒绝跟随重定向")
	// ErrResponseTooLarge 表示响应体超过字节上界。
	ErrResponseTooLarge = errors.New("provider: 响应体超过字节上界")
)

// Secret 是只读凭据。
//
// 它的 String/GoString/MarshalText 全部返回掩码，因此把整个配置结构体打进日志、
// 用 %v/%s/%+v 格式化、或者不小心 JSON 序列化，都不会泄露明文。取明文只有一个显式入口。
type Secret struct {
	value string
}

// NewSecret 包装一个明文凭据。
func NewSecret(value string) Secret { return Secret{value: value} }

// IsZero 表示未配置凭据。
func (s Secret) IsZero() bool { return s.value == "" }

// Reveal 是取明文的唯一入口。命名刻意刺眼：调用点应该少且显眼。
func (s Secret) Reveal() string { return s.value }

// String 返回掩码。
func (s Secret) String() string { return mask(s.value) }

// GoString 让 %#v 也不泄露。
func (s Secret) GoString() string { return "provider.Secret(" + mask(s.value) + ")" }

// MarshalText 让 encoding/json 与 log/slog 得到掩码而不是明文。
func (s Secret) MarshalText() ([]byte, error) { return []byte(mask(s.value)), nil }

// LogValue 让 slog 的结构化输出也走掩码。
func (s Secret) LogValue() string { return mask(s.value) }

func mask(value string) string {
	if value == "" {
		return "<empty>"
	}
	return "<redacted>"
}

// Mode 决定 endpoint 的安全策略。
//
// 它是服务端构造时决定的，不来自请求：caller 无法通过参数把自己切成测试模式，
// 否则一个请求就能把生产 HTTPS 要求绕过去。
type Mode int

const (
	// ModeProduction 要求 https。
	ModeProduction Mode = iota
	// ModeLoopbackTest 只额外允许 loopback 上的 http，供 httptest 使用。
	// 它必须由测试代码显式构造，生产装配永远传 ModeProduction。
	ModeLoopbackTest
)

// Endpoint 是校验通过的出站基地址。零值不可用。
type Endpoint struct {
	base *url.URL
}

// ParseEndpoint 校验并解析 endpoint。
//
// 空字符串是**合法**的"未连接"，返回零值 Endpoint 且 err 为 nil：部署可以先不接遥测，
// 页面显式 Unknown。这与"配了但配错"必须分开——后者返回错误让启动失败。
func ParseEndpoint(raw string, mode Mode) (Endpoint, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Endpoint{}, nil
	}
	// 控制字符会被某些代理解释成请求分隔，先于解析拒绝。
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return Endpoint{}, fmt.Errorf("%w: 含控制字符", ErrEndpointInvalid)
		}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: 无法解析", ErrEndpointInvalid)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return Endpoint{}, fmt.Errorf("%w: scheme 必须是 http 或 https", ErrEndpointInvalid)
	}
	if parsed.Hostname() == "" {
		return Endpoint{}, fmt.Errorf("%w: 缺少 host", ErrEndpointInvalid)
	}
	// userinfo 里的凭据会进日志、进 Referer、也绕过 header 注入这条唯一通道。
	if parsed.User != nil {
		return Endpoint{}, fmt.Errorf("%w: 不允许 URL userinfo", ErrEndpointInvalid)
	}
	// base 上的 query/fragment 会被后续拼接静默丢弃或篡改语义，一律拒绝。
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return Endpoint{}, fmt.Errorf("%w: 不允许 query", ErrEndpointInvalid)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return Endpoint{}, fmt.Errorf("%w: 不允许 fragment", ErrEndpointInvalid)
	}
	if scheme == "http" {
		if mode != ModeLoopbackTest {
			return Endpoint{}, fmt.Errorf("%w: %s", ErrInsecureEndpoint, "拒绝明文 endpoint")
		}
		if !isLoopbackHost(parsed.Hostname()) {
			return Endpoint{}, fmt.Errorf("%w: 测试模式的 http 只允许 loopback", ErrEndpointInvalid)
		}
	}
	// 规范化：去掉尾随斜杠，让后续 JoinPath 的结果稳定。
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return Endpoint{base: parsed}, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsZero 表示未配置。
func (e Endpoint) IsZero() bool { return e.base == nil }

// Host 返回 host:port，供 stats 与启动日志使用。
//
// 刻意不返回完整 URL：路径可能带 org/stream 这类部署拓扑信息，不该进日志。
func (e Endpoint) Host() string {
	if e.base == nil {
		return ""
	}
	return e.base.Host
}

// String 只输出 scheme 与 host，绝不输出 path/query。
func (e Endpoint) String() string {
	if e.base == nil {
		return "<not-configured>"
	}
	return e.base.Scheme + "://" + e.base.Host
}

// resolve 拼出目标 URL。path 必须是服务端代码里的常量路径。
func (e Endpoint) resolve(path string) (*url.URL, error) {
	if e.base == nil {
		return nil, fmt.Errorf("%w: endpoint 未配置", ErrEndpointInvalid)
	}
	joined := *e.base
	joined.Path = e.base.Path + "/" + strings.TrimLeft(path, "/")
	return &joined, nil
}

// ClientOptions 是出站客户端的配置。
type ClientOptions struct {
	Endpoint Endpoint
	// Timeout 是**兜底**超时。每次请求仍然必须带自己的 context deadline：
	// Timeout 只防止一个忘了设 deadline 的调用挂死连接。
	Timeout time.Duration
	// MaxResponseBytes 是响应体字节上界，必须为正。
	MaxResponseBytes int64
	// MaxConcurrent 限制同时在途请求数，必须为正。它防止一次目录刷新把连接池打满。
	MaxConcurrent int
	// Header 是每次请求都注入的固定 header（凭据走这里，且只走这里）。
	Header http.Header
	// Transport 允许测试注入；为 nil 时使用带有界连接池的默认 Transport。
	Transport http.RoundTripper
}

const (
	defaultClientTimeout    = 5 * time.Second
	defaultMaxResponseBytes = int64(8 << 20)
	defaultMaxConcurrent    = 8
)

// Client 是共享的出站 HTTP 客户端。
type Client struct {
	http             *http.Client
	endpoint         Endpoint
	maxResponseBytes int64
	header           http.Header
	// slots 是并发闸门。用带缓冲 channel 而不是信号量库：取消时要能立刻放弃排队。
	slots chan struct{}
}

// NewClient 构造客户端。endpoint 未配置时返回 nil, nil：调用方据此判定"未连接"。
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Endpoint.IsZero() {
		return nil, nil
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultClientTimeout
	}
	maxBytes := opts.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseBytes
	}
	concurrent := opts.MaxConcurrent
	if concurrent <= 0 {
		concurrent = defaultMaxConcurrent
	}
	transport := opts.Transport
	if transport == nil {
		transport = defaultTransport()
	}
	header := http.Header{}
	for key, values := range opts.Header {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// 一律不跟随重定向。上游用 3xx 把只读查询引到别的 host（SSRF）或别的路径
			// （语义漂移）都必须显式失败，而不是悄悄跟过去。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return ErrRedirectBlocked
			},
		},
		endpoint:         opts.Endpoint,
		maxResponseBytes: maxBytes,
		header:           header,
		slots:            make(chan struct{}, concurrent),
	}, nil
}

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // 不继承环境代理：代理地址属于部署配置，不该被环境变量隐式改写。
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// Endpoint 返回配置的 endpoint（只含 scheme+host 的安全表示由 Endpoint.String 提供）。
func (c *Client) Endpoint() Endpoint { return c.endpoint }

// Response 是读取完成的响应。Body 已经在字节上界内完整读出。
type Response struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

// PostForm 以 form 编码发起 POST。
//
// 用 encoder 而不是字符串拼 URL：拼接会让参数值里的 & 与 = 改变请求语义，
// 而参数值可能来自 focus 之类的不可信输入。
func (c *Client) PostForm(ctx context.Context, path string, form url.Values) (Response, error) {
	target, err := c.endpoint.resolve(path)
	if err != nil {
		return Response{}, err
	}
	body := strings.NewReader(form.Encode())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return Response{}, fmt.Errorf("provider: 构造请求失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(ctx, request)
}

// Get 发起 GET。query 由调用方用 url.Values 构造，同样不做字符串拼接。
func (c *Client) Get(ctx context.Context, path string, query url.Values) (Response, error) {
	target, err := c.endpoint.resolve(path)
	if err != nil {
		return Response{}, err
	}
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Response{}, fmt.Errorf("provider: 构造请求失败: %w", err)
	}
	return c.do(ctx, request)
}

// do 执行请求：并发闸门 -> 注入固定 header -> 发送 -> 有界读取。
func (c *Client) do(ctx context.Context, request *http.Request) (Response, error) {
	// 排队也要可取消：父 context 断开时立刻放弃，不占用 slot 也不发请求。
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}

	for key, values := range c.header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	response, err := c.http.Do(request)
	if err != nil {
		// 错误信息里绝不带完整 URL：query 可能含 focus 值，Authorization 更不能出现。
		// url.Error 会把整条 URL 塞进 Error()，因此这里显式重写。
		if errors.Is(err, ErrRedirectBlocked) {
			return Response{}, fmt.Errorf("%w: host=%s", ErrRedirectBlocked, c.endpoint.Host())
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, fmt.Errorf("provider: 请求失败 host=%s", c.endpoint.Host())
	}
	defer func() {
		// 必须读干净或显式关闭，否则连接无法复用。
		_ = response.Body.Close()
	}()

	// 多读一个字节判断"是否严格超限"：恰好等于上界必须被接受。
	limited := io.LimitReader(response.Body, c.maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{}, ctxErr
		}
		return Response{}, fmt.Errorf("provider: 读取响应失败 host=%s", c.endpoint.Host())
	}
	if int64(len(body)) > c.maxResponseBytes {
		return Response{}, fmt.Errorf("%w: host=%s limit=%d",
			ErrResponseTooLarge, c.endpoint.Host(), c.maxResponseBytes)
	}
	return Response{StatusCode: response.StatusCode, Body: body, Header: response.Header}, nil
}
