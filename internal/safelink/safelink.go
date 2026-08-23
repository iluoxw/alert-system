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

import (
	"net/url"
	"strconv"
	"strings"
)

// httpSchemes 是唯一允许的绝对 scheme（大小写不敏感比较）。
var httpSchemes = []string{"http", "https"}

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

// isAbsoluteHTTP 判断是否是 **host 非空**的绝对 http/https URL。
//
// 判定必须真正解析 URL，不能只看"scheme 前缀之后还有字符"：那种前缀检查会放过一整类
// 没有主机名或根本无法解析的值，而浏览器（WHATWG URL）全都拒绝它们——
//
//	https://user@/path            userinfo 之后没有 host
//	https://:443/path             只有端口
//	https://user@?x               userinfo 之后直接是查询串
//	https://[::1                  IPv6 括号未闭合
//	https://exa mple.com          host 里有空格
//	https://evil.example:bad/path 端口不是数字
//	https://example.com:65536/    端口超出 16 位
//
// 于是 UI 会渲染出一个点开后行为未定义的链接。这里用 net/url.Parse 之后**同时**要求：
// 解析成功、绝对 URL、scheme 是 http/https（大小写不敏感）、并且 Hostname() 非空。
// Hostname() 是关键：它剥掉 userinfo 与端口，因此 `user@`、`:443` 这类"authority 文本
// 非空但没有主机名"的值不会被误判成合法。
//
// scheme allowlist 依然是安全边界：`javascript:alert(1)`、`mailto:x` 解析同样"成功"
// （scheme 分别是 javascript / mailto），只是被 scheme 检查挡下。
func isAbsoluteHTTP(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	if !parsed.IsAbs() {
		// 相对形态（含裸主机 `example.com/x`）在这里一律不合法：内部路径必须以 / 开头，
		// 已经在调用方判过。
		return false
	}
	if !isHTTPScheme(parsed.Scheme) {
		return false
	}
	if parsed.Hostname() == "" {
		return false
	}
	return hasUsablePort(parsed)
}

// hasUsablePort 要求"如果写了端口，它必须落在浏览器真的能用的 0..65535 内"。
//
// net/url 只要求端口是数字，因此 `https://example.com:65536/path` 与
// `https://example.com:99999/path` 都能解析成功；而浏览器的 WHATWG URL 对超出 16 位的
// 端口直接抛 TypeError。少了这道检查，这类值就能写进 dashboard_url/runbook_url、被投影
// 出来，最后只在前端 fail closed——即后端认为安全、前端却打不开。
//
// 非数字端口（`:bad`）在更早的 url.Parse 阶段就已经失败，行为不变；没写端口或写成
// `host:`（Port() 返回空）同样不变，交给 scheme 默认端口。
func hasUsablePort(parsed *url.URL) bool {
	port := parsed.Port()
	if port == "" {
		return true
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return number >= 0 && number <= maxPort
}

// maxPort 是 16 位端口上界，与浏览器 URL 解析一致。
const maxPort = 65535

func isHTTPScheme(scheme string) bool {
	for _, allowed := range httpSchemes {
		if strings.EqualFold(scheme, allowed) {
			return true
		}
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
