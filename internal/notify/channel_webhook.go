package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultWebhookTimeout 是 WebhookChannel 未显式设置 Timeout 时使用的默认超时。
const DefaultWebhookTimeout = 5 * time.Second

// WebhookChannel 把 Message 序列化成 JSON POST 到 target（一个 URL），kind="webhook"。
// 只用标准库 net/http，不引入新依赖；Client 可在测试里替换，方便配合 httptest.Server。
type WebhookChannel struct {
	Timeout time.Duration
	Client  *http.Client
}

// NewWebhookChannel 构造一个 WebhookChannel；timeout <= 0 时回退到 DefaultWebhookTimeout。
func NewWebhookChannel(timeout time.Duration) *WebhookChannel {
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	return &WebhookChannel{
		Timeout: timeout,
		Client:  &http.Client{Timeout: timeout},
	}
}

func (w *WebhookChannel) Kind() string { return "webhook" }

// Send 把 msg 序列化为 JSON body POST 到 target；非 2xx 状态码视为失败（返回 error，
// 交给 Sender 决定是否重试）。
func (w *WebhookChannel) Send(ctx context.Context, target string, msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: 序列化 webhook 消息失败: %w", err)
	}

	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: 构造 webhook 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: 发送 webhook 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: webhook 返回非 2xx 状态码 %d", resp.StatusCode)
	}
	return nil
}
