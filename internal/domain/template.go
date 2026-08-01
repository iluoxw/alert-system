package domain

import (
	"fmt"
	"hash/fnv"
	"net"
	"strings"
	"unicode"
)

// placeholder 是变量 token 归一化后的占位符（design.md C2）。
const placeholder = "<*>"

// TemplateFingerprint 计算日志/告警标题的模板指纹（R2/C2）：单遍扫描把 IPv4、
// IPv6、UUID、≥8 位十六进制串、连续数字这五类变量 token 替换为占位符 <*>，
// 其余字符（含分隔符本身）原样保留，再对归一化结果做 FNV-64a 哈希，取
// %016x——与 Fingerprint（fingerprint.go）用同一哈希算法，风格对齐，但这里
// 只有一个字符串输入，不需要 writeField 那套多字段分隔符手法。
//
// 只差变量的同模板文本（如 "Ping timeout from 10.0.0.1" 与 "...10.0.0.2"）
// 归一化后完全一致，因而哈希相同；不含变量的文本没有 token 被替换，归一化
// 结果就是原文，仍会正常走同一哈希（AC2.1 所谓的「原样哈希」）。
//
// 纯函数：不做任何 IO；同输入永远同输出；单遍扫描 O(len(text))。
func TemplateFingerprint(text string) string {
	h := fnv.New64a()
	h.Write([]byte(normalizeTemplate(text)))
	return fmt.Sprintf("%016x", h.Sum64())
}

// normalizeTemplate 单遍扫描 text：按分隔符切出 token，逐 token 判类，命中
// IPv4/IPv6/UUID/hex(>=8)/连续数字任一类则整体替换为 placeholder，其余字符
// （含分隔符）原样保留——同一模板的分隔符文本理应逐字节相同，没必要归整。
//
// 只做整 token 匹配，不做子串查找替换：token 内部只要混入分隔符集合之外的
// 非变量字符（如字母），整个 token 就保持原样，不会被“部分替换”成半个占位符
// （见 template_test.go 的粘连边界用例）。
func normalizeTemplate(text string) string {
	var b strings.Builder
	b.Grow(len(text))

	var token strings.Builder
	flushToken := func() {
		if token.Len() == 0 {
			return
		}
		t := token.String()
		if isVariableToken(t) {
			b.WriteString(placeholder)
		} else {
			b.WriteString(t)
		}
		token.Reset()
	}

	for _, r := range text {
		if isDelimiter(r) {
			flushToken()
			b.WriteRune(r)
			continue
		}
		token.WriteRune(r)
	}
	flushToken()

	return b.String()
}

// isDelimiter 判定 r 是否为 token 分隔符：空白，以及除 '.'/':'/'-' 外的其余
// 非字母数字字符。保留这三个符号在 token 内部不切分，是因为 IPv4（点分）、
// IPv6（冒号分）、UUID（连字符分）都依赖它们作为组内结构；若把它们当分隔符，
// "10.0.0.1" 会被切成四段数字，UUID/IPv6 同理碎成无法识别的片段。
func isDelimiter(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	return r != '.' && r != ':' && r != '-'
}

// isVariableToken 判定整个 token 是否属于 IPv4/IPv6/UUID/hex(>=8)/连续数字
// 五类之一。IPv4/IPv6 用标准库 net.ParseIP 判定，避免手写正则重新实现一遍
// 容易出错的 IP 格式校验（两者都归一为同一 placeholder，不需要区分具体是
// 哪一类）。
func isVariableToken(t string) bool {
	return net.ParseIP(t) != nil || isUUID(t) || isHexAtLeast8(t) || isDigitsOnly(t)
}

// isUUID 判定 t 是否为标准 UUID 格式：5 段以 '-' 分隔，长度依次 8-4-4-4-12，
// 每段只含十六进制字符（大小写不敏感）。
func isUUID(t string) bool {
	parts := strings.Split(t, "-")
	if len(parts) != 5 {
		return false
	}
	wantLens := [5]int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != wantLens[i] || !isHexOnly(p) {
			return false
		}
	}
	return true
}

// isHexAtLeast8 判定 t 是否为长度 >= 8 的纯十六进制字符串（design.md「≥8 位
// 十六进制串」）。纯数字 token（如 "12345678"）同时满足本条件与 isDigitsOnly，
// 但两者归一结果都是同一个 placeholder，不影响输出，无需再排优先级。
func isHexAtLeast8(t string) bool {
	return len(t) >= 8 && isHexOnly(t)
}

// isHexOnly 判定 s 是否非空且全部由十六进制字符（0-9a-fA-F）组成。
func isHexOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// isDigitsOnly 判定 s 是否非空且全部由十进制数字组成（design.md「连续数字」，
// 不限最短长度，与 hex 串的 ≥8 位要求不同）。
func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
