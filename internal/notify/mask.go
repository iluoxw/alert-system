package notify

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// phonePattern 只识别纯 11 位数字的国内手机号；夹杂国家码/分隔符等不确定格式一律
// 按通用 ID 规则脱敏，避免对不确定格式做过度假设。
var phonePattern = regexp.MustCompile(`^\d{11}$`)

// maskTarget 对通知 target 做脱敏，供时间线 detail 使用（R3 安全约束：时间线 detail
// 不含完整敏感标识）。kind 是发送用的渠道类型（如 "webhook"/"lark"/"email"/"sms"），
// 命中已知渠道时按其固定格式脱敏；kind 未命中（含 lark 这类 target 本身是不透明 ID
// 字符串的渠道）时回退到按 target 自身格式特征识别，兼容邮件/短信等尚未接入的渠道。
func maskTarget(kind, target string) string {
	if target == "" {
		return ""
	}
	switch kind {
	case "email":
		return maskEmail(target)
	case "sms", "phone":
		return maskPhone(target)
	case "webhook":
		return maskURL(target)
	}
	switch {
	case strings.Contains(target, "@"):
		return maskEmail(target)
	case phonePattern.MatchString(target):
		return maskPhone(target)
	case strings.Contains(target, "://"):
		return maskURL(target)
	default:
		return maskGenericID(target)
	}
}

// maskEmail 保留 local 部分首字符，其余替换为 ***，domain 部分保留：a***@domain。
func maskEmail(target string) string {
	at := strings.Index(target, "@")
	if at <= 0 {
		return maskGenericID(target)
	}
	local, domain := target[:at], target[at+1:]
	return fmt.Sprintf("%c***@%s", local[0], domain)
}

// maskPhone 保留前 3、后 4 位，中间打码：138****1234。非 11 位数字回退通用 ID 规则。
func maskPhone(target string) string {
	if len(target) != 11 {
		return maskGenericID(target)
	}
	return target[:3] + "****" + target[7:]
}

// maskURL 只保留 scheme://host，丢弃路径/query（可能带鉴权 token 等敏感信息）。
// 解析失败或缺 scheme/host（不是合法 URL）时回退通用 ID 规则。
func maskURL(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return maskGenericID(target)
	}
	return u.Scheme + "://" + u.Host
}

// maskGenericID 是兜底规则：保留首尾各 4 字符、中间固定打码为 ****；
// 长度 ≤8 时首尾会重叠泄露过多信息，直接整体打码为 ****。
func maskGenericID(target string) string {
	if len(target) <= 8 {
		return "****"
	}
	return target[:4] + "****" + target[len(target)-4:]
}
