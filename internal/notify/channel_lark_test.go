package notify

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeLarkMessenger 是 larkMessenger 的内存 fake：按调用顺序记录每次 SendMessage 的
// 参数，并按 results 队列依次返回预设结果，供测试断言卡片 JSON 结构与降级链路。
type fakeLarkMessenger struct {
	calls   []fakeLarkCall
	results []fakeLarkResult // 与 calls 一一对应，用尽后默认成功
}

type fakeLarkCall struct {
	receiveIDType, receiveID, msgType, content string
}

type fakeLarkResult struct {
	result larkMessageResult
	err    error
}

func (f *fakeLarkMessenger) SendMessage(ctx context.Context, receiveIDType, receiveID, msgType, content string) (larkMessageResult, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, fakeLarkCall{receiveIDType, receiveID, msgType, content})
	if idx < len(f.results) {
		return f.results[idx].result, f.results[idx].err
	}
	return larkMessageResult{}, nil
}

func newLarkChannelForTest(m *fakeLarkMessenger) *LarkChannel {
	return &LarkChannel{messenger: m}
}

// 场景：正常发送单告警卡片——断言配色模板、按钮（Ack/Resolve/详情链接）、组摘要缺席。
func TestLarkChannel_Send_NormalCard(t *testing.T) {
	m := &fakeLarkMessenger{}
	ch := newLarkChannelForTest(m)

	msg := Message{
		AlertIDs:  []string{"alert-1"},
		Severity:  "critical",
		Title:     "CPU 使用率过高",
		Body:      "host-1 CPU 使用率持续超过 95%",
		DetailURL: "https://example.com/alerts/alert-1",
	}
	if err := ch.Send(context.Background(), "chat_id:oc_abc", msg); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	if len(m.calls) != 1 {
		t.Fatalf("期望只调用 1 次 SendMessage，实际 %d 次", len(m.calls))
	}
	call := m.calls[0]
	if call.receiveIDType != "chat_id" || call.receiveID != "oc_abc" {
		t.Fatalf("target 解析错误: %+v", call)
	}
	if call.msgType != "interactive" {
		t.Fatalf("期望 msgType=interactive，实际 %q", call.msgType)
	}

	var card map[string]any
	if err := json.Unmarshal([]byte(call.content), &card); err != nil {
		t.Fatalf("卡片 JSON 解析失败: %v", err)
	}
	header, ok := card["header"].(map[string]any)
	if !ok {
		t.Fatalf("卡片缺少 header: %v", card)
	}
	if header["template"] != "red" {
		t.Fatalf("critical 期望配色 red，实际 %v", header["template"])
	}

	elements, ok := card["elements"].([]any)
	if !ok || len(elements) < 2 {
		t.Fatalf("期望至少 2 个 element（正文+按钮组），实际 %v", card["elements"])
	}
	actionEl, ok := elements[len(elements)-1].(map[string]any)
	if !ok || actionEl["tag"] != "action" {
		t.Fatalf("期望最后一个 element 是 action，实际 %v", elements[len(elements)-1])
	}
	actions, _ := actionEl["actions"].([]any)
	if len(actions) != 3 {
		t.Fatalf("单告警发送期望 3 个按钮（认领/解决/详情），实际 %d 个: %v", len(actions), actions)
	}
	ackBtn, _ := actions[0].(map[string]any)
	ackValue, _ := ackBtn["value"].(map[string]any)
	if ackValue["action"] != "ack" || ackValue["alert_id"] != "alert-1" {
		t.Fatalf("认领按钮 value 不符合预期: %v", ackValue)
	}
}

// 场景：分组通知——不应出现 Ack/Resolve 按钮，正文应包含聚合组摘要。
func TestLarkChannel_Send_GroupCard_NoAckResolveButtons(t *testing.T) {
	m := &fakeLarkMessenger{}
	ch := newLarkChannelForTest(m)

	msg := Message{
		AlertIDs: []string{"alert-1", "alert-2", "alert-3"},
		GroupKey: "group-x",
		Severity: "warning",
		Title:    "3 条 warning 级告警",
		Body:     "汇总摘要",
	}
	if err := ch.Send(context.Background(), "oc_group", msg); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	call := m.calls[0]
	var card map[string]any
	if err := json.Unmarshal([]byte(call.content), &card); err != nil {
		t.Fatalf("卡片 JSON 解析失败: %v", err)
	}
	elements, _ := card["elements"].([]any)
	div, _ := elements[0].(map[string]any)
	text, _ := div["text"].(map[string]any)
	content, _ := text["content"].(string)
	if !contains(content, "group-x") || !contains(content, "3 条告警") {
		t.Fatalf("正文缺少聚合组摘要: %q", content)
	}
	for _, el := range elements {
		e, _ := el.(map[string]any)
		if e["tag"] == "action" {
			actions, _ := e["actions"].([]any)
			for _, a := range actions {
				btn, _ := a.(map[string]any)
				if v, ok := btn["value"].(map[string]any); ok {
					t.Fatalf("分组发送不应包含 ack/resolve 按钮: %v", v)
				}
			}
		}
	}
}

func TestBuildAlertCard_ResourceActionsOnlyExposeImplementedOperations(t *testing.T) {
	cases := []struct {
		name, resourceType, action string
	}{
		{name: "fault", resourceType: "fault", action: "advance"},
		{name: "incident", resourceType: "incident", action: "mitigate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, err := buildAlertCard(Message{ResourceType: tc.resourceType, ResourceID: "00000000-0000-0000-0000-000000000001", Title: tc.name})
			if err != nil {
				t.Fatalf("buildAlertCard 失败: %v", err)
			}
			var card map[string]any
			if err := json.Unmarshal([]byte(content), &card); err != nil {
				t.Fatalf("卡片 JSON 解析失败: %v", err)
			}
			elements, _ := card["elements"].([]any)
			actionEl, _ := elements[len(elements)-1].(map[string]any)
			actions, _ := actionEl["actions"].([]any)
			if len(actions) != 1 {
				t.Fatalf("%s 卡片应只有 1 个已实现动作，实际 %d", tc.resourceType, len(actions))
			}
			button, _ := actions[0].(map[string]any)
			value, _ := button["value"].(map[string]any)
			if value["resource_type"] != tc.resourceType || value["action"] != tc.action || value["version"] != float64(1) {
				t.Fatalf("动作信封不符合预期: %v", value)
			}
		})
	}
}

// 场景：卡片发送遇到权限不足错误 → 降级为纯文本+链接重发一次。
func TestLarkChannel_Send_PermissionDenied_DegradesToText(t *testing.T) {
	m := &fakeLarkMessenger{
		results: []fakeLarkResult{
			{result: larkMessageResult{Code: 9499, Msg: "Forbidden"}, err: errUnexported("boom")},
		},
	}
	ch := newLarkChannelForTest(m)

	msg := Message{
		AlertIDs:  []string{"alert-9"},
		Title:     "磁盘空间不足",
		Body:      "host-2 磁盘剩余不足 5%",
		DetailURL: "https://example.com/alerts/alert-9",
	}
	if err := ch.Send(context.Background(), "chat_id:oc_x", msg); err != nil {
		t.Fatalf("期望降级后成功返回，实际 err=%v", err)
	}

	if len(m.calls) != 2 {
		t.Fatalf("期望 2 次调用（卡片失败 + 文本降级），实际 %d 次", len(m.calls))
	}
	if m.calls[0].msgType != "interactive" {
		t.Fatalf("第一次应为 interactive，实际 %q", m.calls[0].msgType)
	}
	if m.calls[1].msgType != "text" {
		t.Fatalf("第二次应降级为 text，实际 %q", m.calls[1].msgType)
	}
	var textBody map[string]any
	if err := json.Unmarshal([]byte(m.calls[1].content), &textBody); err != nil {
		t.Fatalf("降级文本 JSON 解析失败: %v", err)
	}
	text, _ := textBody["text"].(string)
	if !contains(text, "磁盘空间不足") || !contains(text, "https://example.com/alerts/alert-9") {
		t.Fatalf("降级文本内容不完整: %q", text)
	}
}

// 场景：非权限类错误不应触发降级，原样返回给 Sender 重试。
func TestLarkChannel_Send_OtherError_DoesNotDegrade(t *testing.T) {
	m := &fakeLarkMessenger{
		results: []fakeLarkResult{
			{result: larkMessageResult{Code: 500, Msg: "internal error"}, err: errUnexported("boom")},
		},
	}
	ch := newLarkChannelForTest(m)

	msg := Message{AlertIDs: []string{"alert-1"}, Title: "t"}
	if err := ch.Send(context.Background(), "oc_x", msg); err == nil {
		t.Fatal("期望非权限错误原样返回")
	}
	if len(m.calls) != 1 {
		t.Fatalf("非权限错误不应触发降级重发，期望 1 次调用，实际 %d 次", len(m.calls))
	}
}

func TestIsPermissionDenied(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want bool
	}{
		{9499, "Forbidden", true},
		{0, "no permission to send this message", true},
		{0, "机器人无该群组权限", true},
		{500, "internal error", false},
	}
	for _, c := range cases {
		if got := isPermissionDenied(c.code, c.msg); got != c.want {
			t.Errorf("isPermissionDenied(%d, %q) = %v, want %v", c.code, c.msg, got, c.want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		target       string
		wantType, id string
	}{
		{"oc_plain", "chat_id", "oc_plain"},
		{"chat_id:oc_x", "chat_id", "oc_x"},
		{"open_id:ou_x", "open_id", "ou_x"},
		{"unknown:foo", "chat_id", "unknown:foo"},
	}
	for _, c := range cases {
		gotType, gotID := parseTarget(c.target)
		if gotType != c.wantType || gotID != c.id {
			t.Errorf("parseTarget(%q) = (%q, %q), want (%q, %q)", c.target, gotType, gotID, c.wantType, c.id)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (substr == "" || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

type errUnexported string

func (e errUnexported) Error() string { return string(e) }
