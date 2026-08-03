package runbook

import (
	"fmt"
	"strings"
	"time"
)

// UATPolicyConfig 只能由服务端启动配置构造。普通 Runbook 请求必须逐项匹配这里的
// 隔离环境、变更授权和模板白名单，不能通过请求参数自行把 UAT 能力打开。
type UATPolicyConfig struct {
	Enabled                bool
	EnvironmentID          string
	AuthorizationRef       string
	AuthorizationExpiresAt time.Time
	AllowedTemplateIDs     []string
}

type UATPolicy struct {
	enabled                bool
	environmentID          string
	authorizationRef       string
	authorizationExpiresAt time.Time
	allowedTemplateIDs     map[string]struct{}
}

func NewUATPolicy(config UATPolicyConfig) UATPolicy {
	allowed := make(map[string]struct{}, len(config.AllowedTemplateIDs))
	for _, templateID := range config.AllowedTemplateIDs {
		if value := strings.TrimSpace(templateID); value != "" {
			allowed[value] = struct{}{}
		}
	}
	return UATPolicy{
		enabled: config.Enabled, environmentID: strings.TrimSpace(config.EnvironmentID),
		authorizationRef:       strings.TrimSpace(config.AuthorizationRef),
		authorizationExpiresAt: config.AuthorizationExpiresAt, allowedTemplateIDs: allowed,
	}
}

func (p UATPolicy) Enabled() bool { return p.enabled }

// Availability 返回可安全展示给 UI 的运行时能力状态，不泄露授权引用、模板或密钥。
func (p UATPolicy) Availability(now time.Time) (bool, string) {
	switch {
	case !p.enabled:
		return false, "服务端未启用隔离 UAT 执行能力"
	case p.environmentID == "" || p.authorizationRef == "" || p.authorizationExpiresAt.IsZero() || len(p.allowedTemplateIDs) == 0:
		return false, "UAT 授权配置不完整"
	case !now.Before(p.authorizationExpiresAt):
		return false, "UAT 授权已过期"
	default:
		return true, ""
	}
}

func (p UATPolicy) Authorize(parameters map[string]any, now time.Time) error {
	if available, reason := p.Availability(now); !available {
		return fmt.Errorf("%w: %s", ErrUATAuthorizationRequired, reason)
	}
	environmentID, _ := parameters["environment_id"].(string)
	authorizationRef, _ := parameters["authorization_ref"].(string)
	templateID, _ := parameters["template_id"].(string)
	confirmCleanup, _ := parameters["confirm_cleanup"].(bool)
	if strings.TrimSpace(environmentID) != p.environmentID ||
		strings.TrimSpace(authorizationRef) != p.authorizationRef || !confirmCleanup {
		return fmt.Errorf("%w: 请求未匹配有效的隔离 UAT 授权", ErrUATAuthorizationRequired)
	}
	if _, ok := p.allowedTemplateIDs[strings.TrimSpace(templateID)]; !ok {
		return fmt.Errorf("%w: 模板不在服务端 UAT 白名单", ErrUATAuthorizationRequired)
	}
	return nil
}
