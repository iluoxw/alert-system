package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"time"
)

// apiClient 是一个带 cookie jar 的最小 HTTP 客户端：驱动被测系统的真实 REST API
// （登录态用 cookie 维持，不绕过 sessionAuth()/requireAdmin() 中间件），供
// seedConfig（bootstrap 阶段）与各场景测试共用。不依赖 *testing.T——TestMain 里
// seedConfig 阶段没有 *testing.T 可用，出错一律返回 error 由调用方决定如何失败。
type apiClient struct {
	baseURL string
	http    *http.Client
}

func newAPIClient(baseURL string) (*apiClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("e2e: 创建 cookie jar 失败: %w", err)
	}
	return &apiClient{baseURL: baseURL, http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}, nil
}

// login 调用 POST /api/v1/auth/login，成功后 cookie jar 里带上会话 cookie，
// 之后经同一个 apiClient 发出的请求自动复用。
func (c *apiClient) login(username, password string) error {
	status, body, err := c.doJSON(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("e2e: 登录失败 status=%d body=%s", status, body)
	}
	return nil
}

// doJSON 发一个 JSON 请求：body 为 nil 表示无请求体；out 非 nil 时把响应体解码进去
// （out 通常传 *map[string]any，便于按需取字段，不为每个 admin 资源单独定义 DTO）。
// 返回响应状态码与原始响应体。
func (c *apiClient) doJSON(method, path string, body, out any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("e2e: 序列化请求体失败: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: 构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: 请求 %s %s 失败: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: 读取响应体失败: %w", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("e2e: 解析响应体失败: %w（body=%s）", err, raw)
		}
	}
	return resp.StatusCode, raw, nil
}

// postManagedHook 只携带 SRE Control 分发的 managed webhook path；hook UUID 本身完成
// 来源识别与鉴权，符合 Alertmanager receiver 的实际配置方式。
func (c *apiClient) postManagedHook(webhookPath string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+webhookPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: 构造 hook 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: hook 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("e2e: 读取 hook 响应体失败: %w", err)
	}
	return resp.StatusCode, raw, nil
}
