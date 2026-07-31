package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// DefaultEmailTimeout 是 EmailChannel 与 SMTP 服务器交互的默认超时（连接 + 整个协议
// 往返），量级沿用 WebhookChannel 的 DefaultWebhookTimeout。
const DefaultEmailTimeout = 5 * time.Second

// ErrSMTPNotConfigured 在 SMTPConfig.Host 未配置时返回：渠道照常注册，但发送时
// 明确报错，走既有 markFailed 链路（与其余渠道不可用时的处理口径一致）。
var ErrSMTPNotConfigured = errors.New("notify: SMTP 未配置")

// smtpSender 是 EmailChannel 依赖的最小发送能力：把一封已渲染好的报文通过 SMTP 协议
// 发到 to。生产实现 netSMTPSender 包装标准库 net/smtp；单测用本地假 SMTP 服务器走
// 完整协议对话，不出网、不依赖真实凭据（与 larkMessenger/sdkMessenger 的解耦模式一致）。
type smtpSender interface {
	send(ctx context.Context, from, to string, data []byte) error
}

// EmailChannel 是 kind="email" 的 Channel 实现（R3 首个真实到人渠道）：net/smtp +
// PLAIN auth + 可选 STARTTLS，纯文本正文。
type EmailChannel struct {
	from       string
	configured bool
	sender     smtpSender
}

// NewEmailChannel 用 SMTP 凭据构造生产环境的 EmailChannel。沿 NewLarkChannel(appID,
// appSecret string) 的模式接收展开后的字段而非 config.SMTPConfig 整体——notify 包
// 不依赖 internal/config，具体值由 internal/app 装配时从 config.SMTPConfig 拆包传入。
// host 为空表示未配置，渠道仍正常注册，Send 时返回 ErrSMTPNotConfigured。
func NewEmailChannel(host string, port int, username, password, from string, startTLS bool) *EmailChannel {
	return &EmailChannel{
		from:       from,
		configured: host != "",
		sender: &netSMTPSender{
			addr:     net.JoinHostPort(host, strconv.Itoa(port)),
			host:     host,
			username: username,
			password: password,
			startTLS: startTLS,
			timeout:  DefaultEmailTimeout,
		},
	}
}

func (e *EmailChannel) Kind() string { return "email" }

// Send 把 msg 渲染成纯文本邮件发到 target（收件人地址）。
func (e *EmailChannel) Send(ctx context.Context, target string, msg Message) error {
	if !e.configured {
		return ErrSMTPNotConfigured
	}
	return e.sender.send(ctx, e.from, target, buildMessage(e.from, target, msg))
}

// buildMessage 渲染一封符合 RFC 5322 基本要求的纯文本邮件（From/To/Subject/Date/
// Message-Id/MIME 头 + 正文），供 smtpSender 发送。中文主题按 RFC 2047（QEncoding）
// 编码，避免非 ASCII 头字段导致部分 MUA 乱码或拒收；纯 ASCII 主题时 Encode 原样返回，
// 不产生多余编码。
func buildMessage(from, to string, msg Message) []byte {
	subject := mime.QEncoding.Encode("UTF-8", fmt.Sprintf("[%s] %s", msg.Severity, msg.Title))

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: <%d@alert-system>\r\n", time.Now().UnixNano())
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(buildEmailBody(msg))
	return []byte(b.String())
}

// buildEmailBody 是纯文本正文：复用 Message 既有字段，与飞书降级文本（buildDegradedText）
// 展示的信息对齐（聚合组摘要、详情链接），不做额外脱敏——Message 由管道产出时已不含
// 凭据等敏感信息，目标地址本身的脱敏是 sender.go 时间线记录的职责（maskTarget），
// 与邮件正文内容无关。
func buildEmailBody(msg Message) string {
	var b strings.Builder
	b.WriteString(msg.Body)
	if msg.GroupKey != "" {
		fmt.Fprintf(&b, "\n\n聚合组：%s（共 %d 条告警）", msg.GroupKey, len(msg.AlertIDs))
	}
	if msg.DetailURL != "" {
		fmt.Fprintf(&b, "\n详情：%s", msg.DetailURL)
	}
	if !msg.OccurredAt.IsZero() {
		fmt.Fprintf(&b, "\n发生时间：%s", msg.OccurredAt.Format(time.RFC3339))
	}
	return b.String()
}

// netSMTPSender 是 smtpSender 的生产实现，包装标准库 net/smtp，不引入第三方依赖。
type netSMTPSender struct {
	addr     string // host:port
	host     string // 仅用于 PlainAuth 与 STARTTLS 的 ServerName 校验
	username string
	password string
	startTLS bool
	timeout  time.Duration
}

// send 走完整 SMTP 协议对话：连接 -> EHLO -> [STARTTLS] -> [AUTH PLAIN] -> MAIL FROM ->
// RCPT TO -> DATA -> QUIT。startTLS=false 时无论服务器是否支持都不升级（供本地
// mailhog 类调试）；username 为空时跳过 AUTH（部分内网 SMTP 中继允许匿名转发）。
func (n *netSMTPSender) send(ctx context.Context, from, to string, data []byte) error {
	timeout := n.timeout
	if timeout <= 0 {
		timeout = DefaultEmailTimeout
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", n.addr)
	if err != nil {
		return fmt.Errorf("notify: 连接 SMTP 服务器失败: %w", err)
	}
	// net/smtp 的协议往返不支持 context 取消，用连接级 deadline 兜底整体超时，
	// 与 dial 阶段的 ctx 超时语义对齐，避免协议卡死导致 goroutine 泄漏。
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer conn.Close()

	client, err := smtp.NewClient(conn, n.host)
	if err != nil {
		return fmt.Errorf("notify: 建立 SMTP 会话失败: %w", err)
	}
	defer client.Close()

	if n.startTLS {
		// 服务器不广播 STARTTLS 时绝不能静默降级为明文继续——这正是 STARTTLS 剥离
		// 攻击面：中间人只需摘掉能力广播就能让客户端在明文信道上发出 AUTH PLAIN 的
		// base64 凭据。startTLS=true 是从严取舍，必须直接拒绝，而不是退回明文对话。
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return errors.New("notify: 服务器不支持 STARTTLS，拒绝明文发送；本地调试请显式设 SMTP_STARTTLS=false")
		}
		if err := client.StartTLS(&tls.Config{ServerName: n.host}); err != nil {
			return fmt.Errorf("notify: SMTP STARTTLS 失败: %w", err)
		}
	}

	if n.username != "" {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(smtp.PlainAuth("", n.username, n.password, n.host)); err != nil {
				return fmt.Errorf("notify: SMTP 认证失败: %w", err)
			}
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("notify: SMTP MAIL FROM 失败: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("notify: SMTP RCPT TO 失败: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("notify: SMTP DATA 失败: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return fmt.Errorf("notify: 写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: 关闭 SMTP DATA 失败: %w", err)
	}
	return client.Quit()
}
