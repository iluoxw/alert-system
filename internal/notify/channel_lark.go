package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcard "github.com/larksuite/oapi-sdk-go/v3/card"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// larkMessageResult 携带一次飞书消息发送的业务级结果（Code/Msg 来自飞书 API 响应体，
// 不含任何凭据），供 isPermissionDenied 判断是否需要降级为纯文本+链接（R6.4）。
type larkMessageResult struct {
	Code int
	Msg  string
}

// larkMessenger 是 LarkChannel 依赖的最小发送能力。生产实现 sdkMessenger 包装官方
// oapi-sdk-go 客户端；单测用 fake 替换，不依赖真实飞书凭据/网络。
type larkMessenger interface {
	// SendMessage 发送一条消息；msgType 为 "interactive"（卡片）或 "text"（降级文本），
	// content 是该 msg_type 对应的已序列化 JSON body。
	SendMessage(ctx context.Context, receiveIDType, receiveID, msgType, content string) (larkMessageResult, error)
}

// sdkMessenger 是 larkMessenger 的生产实现。
type sdkMessenger struct {
	client *lark.Client
}

func newSDKMessenger(appID, appSecret string) *sdkMessenger {
	return &sdkMessenger{client: lark.NewClient(appID, appSecret)}
}

func (m *sdkMessenger) SendMessage(ctx context.Context, receiveIDType, receiveID, msgType, content string) (larkMessageResult, error) {
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(receiveID).
		MsgType(msgType).
		Content(content).
		Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(body).
		Build()

	resp, err := m.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		// 不回显底层 transport 错误原文：这类 error 由 SDK 内部 HTTP 客户端产生，
		// 硬约束要求凭据绝不出现在日志/错误字符串里，宁可损失一些调试细节也不冒这个险
		// （该 error 会被 Sender 原样写进 notification_log.last_error 与告警时间线，用户可见）。
		return larkMessageResult{}, errors.New("notify: 调用飞书消息接口失败（网络或客户端错误）")
	}
	result := larkMessageResult{Code: resp.Code, Msg: resp.Msg}
	if !resp.Success() {
		// resp.Msg 是飞书服务端返回的错误描述，不含调用方凭据，可以安全地放进 error。
		return result, fmt.Errorf("notify: 飞书返回业务错误 code=%d msg=%s", resp.Code, resp.Msg)
	}
	return result, nil
}

// knownPermissionDeniedCodes 是飞书 API 里代表「应用/机器人无权限执行该操作」的已知业务
// 错误码（用于 R6.4 降级判断）。不同租户的权限配置差异较大，这里只是已知范围，遇到新码
// 时下面的 isPermissionDenied 还会按 msg 关键字兜底；如实际运行中发现遗漏，应扩充此表。
var knownPermissionDeniedCodes = map[int]bool{
	9499:     true, // Forbidden
	19002:    true, // 无权限访问该资源
	99991672: true, // 应用权限不足
}

// isPermissionDenied 判断一次发送失败是否属于「无权限发送交互卡片」，需要降级为纯文本+
// 跳转链接（R6.4），而非当作普通失败交给 Sender 重试（重试同一个会一直失败的权限错误没有
// 意义）。
func isPermissionDenied(code int, msg string) bool {
	if knownPermissionDeniedCodes[code] {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "no permission") || strings.Contains(lower, "forbidden") || strings.Contains(msg, "权限")
}

// severityTemplate 把 Severity 映射为飞书卡片 header 的配色模板。
func severityTemplate(severity string) string {
	switch strings.ToLower(severity) {
	case "critical":
		return larkcard.TemplateRed
	case "warning":
		return larkcard.TemplateOrange
	case "info":
		return larkcard.TemplateBlue
	default:
		return larkcard.TemplateGrey
	}
}

// buildAlertCard 把 Message 渲染成飞书交互卡片（R6.2/R9.5）：header 按 severity 配色，
// 正文展示 Body，分组通知（GroupKey 非空）附加聚合组摘要；Ack/Resolve 按钮只在单告警
// （非分组）发送时提供——分组场景一次点击该确认哪条告警语义不清，交给用户点详情链接
// 进入分组详情页逐条处理（这一决策留待任务 22/27 的分组通知编排复核）。
func buildAlertCard(msg Message) (string, error) {
	header := larkcard.NewMessageCardHeader().
		Template(severityTemplate(msg.Severity)).
		Title(larkcard.NewMessageCardPlainText().Content(msg.Title).Build()).
		Build()

	bodyText := msg.Body
	if msg.GroupKey != "" {
		bodyText = fmt.Sprintf("%s\n\n**聚合组**：%s（共 %d 条告警）", bodyText, msg.GroupKey, len(msg.AlertIDs))
	}
	elements := []larkcard.MessageCardElement{
		larkcard.NewMessageCardDiv().Text(larkcard.NewMessageCardLarkMd().Content(bodyText).Build()).Build(),
	}

	var actions []larkcard.MessageCardActionElement
	if msg.GroupKey == "" && len(msg.AlertIDs) == 1 {
		alertID := msg.AlertIDs[0]
		actions = append(actions,
			larkcard.NewMessageCardEmbedButton().
				Text(larkcard.NewMessageCardPlainText().Content("认领").Build()).
				Type(larkcard.MessageCardButtonTypePrimary).
				Value(map[string]interface{}{"action": "ack", "alert_id": alertID}).
				Build(),
			larkcard.NewMessageCardEmbedButton().
				Text(larkcard.NewMessageCardPlainText().Content("解决").Build()).
				Type(larkcard.MessageCardButtonTypeDefault).
				Value(map[string]interface{}{"action": "resolve", "alert_id": alertID}).
				Build(),
		)
	}
	if msg.DetailURL != "" {
		actions = append(actions,
			larkcard.NewMessageCardEmbedButton().
				Text(larkcard.NewMessageCardPlainText().Content("查看详情").Build()).
				Url(msg.DetailURL).
				Build(),
		)
	}
	if len(actions) > 0 {
		elements = append(elements, larkcard.NewMessageCardAction().Actions(actions).Build())
	}

	card := larkcard.NewMessageCard().
		Config(larkcard.NewMessageCardConfig().WideScreenMode(true).Build()).
		Header(header).
		Elements(elements).
		Build()

	content, err := card.String()
	if err != nil {
		return "", fmt.Errorf("notify: 序列化飞书卡片失败: %w", err)
	}
	return content, nil
}

// buildDegradedText 是卡片因权限不足降级后的纯文本消息（R6.4）：标题+正文+聚合组摘要+
// 详情链接，msg_type=text。
func buildDegradedText(msg Message) (string, error) {
	var b strings.Builder
	b.WriteString(msg.Title)
	if msg.Body != "" {
		b.WriteString("\n")
		b.WriteString(msg.Body)
	}
	if msg.GroupKey != "" {
		fmt.Fprintf(&b, "\n聚合组：%s（共 %d 条告警）", msg.GroupKey, len(msg.AlertIDs))
	}
	if msg.DetailURL != "" {
		fmt.Fprintf(&b, "\n详情：%s", msg.DetailURL)
	}

	payload := struct {
		Text string `json:"text"`
	}{Text: b.String()}
	bs, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("notify: 序列化飞书降级文本消息失败: %w", err)
	}
	return string(bs), nil
}

// parseTarget 解析 target 里的接收者类型：支持 "<type>:<id>" 前缀形式（如
// "open_id:ou_xxx"）指定 chat_id/open_id/user_id/union_id/email 中的一种；不带前缀时
// 默认按 chat_id 处理（群聊场景是最常见的告警通知目标）。
func parseTarget(target string) (receiveIDType, receiveID string) {
	if idx := strings.IndexByte(target, ':'); idx > 0 {
		switch target[:idx] {
		case "chat_id", "open_id", "user_id", "union_id", "email":
			return target[:idx], target[idx+1:]
		}
	}
	return "chat_id", target
}

// LarkChannel 是 kind="lark" 的 Channel 实现（R6.2 主力渠道）：发送交互卡片，失败时按
// R6.4 判断是否为权限问题并降级为纯文本+跳转链接。
type LarkChannel struct {
	messenger larkMessenger
}

// NewLarkChannel 用 app_id/app_secret 构造生产环境的 LarkChannel（真实调用飞书 API）。
func NewLarkChannel(appID, appSecret string) *LarkChannel {
	return &LarkChannel{messenger: newSDKMessenger(appID, appSecret)}
}

func (c *LarkChannel) Kind() string { return "lark" }

// Send 先尝试发送交互卡片；若失败且判定为权限不足（isPermissionDenied），降级为纯文本+
// 跳转链接重发一次；其余错误原样返回，交给 Sender 的退避重试处理。
func (c *LarkChannel) Send(ctx context.Context, target string, msg Message) error {
	receiveIDType, receiveID := parseTarget(target)

	cardContent, err := buildAlertCard(msg)
	if err != nil {
		return err
	}
	result, sendErr := c.messenger.SendMessage(ctx, receiveIDType, receiveID, "interactive", cardContent)
	if sendErr == nil {
		return nil
	}
	if !isPermissionDenied(result.Code, result.Msg) {
		return sendErr
	}

	textContent, buildErr := buildDegradedText(msg)
	if buildErr != nil {
		return buildErr
	}
	if _, err := c.messenger.SendMessage(ctx, receiveIDType, receiveID, "text", textContent); err != nil {
		return fmt.Errorf("notify: 卡片发送因权限不足降级为文本后仍失败: %w", err)
	}
	return nil
}
