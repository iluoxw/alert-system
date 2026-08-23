package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs/provider"
)

func validFileProviders() fileServiceObservabilityProvidersConfig {
	return defaultFileProviders()
}

// endpoint 为空是合法的"未连接"：启动成功、页面显式 Unknown。
func TestEmptyEndpointMeansNotConnected(t *testing.T) {
	config, err := normalizeServiceObservabilityProviders(validFileProviders(), provider.ModeProduction)
	if err != nil {
		t.Fatalf("空 endpoint 应合法: %v", err)
	}
	if config.Prometheus.Connected() || config.OpenObserve.Connected() {
		t.Fatal("空 endpoint 应判定为未连接")
	}
}

func TestConfiguredEndpointIsConnected(t *testing.T) {
	fc := validFileProviders()
	fc.Prometheus.Endpoint = "https://metrics.internal/prometheus"
	fc.OpenObserve.Endpoint = "https://logs.internal"
	fc.OpenObserve.Organization = "default"
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("合法配置应通过: %v", err)
	}
	if !config.Prometheus.Connected() || !config.OpenObserve.Connected() {
		t.Fatal("两个 provider 都应判定为已连接")
	}
	if config.Prometheus.Endpoint.Host() != "metrics.internal" {
		t.Fatalf("Host=%q", config.Prometheus.Endpoint.Host())
	}
}

// 显式配置但非法必须让启动失败，而不是静默兜底成默认值。
func TestInvalidProviderConfigFailsStartup(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*fileServiceObservabilityProvidersConfig)
	}{
		{"生产模式 http", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Endpoint = "http://metrics.internal"
		}},
		{"endpoint 带 userinfo", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Endpoint = "https://user:pass@metrics.internal"
		}},
		{"endpoint 带 query", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Endpoint = "https://metrics.internal?org=x"
		}},
		{"endpoint 带 fragment", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Endpoint = "https://metrics.internal#x"
		}},
		{"非 http scheme", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Endpoint = "ftp://metrics.internal"
		}},
		{"timeout 为零", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Timeout = "0s"
		}},
		{"timeout 为负", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Timeout = "-1s"
		}},
		{"timeout 超上界", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Timeout = "10m"
		}},
		{"timeout 不是 duration", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.Timeout = "soon"
		}},
		{"max_response_bytes 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.Prometheus.MaxResponseBytes = 0
		}},
		{"max_concurrent 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.OpenObserve.MaxConcurrent = 0
		}},
		{"max_rows 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.OpenObserve.MaxRows = -1
		}},
		{"max_series 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.OpenObserve.MaxSeries = 0
		}},
		{"evidence_grant_ttl 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.EvidenceGrantTTL = "0s"
		}},
		{"evidence_grant_ttl 超上界", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.EvidenceGrantTTL = "48h"
		}},
		{"evidence_grant_ttl 不是 duration", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.EvidenceGrantTTL = "forever"
		}},
		{"audit_retention_days 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.AuditRetentionDays = 0
		}},
		{"grant_cleanup_batch 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.GrantCleanupBatch = 0
		}},
		{"background_refresh_min_interval 非正", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.BackgroundRefreshMinInterval = "0s"
		}},
		{"background_refresh_min_interval 不是 duration", func(fc *fileServiceObservabilityProvidersConfig) {
			fc.BackgroundRefreshMinInterval = "sometimes"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := validFileProviders()
			tc.apply(&fc)
			if _, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction); err == nil {
				t.Fatal("非法配置必须让启动失败")
			}
		})
	}
}

// 错误信息不得回显 endpoint 原文：它可能含内部主机名。
func TestInvalidEndpointErrorDoesNotEchoValue(t *testing.T) {
	fc := validFileProviders()
	fc.Prometheus.Endpoint = "http://very-secret-internal-host.corp"
	_, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err == nil {
		t.Fatal("应失败")
	}
	if strings.Contains(err.Error(), "very-secret-internal-host") {
		t.Fatalf("错误信息泄露了 endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "prometheus") {
		t.Fatalf("错误信息应指出是哪一项配置: %v", err)
	}
}

// 生产装配一律 ModeProduction：YAML 与环境变量都没有"切成测试模式"的开关。
func TestNoConfigSwitchCanEnableLoopbackHTTP(t *testing.T) {
	fc := validFileProviders()
	fc.Prometheus.Endpoint = "http://127.0.0.1:9090"
	if _, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction); err == nil {
		t.Fatal("生产模式不应接受明文 endpoint，即使是 loopback")
	}
	// 只有代码显式传 ModeLoopbackTest 才允许，而 Load() 永远传 ModeProduction。
	if _, err := normalizeServiceObservabilityProviders(fc, provider.ModeLoopbackTest); err != nil {
		t.Fatalf("测试模式应接受 loopback http: %v", err)
	}
}

// 凭据只从服务端 env 读取，且不出现在 String() 里。
func TestTokensComeFromEnvAndNeverLeak(t *testing.T) {
	const secret = "prom-token-plaintext"
	t.Setenv("SERVICE_OBSERVABILITY_PROMETHEUS_TOKEN", secret)
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_TOKEN", "oo-token-plaintext")

	fc := validFileProviders()
	fc.Prometheus.Endpoint = "https://metrics.internal/prometheus"
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.Prometheus.Token.Reveal() != secret {
		t.Fatal("token 应从环境变量读到")
	}
	rendered := []string{
		config.String(),
		config.Prometheus.String(),
		fmt.Sprintf("%v", config),
		fmt.Sprintf("%+v", config),
	}
	for i, text := range rendered {
		if strings.Contains(text, secret) || strings.Contains(text, "oo-token-plaintext") {
			t.Fatalf("渲染 %d 泄露了 token: %s", i, text)
		}
	}
	// endpoint 的 path 同样不该进日志（它可能含 org/stream 拓扑信息）。
	if strings.Contains(config.String(), "prometheus/") {
		t.Fatalf("String() 泄露了 endpoint path: %s", config.String())
	}
}

// YAML 结构里刻意没有 token 字段：示例文件不能被复制成带明文凭据的部署配置。
func TestYAMLHasNoTokenField(t *testing.T) {
	rendered := fmt.Sprintf("%+v", fileServiceObservabilityProvidersConfig{})
	for _, forbidden := range []string{"Token", "Secret", "Password", "Authorization"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("YAML provider 配置不应有 %s 字段: %s", forbidden, rendered)
		}
	}
}

// 环境变量优先于 YAML；数值写错必须让启动失败而不是回落默认值。
func TestProviderEnvOverrides(t *testing.T) {
	t.Setenv("SERVICE_OBSERVABILITY_PROMETHEUS_ENDPOINT", "https://env.metrics.internal")
	t.Setenv("SERVICE_OBSERVABILITY_PROMETHEUS_MAX_ROWS", "77")
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_ORGANIZATION", "env-org")
	t.Setenv("SERVICE_OBSERVABILITY_AUDIT_RETENTION_DAYS", "7")

	fc := fileConfig{ServiceObservability: fileServiceObservabilityConfig{Providers: validFileProviders()}}
	applyServiceObservabilityProviderEnvOverrides(&fc)
	config, err := normalizeServiceObservabilityProviders(fc.ServiceObservability.Providers, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.Prometheus.Endpoint.Host() != "env.metrics.internal" {
		t.Fatalf("Host=%q，环境变量应生效", config.Prometheus.Endpoint.Host())
	}
	if config.Prometheus.MaxRows != 77 {
		t.Fatalf("MaxRows=%d，want 77", config.Prometheus.MaxRows)
	}
	if config.OpenObserve.Organization != "env-org" {
		t.Fatalf("Organization=%q", config.OpenObserve.Organization)
	}
	if config.AuditRetentionDays != 7 {
		t.Fatalf("AuditRetentionDays=%d，want 7", config.AuditRetentionDays)
	}
}

func TestNonIntegerProviderEnvFailsStartup(t *testing.T) {
	t.Setenv("SERVICE_OBSERVABILITY_PROMETHEUS_MAX_ROWS", "many")
	fc := fileConfig{ServiceObservability: fileServiceObservabilityConfig{Providers: validFileProviders()}}
	applyServiceObservabilityProviderEnvOverrides(&fc)
	if _, err := normalizeServiceObservabilityProviders(
		fc.ServiceObservability.Providers, provider.ModeProduction,
	); err == nil {
		t.Fatal("写错的环境变量必须让启动失败，不能静默回落默认值")
	}
}

// 默认值必须自身合法：一份什么都不配的部署要能启动成功。
func TestDefaultsAreSelfConsistent(t *testing.T) {
	config, err := normalizeServiceObservabilityProviders(defaultFileProviders(), provider.ModeProduction)
	if err != nil {
		t.Fatalf("默认配置必须合法: %v", err)
	}
	if config.EvidenceGrantTTL <= 0 || config.EvidenceGrantTTL > maxEvidenceGrantTTL {
		t.Fatalf("默认 EvidenceGrantTTL=%s 不在允许范围内", config.EvidenceGrantTTL)
	}
	if config.BackgroundRefreshMinInterval <= 0 {
		t.Fatal("默认后台刷新间隔必须为正：它是审计写放大的闸门")
	}
	if config.AuditRetentionDays <= 0 || config.GrantCleanupBatch <= 0 {
		t.Fatal("默认保留与清理批量必须为正")
	}
	if config.Prometheus.Timeout <= 0 || config.Prometheus.Timeout > maxProviderTimeout {
		t.Fatalf("默认 timeout=%s 不在允许范围内", config.Prometheus.Timeout)
	}
	_ = time.Second
}
