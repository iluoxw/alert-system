package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	larkWebhookKind          = "lark_webhook"
	larkWebhookMaxTextBytes  = 18 * 1024
	larkWebhookMaxReplyBytes = 64 * 1024
)

// LarkWebhookChannel 通过群聊自定义机器人的 webhook 发送文本通知。它与 kind="lark"
// 的应用机器人渠道是两种协议：前者直接 POST 到 bot/v2/hook，后者使用 App ID/Secret
// 调用消息 OpenAPI，二者的 target 和请求体不能混用。
type LarkWebhookChannel struct {
	Timeout time.Duration
	Client  *http.Client
}

func NewLarkWebhookChannel(timeout time.Duration) *LarkWebhookChannel {
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	return &LarkWebhookChannel{
		Timeout: timeout,
		Client:  &http.Client{Timeout: timeout},
	}
}

func (c *LarkWebhookChannel) Kind() string { return larkWebhookKind }

type larkWebhookRequest struct {
	MsgType string `json:"msg_type"`
	Content struct {
		Text string `json:"text"`
	} `json:"content"`
}

type larkWebhookResponse struct {
	Code          int    `json:"code"`
	Msg           string `json:"msg"`
	StatusCode    int    `json:"StatusCode"`
	StatusMessage string `json:"StatusMessage"`
}

func formatLarkWebhookMessage(msg Message) string {
	var b strings.Builder
	b.WriteString("[SRE Control]")
	if msg.Severity != "" {
		fmt.Fprintf(&b, " [%s]", strings.ToUpper(msg.Severity))
	}
	if msg.Title != "" {
		fmt.Fprintf(&b, " %s", msg.Title)
	}
	if msg.Body != "" && msg.Body != msg.Title {
		fmt.Fprintf(&b, "\n%s", msg.Body)
	}
	if len(msg.Labels) > 0 {
		keys := make([]string, 0, len(msg.Labels))
		for key := range msg.Labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+msg.Labels[key])
		}
		fmt.Fprintf(&b, "\n标签：%s", strings.Join(parts, " · "))
	}
	if !msg.OccurredAt.IsZero() {
		fmt.Fprintf(&b, "\n发生时间：%s", msg.OccurredAt.Format(time.RFC3339))
	}
	if msg.DetailURL != "" {
		fmt.Fprintf(&b, "\n详情：%s", msg.DetailURL)
	}
	return truncateUTF8Bytes(b.String(), larkWebhookMaxTextBytes)
}

func truncateUTF8Bytes(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	const suffix = "\n…（内容过长，已截断）"
	limit := maxBytes - len(suffix)
	if limit <= 0 {
		return suffix
	}
	cut := limit
	for cut > 0 && (value[cut]&0xC0) == 0x80 {
		cut--
	}
	return value[:cut] + suffix
}

// Send 同时校验 HTTP 状态和飞书响应体业务码。飞书参数错误也可能返回 HTTP 200，
// 只检查 HTTP 状态会把未送达误记为成功。
func (c *LarkWebhookChannel) Send(ctx context.Context, target string, msg Message) error {
	payload := larkWebhookRequest{MsgType: "text"}
	payload.Content.Text = formatLarkWebhookMessage(msg)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: 序列化飞书群机器人消息失败: %w", err)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: 构造飞书群机器人请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: 发送飞书群机器人请求失败")
	}
	defer resp.Body.Close()

	replyBody, readErr := io.ReadAll(io.LimitReader(resp.Body, larkWebhookMaxReplyBytes))
	if readErr != nil {
		return fmt.Errorf("notify: 读取飞书群机器人响应失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: 飞书群机器人返回非 2xx 状态码 %d", resp.StatusCode)
	}

	var result larkWebhookResponse
	if err := json.Unmarshal(replyBody, &result); err != nil {
		return fmt.Errorf("notify: 无法解析飞书群机器人响应")
	}
	if result.Code != 0 || result.StatusCode != 0 {
		message := result.Msg
		if message == "" {
			message = result.StatusMessage
		}
		return fmt.Errorf("notify: 飞书群机器人返回业务错误 code=%d status_code=%d msg=%s", result.Code, result.StatusCode, message)
	}
	return nil
}
