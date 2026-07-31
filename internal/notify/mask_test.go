package notify

import "testing"

// TestMaskTarget 表驱动覆盖 R3 要求的脱敏规则：邮箱/手机号/URL/短 ID/长 ID/空串。
func TestMaskTarget(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		target string
		want   string
	}{
		{"邮箱按 kind 命中", "email", "alice@example.com", "a***@example.com"},
		{"邮箱按内容嗅探命中（kind 未知）", "unknown", "bob@corp.io", "b***@corp.io"},
		{"手机号按 kind 命中", "sms", "13812345678", "138****5678"},
		{"手机号按内容嗅探命中（kind=lark）", "lark", "13812345678", "138****5678"},
		{"webhook URL 只留 scheme+host", "webhook", "https://hooks.example.com/abc?token=secret", "https://hooks.example.com"},
		{"URL 按内容嗅探命中（kind 未知）", "unknown", "http://x.internal:8080/path?a=1", "http://x.internal:8080"},
		{"短 ID 整体打码", "lark", "ou_1234", "****"},
		{"长度恰好 8 的 ID 整体打码", "lark", "abcdefgh", "****"},
		{"长 ID 保留首尾各 4 字符", "lark", "ou_1234567890abcdef", "ou_1****cdef"},
		{"空串返回空串", "webhook", "", ""},
		{"非 11 位数字回退通用 ID 规则", "sms", "12345", "****"},
		{"email kind 但无 @ 回退通用 ID 规则", "email", "not-an-email", "not-****mail"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskTarget(tc.kind, tc.target)
			if got != tc.want {
				t.Fatalf("maskTarget(%q, %q) = %q，期望 %q", tc.kind, tc.target, got, tc.want)
			}
		})
	}
}
