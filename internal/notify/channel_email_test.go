package notify

import (
	"context"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- buildMessage/buildEmailBody 纯函数测试（无 IO） ----------

func TestBuildMessage_HeadersAndPlainSubject(t *testing.T) {
	msg := Message{
		Severity: "critical",
		Title:    "CPU high",
		Body:     "host-1 CPU 使用率持续超过 95%",
	}
	raw := string(buildMessage("alert-bot@example.com", "oncall@example.com", msg))

	for _, want := range []string{
		"From: alert-bot@example.com\r\n",
		"To: oncall@example.com\r\n",
		"Subject: [critical] CPU high\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("buildMessage() 缺少期望的头部 %q，实际:\n%s", want, raw)
		}
	}
	if !strings.Contains(raw, "\r\n\r\nhost-1 CPU 使用率持续超过 95%") {
		t.Errorf("buildMessage() 正文不含 Body，实际:\n%s", raw)
	}
}

// 中文主题必须走 RFC 2047 编码，否则非 ASCII 头部会导致部分 MUA 乱码或拒收。
func TestBuildMessage_ChineseSubjectRFC2047Encoded(t *testing.T) {
	msg := Message{Severity: "critical", Title: "CPU 使用率过高"}
	raw := string(buildMessage("a@example.com", "b@example.com", msg))

	if strings.Contains(raw, "使用率") {
		t.Errorf("buildMessage() Subject 头未编码，直接出现了非 ASCII 原文:\n%s", raw)
	}
	if !strings.Contains(raw, "Subject: =?UTF-8?") {
		t.Errorf("buildMessage() 期望 Subject 头使用 RFC 2047 编码，实际:\n%s", raw)
	}
}

func TestBuildEmailBody_GroupAndDetailAndOccurredAt(t *testing.T) {
	occurred := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	msg := Message{
		AlertIDs:   []string{"a1", "a2"},
		GroupKey:   "team-a-critical",
		Body:       "汇总摘要",
		DetailURL:  "https://example.com/groups/team-a-critical",
		OccurredAt: occurred,
	}
	body := buildEmailBody(msg)

	for _, want := range []string{
		"汇总摘要",
		"聚合组：team-a-critical（共 2 条告警）",
		"详情：https://example.com/groups/team-a-critical",
		"发生时间：" + occurred.Format(time.RFC3339),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("buildEmailBody() 缺少 %q，实际:\n%s", want, body)
		}
	}
}

// ---------- EmailChannel.Send 编排测试（fake sender，不碰网络） ----------

// fakeSMTPSenderCall 记录一次 send 调用的入参，供断言 EmailChannel 是否正确委托。
type fakeSMTPSenderCall struct {
	from, to string
	data     []byte
}

type fakeSMTPSender struct {
	calls []fakeSMTPSenderCall
	err   error
}

func (f *fakeSMTPSender) send(ctx context.Context, from, to string, data []byte) error {
	f.calls = append(f.calls, fakeSMTPSenderCall{from, to, data})
	return f.err
}

func TestEmailChannel_Kind(t *testing.T) {
	ch := &EmailChannel{configured: true, sender: &fakeSMTPSender{}}
	if ch.Kind() != "email" {
		t.Errorf("Kind() = %q, want %q", ch.Kind(), "email")
	}
}

func TestEmailChannel_Send_NotConfigured(t *testing.T) {
	fake := &fakeSMTPSender{}
	ch := &EmailChannel{configured: false, sender: fake}

	err := ch.Send(context.Background(), "oncall@example.com", Message{Title: "x"})
	if !errors.Is(err, ErrSMTPNotConfigured) {
		t.Fatalf("Send() error = %v, want ErrSMTPNotConfigured", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("Host 未配置时不应调用底层 sender，实际调用了 %d 次", len(fake.calls))
	}
}

func TestEmailChannel_Send_DelegatesToSender(t *testing.T) {
	fake := &fakeSMTPSender{}
	ch := &EmailChannel{from: "alert-bot@example.com", configured: true, sender: fake}

	msg := Message{Severity: "warning", Title: "磁盘空间不足", Body: "host-2 磁盘使用率 92%"}
	if err := ch.Send(context.Background(), "oncall@example.com", msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("期望调用 1 次底层 sender，实际 %d 次", len(fake.calls))
	}
	call := fake.calls[0]
	if call.from != "alert-bot@example.com" || call.to != "oncall@example.com" {
		t.Errorf("send() from/to = %q/%q, want alert-bot@example.com/oncall@example.com", call.from, call.to)
	}
	if !strings.Contains(string(call.data), "host-2 磁盘使用率 92%") {
		t.Errorf("send() data 不含正文，实际:\n%s", call.data)
	}
}

func TestEmailChannel_Send_PropagatesSenderError(t *testing.T) {
	fake := &fakeSMTPSender{err: errors.New("boom")}
	ch := &EmailChannel{configured: true, sender: fake}

	if err := ch.Send(context.Background(), "oncall@example.com", Message{Title: "x"}); err == nil {
		t.Fatal("Send() error = nil, want propagated sender error")
	}
}

// ---------- netSMTPSender 真实协议对话测试（本地 net.Listener 假 SMTP 服务器，不出网） ----------

// fakeSMTPServer 是一个最小 SMTP 服务端：只接受一条连接，按收到的命令给出符合 RFC 5321
// 的响应码，同时记录关键命令内容供断言。不支持真实 TLS 升级——advertiseSTARTTLS 仅用于
// 验证"服务器不支持时客户端安全跳过"，真实 STARTTLS 握手逻辑由 Go 标准库自身覆盖，
// 不在本包重复验证。
type fakeSMTPServer struct {
	ln                net.Listener
	advertiseAuth     bool
	advertiseSTARTTLS bool
	rejectRcpt        bool

	mu       chan struct{} // 关闭时置位，标记一次会话已处理完毕，供测试同步等待
	ehlo     string
	authLine string
	mailFrom string
	rcptTo   string
	data     string
}

func startFakeSMTPServer(t *testing.T, advertiseAuth bool) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起本地假 SMTP 服务器失败: %v", err)
	}
	srv := &fakeSMTPServer{ln: ln, advertiseAuth: advertiseAuth, mu: make(chan struct{})}
	go srv.serveOne()
	t.Cleanup(func() { ln.Close() })
	return srv
}

func (s *fakeSMTPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("解析假 SMTP 服务器地址失败: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析假 SMTP 服务器端口失败: %v", err)
	}
	return host, port
}

func (s *fakeSMTPServer) serveOne() {
	defer close(s.mu)
	conn, err := s.ln.Accept()
	if err != nil {
		return // 测试清理时关闭 listener，属正常退出
	}
	defer conn.Close()

	tp := textproto.NewConn(conn)
	tp.PrintfLine("220 fake.smtp.local ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			s.ehlo = line
			tp.PrintfLine("250-fake.smtp.local")
			if s.advertiseSTARTTLS {
				tp.PrintfLine("250-STARTTLS")
			}
			if s.advertiseAuth {
				tp.PrintfLine("250-AUTH PLAIN")
			}
			tp.PrintfLine("250 OK")
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.authLine = line
			tp.PrintfLine("235 2.7.0 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			s.mailFrom = line
			tp.PrintfLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			s.rcptTo = line
			if s.rejectRcpt {
				tp.PrintfLine("550 no such user")
				continue
			}
			tp.PrintfLine("250 OK")
		case upper == "DATA":
			tp.PrintfLine("354 End data with <CR><LF>.<CR><LF>")
			raw, _ := io.ReadAll(tp.DotReader())
			s.data = string(raw)
			tp.PrintfLine("250 OK")
		case upper == "QUIT":
			tp.PrintfLine("221 Bye")
			return
		default:
			tp.PrintfLine("500 unrecognized command")
		}
	}
}

// wait 等服务端处理完这一次连接（Send 调用已同步返回后再读取断言字段，避免数据竞争）。
func (s *fakeSMTPServer) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.mu:
	case <-time.After(2 * time.Second):
		t.Fatal("等待假 SMTP 服务器处理完成超时")
	}
}

func TestNetSMTPSender_Send_FullDialogWithAuth(t *testing.T) {
	srv := startFakeSMTPServer(t, true)
	host, port := srv.hostPort(t)

	ch := NewEmailChannel(host, port, "alert-bot", "s3cr3t", "alert-bot@example.com", false)
	msg := Message{Severity: "critical", Title: "CPU high", Body: "host-1 CPU 过高"}

	if err := ch.Send(context.Background(), "oncall@example.com", msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	srv.wait(t)

	if srv.authLine == "" {
		t.Error("服务器广播了 AUTH，期望客户端发起 AUTH PLAIN，但未收到")
	}
	if srv.mailFrom == "" || !strings.Contains(srv.mailFrom, "alert-bot@example.com") {
		t.Errorf("MAIL FROM 命令 = %q, 期望包含发件地址", srv.mailFrom)
	}
	if srv.rcptTo == "" || !strings.Contains(srv.rcptTo, "oncall@example.com") {
		t.Errorf("RCPT TO 命令 = %q, 期望包含收件地址", srv.rcptTo)
	}
	if !strings.Contains(srv.data, "host-1 CPU 过高") {
		t.Errorf("DATA 内容不含正文，实际:\n%s", srv.data)
	}
	// 密码不应以任何形式出现在协议往返之外的地方；这里额外确认它不会明文出现在
	// AUTH 命令行（PLAIN 机制经 base64 编码，不会是明文）。
	if strings.Contains(srv.authLine, "s3cr3t") {
		t.Error("AUTH 命令行不应包含明文密码")
	}
}

func TestNetSMTPSender_Send_NoAuthWhenUsernameEmpty(t *testing.T) {
	srv := startFakeSMTPServer(t, true) // 服务器支持 AUTH，但客户端未配置用户名
	host, port := srv.hostPort(t)

	ch := NewEmailChannel(host, port, "", "", "alert-bot@example.com", false)
	if err := ch.Send(context.Background(), "oncall@example.com", Message{Title: "x"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	srv.wait(t)

	if srv.authLine != "" {
		t.Errorf("username 为空时不应发起 AUTH，实际收到命令 %q", srv.authLine)
	}
}

// STARTTLS=false 时即使服务器广播支持，也必须跳过——供本地 mailhog 类调试。
func TestNetSMTPSender_Send_STARTTLSSkippedWhenDisabled(t *testing.T) {
	srv := startFakeSMTPServer(t, false)
	srv.advertiseSTARTTLS = true
	host, port := srv.hostPort(t)

	ch := NewEmailChannel(host, port, "", "", "alert-bot@example.com", false)
	if err := ch.Send(context.Background(), "oncall@example.com", Message{Title: "x"}); err != nil {
		t.Fatalf("Send() error = %v (StartTLS=false 时不应尝试真实 TLS 握手)", err)
	}
	srv.wait(t)

	if !strings.Contains(srv.data, "x") {
		t.Errorf("DATA 内容异常: %q", srv.data)
	}
}

// 服务器不支持 STARTTLS 时，即便渠道配置 StartTLS=true 也必须拒绝明文发送，而不是
// 静默降级——这正是 STARTTLS 剥离攻击面：中间人只需摘掉能力广播就能让客户端在明文
// 信道上发出 AUTH PLAIN 的 base64 凭据。
func TestNetSMTPSender_Send_STARTTLSRequiredButUnsupported_Errors(t *testing.T) {
	srv := startFakeSMTPServer(t, false) // advertiseSTARTTLS 默认 false：服务器不支持
	host, port := srv.hostPort(t)

	ch := NewEmailChannel(host, port, "", "", "alert-bot@example.com", true)
	err := ch.Send(context.Background(), "oncall@example.com", Message{Title: "x"})
	if err == nil {
		t.Fatal("Send() error = nil, want 服务器不支持 STARTTLS 时拒绝明文发送")
	}
	srv.wait(t)

	// AUTH/MAIL/RCPT/DATA 均不应发生：必须在 STARTTLS 协商失败后立即中止，不能继续
	// 明文对话。
	if srv.mailFrom != "" || srv.rcptTo != "" || srv.data != "" {
		t.Errorf("STARTTLS 失败后不应继续明文对话，实际 mailFrom=%q rcptTo=%q data=%q", srv.mailFrom, srv.rcptTo, srv.data)
	}
}

func TestNetSMTPSender_Send_RcptRejected(t *testing.T) {
	srv := startFakeSMTPServer(t, false)
	srv.rejectRcpt = true
	host, port := srv.hostPort(t)

	ch := NewEmailChannel(host, port, "", "", "alert-bot@example.com", false)
	err := ch.Send(context.Background(), "nobody@example.com", Message{Title: "x"})
	if err == nil {
		t.Fatal("Send() error = nil, want RCPT TO 被拒绝时返回错误")
	}
	srv.wait(t)
}

func TestNetSMTPSender_Send_DialFailureWrapped(t *testing.T) {
	// 连接一个未监听的本地端口，制造连接失败。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("准备失败端口失败: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ln.Close() // 关闭后该端口大概率立即不可连接

	ch := NewEmailChannel(host, port, "", "", "alert-bot@example.com", false)
	if err := ch.Send(context.Background(), "oncall@example.com", Message{Title: "x"}); err == nil {
		t.Fatal("Send() error = nil, want 连接失败时返回错误")
	}
}
