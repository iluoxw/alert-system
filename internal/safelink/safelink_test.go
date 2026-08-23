package safelink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 表驱动覆盖策略边界。
//
// 需要显式记录的一点：拒绝 `mailto:`、自定义 scheme、裸主机与 scheme-relative 输入是对
// V1 写入口径的**有意安全收紧**。这些值在 V1 里可以写进 dashboard_url/runbook_url 并被
// 渲染成可点击链接；运营页面的链接会被值班人在事故中直接点开，因此这里从严。历史遗留的
// 非法值不阻断其它服务信息，只在投影时最小范围 fail closed。
// linkPolicyCases 是 Go 与前端共用的策略用例表。
//
// 两侧读同一份 testdata/link_policy_cases.json：这样"Go 与 TS 口径一致"是被测试机械证明
// 的，而不是靠人工对照两份实现。前端的断言在
// web/scripts/verify-service-observability.mjs 里读同一个文件。
type linkPolicyCase struct {
	Name string `json:"name"`
	Href string `json:"href"`
	Safe bool   `json:"safe"`
}

func loadLinkPolicyCases(t *testing.T) []linkPolicyCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "link_policy_cases.json"))
	if err != nil {
		t.Fatalf("读取共享用例表失败: %v", err)
	}
	var table struct {
		Cases []linkPolicyCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("解析共享用例表失败: %v", err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("共享用例表为空")
	}
	return table.Cases
}

// 表驱动覆盖策略边界。
//
// 需要显式记录的一点：拒绝 `mailto:`、自定义 scheme、裸主机与 scheme-relative 输入是对
// V1 写入口径的**有意安全收紧**。这些值在 V1 里可以写进 dashboard_url/runbook_url 并被
// 渲染成可点击链接；运营页面的链接会被值班人在事故中直接点开，因此这里从严。历史遗留的
// 非法值不阻断其它服务信息，只在投影时最小范围 fail closed。
func TestIsSafe(t *testing.T) {
	for _, tc := range loadLinkPolicyCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := IsSafe(tc.Href); got != tc.Safe {
				t.Fatalf("IsSafe(%q) = %v, want %v", tc.Href, got, tc.Safe)
			}
		})
	}
}

// 绝对 URL 必须有**真正的**主机名：只检查"scheme 之后还有字符"会放过 hostless 与
// malformed 值，而浏览器（WHATWG URL）全都拒绝它们。
func TestAbsoluteHTTPRequiresRealHostname(t *testing.T) {
	hostless := []string{
		"https://user@/path", "https://:443/path", "https://user@?x",
		"http:///runbooks", "http://", "https://?x=1", "https://#x",
	}
	for _, href := range hostless {
		if IsSafe(href) {
			t.Fatalf("%q 没有主机名，不应被接受", href)
		}
	}
	malformed := []string{"https://[::1", "https://exa mple.com", "https://evil.example:bad/path"}
	for _, href := range malformed {
		if IsSafe(href) {
			t.Fatalf("%q 无法解析，不应被接受", href)
		}
	}
	// 反向控制：带 userinfo / 端口 / 括号 IPv6 的真实主机必须继续可用。
	for _, href := range []string{
		"https://user@grafana.example.com/d", "https://user:pass@grafana.example.com/d",
		"https://grafana.example.com:3000/d", "https://[::1]:8080/d", "https://[::1]/d",
		"http://10.60.199.160:9090/graph", "HtTpS://grafana.example.com/d",
	} {
		if !IsSafe(href) {
			t.Fatalf("%q 有真实主机名，应被接受", href)
		}
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
