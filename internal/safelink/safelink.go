// Package safelink 是运营页面可点击入口的链接安全策略。
//
// 它存在的唯一目的是让"写入校验"与"投影渲染"共用**同一份** allowlist：两处各写一遍，
// 迟早出现"写得进去、但渲染时又被拦掉"或者反过来"历史脏值被原样渲染成可点击链接"。
//
// 这是一个叶子包：只依赖标准库，因此 internal/api（写入边界）与
// internal/serviceobs/view（Page View 投影）都能引用它，不会破坏
// `api -> facade/view/context -> ports` 的依赖方向，也不会把领域私有 package 拖进
// 通用投影层。
//
// # 策略
//
// 只允许两种形态：
//
//  1. alert-system 内部单斜杠相对路径：以 `/` 开头但不是 `//` 开头；
//  2. 绝对 `http://` / `https://` URL，且 host 非空。
//
// 其余一律拒绝，包括 `javascript:`、`data:`、`vbscript:`、`mailto:`、任何自定义
// scheme、裸主机（`example.com/x`）、scheme-relative（`//example.com`）、控制字符，
// 以及用反斜杠伪造 authority 的写法（`/\example.com`、`\\example.com`）——浏览器会把
// 反斜杠当成斜杠处理，于是一个"看起来是内部路径"的值会跳到外站。
//
// # 这是有意的 V1 安全收紧
//
// 拒绝 `mailto:`、自定义 scheme、裸主机与 scheme-relative 输入，比 V1 的"只 trim"更严：
// 之前这些值可以写进 dashboard_url/runbook_url 并被渲染成可点击链接。收紧是刻意的，
// 因为运营页面的链接会被值班人在事故中直接点开。历史遗留的非法值不阻断其它服务信息，
// 只在投影时最小范围 fail closed（那一个链接不输出）。
package safelink

import "strings"

// httpSchemes 是唯一允许的绝对 scheme（大小写不敏感比较）。
var httpSchemes = []string{"http://", "https://"}

// Normalize 去掉两端空白并返回可用于比较的值。
//
// 只 trim 外层空白：内部空白与控制字符不是"格式问题"，而是明确的非法输入。
func Normalize(raw string) string {
	return strings.TrimSpace(raw)
}

// IsSafe 判断一个链接是否可以作为运营页面的可点击入口。
//
// 空值返回 false：调用方各自决定"空是否合法"——写入校验允许空（未配置 Dashboard/
// Runbook 是正常状态），投影则直接不输出这个链接。
func IsSafe(raw string) bool {
	value := Normalize(raw)
	if value == "" {
		return false
	}
	if containsControlCharacters(value) {
		return false
	}
	// 反斜杠一律拒绝：浏览器把 `/\host` 与 `\\host` 当成 authority 前缀，于是"内部
	// 路径"会变成跳转外站。合法的内部路径与 http(s) URL 都不需要反斜杠。
	if strings.ContainsAny(value, `\`) {
		return false
	}
	if strings.HasPrefix(value, "//") {
		// scheme-relative：继承当前页面协议后跳到任意外站主机。
		return false
	}
	if strings.HasPrefix(value, "/") {
		// alert-system 内部单斜杠相对路径。
		return true
	}
	return isAbsoluteHTTP(value)
}

// SafeOrEmpty 返回可安全渲染的链接；不安全时返回空字符串。
//
// 投影层用它 fail closed：历史遗留的非法值不会变成可点击 href，也不会被"修正"成另一个
// 地址——猜一个 URL 比不给链接更危险。
func SafeOrEmpty(raw string) string {
	if !IsSafe(raw) {
		return ""
	}
	return Normalize(raw)
}

// isAbsoluteHTTP 判断是否是 host 非空的绝对 http/https URL。
//
// 刻意不用 net/url.Parse 做唯一判据：Parse 对 `javascript:alert(1)` 这类输入同样"成功"
// （scheme=javascript），真正的判定还是 scheme allowlist，因此这里直接按前缀白名单判断，
// 语义更直白也更难被绕过。
func isAbsoluteHTTP(value string) bool {
	lowered := strings.ToLower(value)
	for _, scheme := range httpSchemes {
		if !strings.HasPrefix(lowered, scheme) {
			continue
		}
		authority := value[len(scheme):]
		if authority == "" {
			return false
		}
		// host 必须非空：`http:///path` 的 authority 以 `/` 开头，等于没有 host。
		if strings.HasPrefix(authority, "/") || strings.HasPrefix(authority, "?") ||
			strings.HasPrefix(authority, "#") {
			return false
		}
		return true
	}
	return false
}

// containsControlCharacters 与 API 边界同口径：C0 控制字符与 DEL 一律拒绝。
//
// 控制字符可以把一个链接拆成两行、伪造显示文本，或绕过只看前缀的过滤。
func containsControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
