package safelink

import "testing"

// 表驱动覆盖策略边界。
//
// 需要显式记录的一点：拒绝 `mailto:`、自定义 scheme、裸主机与 scheme-relative 输入是对
// V1 写入口径的**有意安全收紧**。这些值在 V1 里可以写进 dashboard_url/runbook_url 并被
// 渲染成可点击链接；运营页面的链接会被值班人在事故中直接点开，因此这里从严。历史遗留的
// 非法值不阻断其它服务信息，只在投影时最小范围 fail closed。
func TestIsSafe(t *testing.T) {
	cases := []struct {
		name string
		href string
		safe bool
	}{
		// 合法：alert-system 内部单斜杠路径。
		{"内部根路径", "/", true},
		{"内部路径", "/settings/services/svc-1", true},
		{"内部路径带查询", "/alerts?service_id=svc-1", true},
		{"内部路径带片段", "/faults/FLT-1#timeline", true},
		{"内部路径两端空白被 trim", "  /runbooks/e2b  ", true},
		{"内部路径含编码字符", "/runbooks/%E4%B8%AD%E6%96%87", true},

		// 合法：绝对 http/https。
		{"https URL", "https://grafana.example.com/d/abc", true},
		{"http URL", "http://grafana.example.com/d/abc", true},
		{"混合大小写 scheme", "HtTpS://grafana.example.com/d/abc", true},
		{"带端口", "https://grafana.example.com:3000/d/abc", true},
		{"带认证信息仍是 http(s)", "https://user@grafana.example.com/d", true},

		// 非法：危险 scheme（含大小写与空白变形）。
		{"javascript", "javascript:alert(1)", false},
		{"javascript 大写", "JavaScript:alert(1)", false},
		{"javascript 前置空白", "  javascript:alert(1)", false},
		{"data", "data:text/html;base64,PHNjcmlwdD4=", false},
		{"vbscript", "vbscript:msgbox(1)", false},
		{"mailto 是有意收紧", "mailto:sre@example.com", false},
		{"tel 是有意收紧", "tel:+8613800000000", false},
		{"file", "file:///etc/passwd", false},
		{"自定义 scheme", "firebreak://open/service", false},

		// 非法：scheme-relative 与裸主机。
		{"scheme-relative", "//evil.example.com/path", false},
		{"scheme-relative 带空白", "  //evil.example.com", false},
		{"裸主机", "grafana.example.com/d/abc", false},
		{"裸主机带端口", "grafana.example.com:3000", false},
		{"相对路径无前导斜杠", "runbooks/e2b", false},

		// 非法：反斜杠伪造 authority（浏览器把 \ 当 /）。
		{"反斜杠 authority", `/\evil.example.com`, false},
		{"双反斜杠", `\\evil.example.com`, false},
		{"http URL 内含反斜杠", `https://grafana.example.com\@evil.example.com`, false},

		// 非法：控制字符与空值。
		{"换行注入", "/runbooks\ne2b", false},
		{"回车注入", "/runbooks\re2b", false},
		{"制表符", "/runbooks\te2b", false},
		{"NUL", "/runbooks\x00e2b", false},
		{"DEL", "/runbooks\x7fe2b", false},
		{"javascript 中夹控制字符", "java\x01script:alert(1)", false},
		{"空字符串", "", false},
		{"只有空白", "   ", false},

		// 非法：http 但没有 host。
		{"http 无 host", "http://", false},
		{"http 三斜杠", "http:///runbooks", false},
		{"http 只有查询", "https://?x=1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSafe(tc.href); got != tc.safe {
				t.Fatalf("IsSafe(%q) = %v, want %v", tc.href, got, tc.safe)
			}
		})
	}
}

// SafeOrEmpty 是投影层的 fail closed 入口：不安全时返回空串，安全时返回 trim 后的值。
// 它绝不"修正"一个不安全的链接——猜一个 URL 比不给链接更危险。
func TestSafeOrEmpty(t *testing.T) {
	if got := SafeOrEmpty("  /runbooks/e2b  "); got != "/runbooks/e2b" {
		t.Fatalf("SafeOrEmpty 应返回 trim 后的安全值, got %q", got)
	}
	for _, unsafe := range []string{
		"javascript:alert(1)", "//evil.example.com", "grafana.example.com", `/\evil`, "", "   ",
	} {
		if got := SafeOrEmpty(unsafe); got != "" {
			t.Fatalf("SafeOrEmpty(%q) = %q, want 空字符串", unsafe, got)
		}
	}
}

// Normalize 只去掉外层空白，不改变内部内容。
func TestNormalizeOnlyTrimsOuterWhitespace(t *testing.T) {
	if got := Normalize("  /a b  "); got != "/a b" {
		t.Fatalf("Normalize = %q", got)
	}
	if got := Normalize(""); got != "" {
		t.Fatalf("Normalize(\"\") = %q", got)
	}
}
