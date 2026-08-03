package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"alert-system/internal/runbook"
)

type e2bUATRuntime struct {
	Executor             runbook.Executor
	Policy               runbook.UATPolicy
	Host                 string
	AllowedTemplateCount int
	AuthorizationExpires time.Time
}

// buildE2BUATRuntime 只在显式启动开关为 true 时读取一组专用 UAT 环境变量；绝不
// 回退到 E2B_API_URL/E2B_API_KEY，避免把已有生产凭据误接成演练执行器。
func buildE2BUATRuntime(enabled bool) (e2bUATRuntime, error) {
	if !enabled {
		return e2bUATRuntime{Executor: runbook.DisabledExecutor{}}, nil
	}
	required := []string{
		"E2B_UAT_API_URL", "E2B_UAT_API_KEY", "E2B_UAT_ENVIRONMENT_ID",
		"E2B_UAT_API_ALLOWED_IPS", "E2B_UAT_TEMPLATE_IDS", "E2B_UAT_AUTHORIZATION_REF",
		"E2B_UAT_AUTHORIZATION_EXPIRES_AT",
	}
	values := make(map[string]string, len(required))
	for _, key := range required {
		values[key] = strings.TrimSpace(os.Getenv(key))
		if values[key] == "" {
			return e2bUATRuntime{}, fmt.Errorf("启用 E2B UAT Runbook 需要环境变量 %s", key)
		}
	}
	parsed, err := url.Parse(values["E2B_UAT_API_URL"])
	if err != nil || parsed.Hostname() == "" {
		return e2bUATRuntime{}, fmt.Errorf("E2B_UAT_API_URL 非法")
	}
	if !hasDNSLabel(parsed.Hostname(), "uat") {
		return e2bUATRuntime{}, fmt.Errorf("E2B_UAT_API_URL host 必须包含独立的 uat DNS label")
	}
	expiresAt, err := time.Parse(time.RFC3339, values["E2B_UAT_AUTHORIZATION_EXPIRES_AT"])
	if err != nil {
		return e2bUATRuntime{}, fmt.Errorf("E2B_UAT_AUTHORIZATION_EXPIRES_AT 必须是 RFC3339: %w", err)
	}
	if !time.Now().Before(expiresAt) {
		return e2bUATRuntime{}, fmt.Errorf("E2B_UAT_AUTHORIZATION_EXPIRES_AT 已过期")
	}
	templateIDs := splitNonEmpty(values["E2B_UAT_TEMPLATE_IDS"])
	if len(templateIDs) == 0 {
		return e2bUATRuntime{}, fmt.Errorf("E2B_UAT_TEMPLATE_IDS 必须包含至少一个模板")
	}
	executor, err := runbook.NewE2BUATExecutor(runbook.E2BUATExecutorConfig{
		BaseURL: values["E2B_UAT_API_URL"], APIKey: values["E2B_UAT_API_KEY"],
		AllowedHost: parsed.Hostname(), AllowedIPs: splitNonEmpty(values["E2B_UAT_API_ALLOWED_IPS"]),
	})
	if err != nil {
		return e2bUATRuntime{}, err
	}
	policy := runbook.NewUATPolicy(runbook.UATPolicyConfig{
		Enabled: true, EnvironmentID: values["E2B_UAT_ENVIRONMENT_ID"],
		AuthorizationRef: values["E2B_UAT_AUTHORIZATION_REF"], AuthorizationExpiresAt: expiresAt,
		AllowedTemplateIDs: templateIDs,
	})
	return e2bUATRuntime{
		Executor: runbook.NewRoutingExecutor(runbook.DisabledExecutor{}, map[string]runbook.Executor{
			runbook.ActionLifecycleVerify: executor,
		}),
		Policy: policy, Host: parsed.Hostname(), AllowedTemplateCount: len(templateIDs),
		AuthorizationExpires: expiresAt,
	}, nil
}

func hasDNSLabel(host, expected string) bool {
	for _, label := range strings.Split(strings.ToLower(host), ".") {
		if label == expected {
			return true
		}
	}
	return false
}

func splitNonEmpty(value string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0)
	for _, raw := range strings.Split(value, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result
}
