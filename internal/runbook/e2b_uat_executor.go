package runbook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"alert-system/internal/domain"
)

var ErrUATCleanupFailed = errors.New("runbook: E2B UAT Sandbox 自动清理失败")
var ErrE2BUATTransport = errors.New("runbook: E2B UAT API 传输失败")
var ErrE2BUATOperation = errors.New("runbook: E2B UAT API 操作失败")

type E2BUATExecutorConfig struct {
	BaseURL           string
	APIKey            string
	AllowedHost       string
	AllowedIPs        []string
	AllowHTTP         bool // 仅供 httptest；服务启动配置从不打开。
	HTTPClient        *http.Client
	CreateTimeout     int
	ResumeTimeout     int
	HTTPTimeout       time.Duration
	ResumeHTTPTimeout time.Duration
	ReadyTimeout      time.Duration
	PollInterval      time.Duration
	PauseSettle       time.Duration
	CleanupTimeout    time.Duration
}

type E2BUATExecutor struct {
	baseURL           *url.URL
	apiKey            string
	httpClient        *http.Client
	createTimeout     int
	resumeTimeout     int
	httpTimeout       time.Duration
	resumeHTTPTimeout time.Duration
	readyTimeout      time.Duration
	pollInterval      time.Duration
	pauseSettle       time.Duration
	cleanupTimeout    time.Duration
}

func NewE2BUATExecutor(config E2BUATExecutorConfig) (*E2BUATExecutor, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("runbook: 非法 E2B UAT API URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("runbook: E2B UAT API URL 只能包含 scheme 和 host")
	}
	allowedHost := strings.TrimSpace(config.AllowedHost)
	if allowedHost == "" || !strings.EqualFold(parsed.Hostname(), allowedHost) {
		return nil, fmt.Errorf("runbook: E2B UAT API host 不在显式白名单")
	}
	if strings.EqualFold(parsed.Hostname(), "api.agi.exc.show") ||
		strings.EqualFold(parsed.Hostname(), "api.uat.agi.exc.show") {
		return nil, fmt.Errorf("runbook: 已确认指向生产入口的 E2B API 禁止作为 UAT 执行目标")
	}
	if parsed.Scheme != "https" && !(config.AllowHTTP && parsed.Scheme == "http") {
		return nil, fmt.Errorf("runbook: E2B UAT API 必须使用 HTTPS")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("runbook: 缺少专用 E2B UAT API key")
	}
	allowedIPs := make(map[string]struct{}, len(config.AllowedIPs))
	for _, raw := range config.AllowedIPs {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			return nil, fmt.Errorf("runbook: E2B UAT API IP 白名单包含非法地址")
		}
		allowedIPs[ip.String()] = struct{}{}
	}
	if len(allowedIPs) == 0 {
		return nil, fmt.Errorf("runbook: E2B UAT API 必须配置精确 IP 白名单")
	}

	client := http.DefaultClient
	if config.HTTPClient != nil {
		client = config.HTTPClient
	}
	clientCopy := *client
	// 不允许 3xx 把 X-API-Key 带到重定向目标；UAT endpoint 必须直接响应。
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if configured, ok := client.Transport.(*http.Transport); ok && configured != nil {
		transport = configured.Clone()
	}
	// 不走进程继承的代理；每次连接都重新解析并只拨号到精确的 UAT IP 白名单，防止
	// DNS 漂移或重绑定把专用 API key 和变更请求带到生产入口。
	transport.Proxy = nil
	transport.DialContext = restrictedDialContext(allowedIPs)
	clientCopy.Transport = transport

	return &E2BUATExecutor{
		baseURL: parsed, apiKey: config.APIKey, httpClient: &clientCopy,
		createTimeout:     defaultInt(config.CreateTimeout, 120),
		resumeTimeout:     defaultInt(config.ResumeTimeout, 600),
		httpTimeout:       defaultDuration(config.HTTPTimeout, 90*time.Second),
		resumeHTTPTimeout: defaultDuration(config.ResumeHTTPTimeout, 630*time.Second),
		readyTimeout:      defaultDuration(config.ReadyTimeout, 40*time.Second),
		pollInterval:      defaultDuration(config.PollInterval, 2*time.Second),
		pauseSettle:       defaultDuration(config.PauseSettle, 5*time.Second),
		cleanupTimeout:    defaultDuration(config.CleanupTimeout, 90*time.Second),
	}, nil
}

func (e *E2BUATExecutor) Availability(actionKey string) (bool, string) {
	if actionKey == ActionLifecycleVerify {
		return true, ""
	}
	return false, "E2B UAT 执行器只支持生命周期探针"
}

func (e *E2BUATExecutor) Execute(ctx context.Context, request ExecutionRequest) (result map[string]any, err error) {
	if request.ActionKey != ActionLifecycleVerify || request.Mode != domain.RunbookModeApply {
		return nil, ErrActionNotAllowed
	}
	if validationErr := validateActionParameters(request.ActionKey, request.Parameters); validationErr != nil {
		return nil, validationErr
	}
	templateID, _ := request.Parameters["template_id"].(string)
	templateAlias, _ := request.Parameters["template_alias"].(string)
	if strings.TrimSpace(templateAlias) == "" {
		templateAlias = templateID
	}
	result = map[string]any{
		"executor": "e2b-uat-api", "external_mutation": true, "environment": "uat",
		"template_id": templateID, "template_alias": templateAlias,
		"status": "fail", "cleanup_attempted": false, "cleanup_succeeded": false,
	}

	createdAt := time.Now()
	var createResponse struct {
		SandboxID string `json:"sandboxID"`
	}
	status, requestErr := e.do(ctx, http.MethodPost, "/sandboxes", map[string]any{
		"templateID": templateID, "timeout": e.createTimeout,
	}, &createResponse, e.httpTimeout)
	result["create_duration_ms"] = time.Since(createdAt).Milliseconds()
	if requestErr != nil {
		return result, requestErr
	}
	if status != http.StatusCreated || strings.TrimSpace(createResponse.SandboxID) == "" {
		return result, unexpectedE2BStatus("create", status)
	}
	sandboxID := createResponse.SandboxID
	result["sandbox_ref_hash"] = hashAuditReference(sandboxID)

	// create 成功后，无论后续步骤失败、超时或请求 context 被取消，都用独立短超时
	// context 尝试删除测试 Sandbox，并让清理失败覆盖原本的成功结果。
	defer func() {
		result["cleanup_attempted"] = true
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cleanupTimeout)
		defer cancel()
		cleanupStatus, cleanupErr := e.do(cleanupCtx, http.MethodDelete, sandboxPath(sandboxID), nil, nil, e.cleanupTimeout)
		cleanupOK := cleanupErr == nil && (cleanupStatus == http.StatusOK || cleanupStatus == http.StatusNoContent || cleanupStatus == http.StatusNotFound)
		result["cleanup_succeeded"] = cleanupOK
		result["cleanup_http_status"] = cleanupStatus
		if !cleanupOK {
			result["status"] = "fail"
			cleanupFailure := ErrUATCleanupFailed
			if cleanupErr != nil {
				cleanupFailure = fmt.Errorf("%w: %v", ErrUATCleanupFailed, cleanupErr)
			} else {
				cleanupFailure = fmt.Errorf("%w: HTTP %d", ErrUATCleanupFailed, cleanupStatus)
			}
			if err == nil {
				err = cleanupFailure
			} else {
				err = errors.Join(err, cleanupFailure)
			}
		}
	}()

	if err = e.waitUntilReady(ctx, sandboxID); err != nil {
		return result, err
	}
	pauseAt := time.Now()
	status, err = e.do(ctx, http.MethodPost, sandboxPath(sandboxID)+"/pause", nil, nil, e.httpTimeout)
	result["pause_duration_ms"] = time.Since(pauseAt).Milliseconds()
	if err != nil {
		return result, err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return result, unexpectedE2BStatus("pause", status)
	}
	if err = waitContext(ctx, e.pauseSettle); err != nil {
		return result, err
	}

	resumeAt := time.Now()
	status, err = e.do(ctx, http.MethodPost, sandboxPath(sandboxID)+"/resume", map[string]any{
		"timeout": e.resumeTimeout,
	}, nil, e.resumeHTTPTimeout)
	result["resume_duration_ms"] = time.Since(resumeAt).Milliseconds()
	if err != nil {
		return result, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return result, unexpectedE2BStatus("resume", status)
	}
	status, err = e.do(ctx, http.MethodGet, sandboxPath(sandboxID), nil, nil, e.httpTimeout)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK {
		return result, unexpectedE2BStatus("resume verify", status)
	}
	result["status"] = "pass"
	result["reason"] = "snapshot-chain-healthy"
	return result, nil
}

func (e *E2BUATExecutor) waitUntilReady(ctx context.Context, sandboxID string) error {
	readyCtx, cancel := context.WithTimeout(ctx, e.readyTimeout)
	defer cancel()
	for {
		status, err := e.do(readyCtx, http.MethodGet, sandboxPath(sandboxID), nil, nil, e.httpTimeout)
		if err != nil {
			return err
		}
		if status == http.StatusOK {
			return nil
		}
		if status >= 400 && status < 500 && status != http.StatusNotFound && status != http.StatusConflict && status != http.StatusTooManyRequests {
			return unexpectedE2BStatus("ready", status)
		}
		if err := waitContext(readyCtx, e.pollInterval); err != nil {
			return fmt.Errorf("%w: Sandbox ready 超时: %w", ErrE2BUATOperation, err)
		}
	}
}

func (e *E2BUATExecutor) do(ctx context.Context, method, path string, payload any, output any, timeout time.Duration) (int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("runbook: 编码 E2B UAT 请求失败: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := *e.baseURL
	endpoint.Path = path
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint.String(), body)
	if err != nil {
		return 0, fmt.Errorf("runbook: 构造 E2B UAT 请求失败: %w", err)
	}
	request.Header.Set("X-API-Key", e.apiKey)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := e.httpClient.Do(request)
	if err != nil {
		// net/http 的 url.Error 会带完整请求 path，其中包含 Sandbox ID。审计只允许
		// 保存不可逆 hash，所以这里只保留可用于 errors.Is 的取消/超时类别。
		switch {
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("%w: %w", ErrE2BUATTransport, context.Canceled)
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("%w: %w", ErrE2BUATTransport, context.DeadlineExceeded)
		default:
			return 0, ErrE2BUATTransport
		}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 1<<20)
	if output != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		if err := json.NewDecoder(limited).Decode(output); err != nil {
			return response.StatusCode, fmt.Errorf("%w: 响应格式非法", ErrE2BUATOperation)
		}
	} else {
		_, _ = io.Copy(io.Discard, limited)
	}
	return response.StatusCode, nil
}

func sandboxPath(sandboxID string) string {
	return "/sandboxes/" + url.PathEscape(sandboxID)
}

func hashAuditReference(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func unexpectedE2BStatus(operation string, status int) error {
	return fmt.Errorf("%w: %s 返回 HTTP %d", ErrE2BUATOperation, operation, status)
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func defaultInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func restrictedDialContext(allowedIPs map[string]struct{}) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("runbook: E2B UAT 拨号地址非法: %w", err)
		}
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("runbook: 解析 E2B UAT API host 失败: %w", err)
		}
		for _, candidate := range resolved {
			if _, ok := allowedIPs[candidate.IP.String()]; ok {
				return dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			}
		}
		return nil, fmt.Errorf("runbook: E2B UAT API 当前解析结果不在精确 IP 白名单")
	}
}
