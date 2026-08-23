package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

// 凭据只从服务端 env/file 引用读取，且不出现在任何渲染里。
func TestBearerTokenComesFromEnvRefAndNeverLeaks(t *testing.T) {
	const secret = "prom-token-plaintext"
	t.Setenv("PROM_TOKEN_ENV", secret)

	fc := validFileProviders()
	fc.Prometheus.Endpoint = "https://metrics.internal/prometheus"
	fc.Prometheus.Auth = fileProviderAuthConfig{Mode: "bearer", TokenEnv: "PROM_TOKEN_ENV"}
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.Prometheus.Auth.Mode != AuthModeBearer {
		t.Fatalf("mode=%q", config.Prometheus.Auth.Mode)
	}
	if config.Prometheus.Auth.Token.Reveal() != secret {
		t.Fatal("token 应从被引用的环境变量读到")
	}
	name, value, ok := config.Prometheus.Auth.Header()
	if !ok || name != "Authorization" || value != "Bearer "+secret {
		t.Fatalf("header=(%q,%q,%t)", name, value, ok)
	}
	assertNoLeak(t, config, secret, "PROM_TOKEN_ENV")
}

// Basic 是实测的 OpenObserve 凭据形态：username 与 password 必须分离。
func TestBasicAuthUsernamePasswordSeparation(t *testing.T) {
	const password = "oo-password-plaintext"
	t.Setenv("OO_PASSWORD_ENV", password)

	fc := validFileProviders()
	fc.OpenObserve.Endpoint = "https://logs.internal"
	fc.OpenObserve.Organization = "secret-org"
	fc.OpenObserve.Auth = fileProviderAuthConfig{
		Mode: "basic", Username: "sre@example.com", PasswordEnv: "OO_PASSWORD_ENV",
	}
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.OpenObserve.Auth.Username != "sre@example.com" {
		t.Fatalf("username=%q", config.OpenObserve.Auth.Username)
	}
	name, value, ok := config.OpenObserve.Auth.Header()
	if !ok || name != "Authorization" || !strings.HasPrefix(value, "Basic ") {
		t.Fatalf("header=(%q,%q,%t)", name, value, ok)
	}
	// username 与 organization 都不该出现在任何渲染里。
	assertNoLeak(t, config, password, "sre@example.com", "secret-org", value)
}

// 文件引用：必须是常规文件、有界、trim，且错误不回显路径。
func TestFileSecretRefBounds(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte("  file-token-value\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	fc := validFileProviders()
	fc.Prometheus.Endpoint = "https://metrics.internal"
	fc.Prometheus.Auth = fileProviderAuthConfig{Mode: "bearer", TokenFile: good}
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("合法文件引用应通过: %v", err)
	}
	if config.Prometheus.Auth.Token.Reveal() != "file-token-value" {
		t.Fatalf("文件凭据未被 trim: %q", config.Prometheus.Auth.Token.Reveal())
	}

	oversize := filepath.Join(dir, "oversize")
	if err := os.WriteFile(oversize, make([]byte, maxSecretFileBytes+1), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	cases := map[string]string{
		"超过上界":   oversize,
		"内容为空":   empty,
		"不存在":    filepath.Join(dir, "missing"),
		"不是常规文件": dir,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			broken := validFileProviders()
			broken.Prometheus.Endpoint = "https://metrics.internal"
			broken.Prometheus.Auth = fileProviderAuthConfig{Mode: "bearer", TokenFile: path}
			_, err := normalizeServiceObservabilityProviders(broken, provider.ModeProduction)
			if err == nil {
				t.Fatal("应失败")
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), dir) {
				t.Fatalf("错误信息回显了文件路径: %v", err)
			}
		})
	}
}

// 认证模式的必填与冲突组合必须全部 fail closed。
func TestAuthModeCombinations(t *testing.T) {
	t.Setenv("SOME_TOKEN", "value")
	t.Setenv("SOME_PASSWORD", "value")

	cases := []struct {
		name string
		auth fileProviderAuthConfig
	}{
		{"未知模式", fileProviderAuthConfig{Mode: "mtls"}},
		{"basic 缺 username", fileProviderAuthConfig{Mode: "basic", PasswordEnv: "SOME_PASSWORD"}},
		{"basic 缺 password 引用", fileProviderAuthConfig{Mode: "basic", Username: "u"}},
		{"basic 带 token 引用", fileProviderAuthConfig{
			Mode: "basic", Username: "u", PasswordEnv: "SOME_PASSWORD", TokenEnv: "SOME_TOKEN"}},
		{"bearer 缺 token 引用", fileProviderAuthConfig{Mode: "bearer"}},
		{"bearer 带 username", fileProviderAuthConfig{
			Mode: "bearer", Username: "u", TokenEnv: "SOME_TOKEN"}},
		{"bearer 带 password 引用", fileProviderAuthConfig{
			Mode: "bearer", TokenEnv: "SOME_TOKEN", PasswordEnv: "SOME_PASSWORD"}},
		{"none 却带凭据", fileProviderAuthConfig{Mode: "none", TokenEnv: "SOME_TOKEN"}},
		{"env 与 file 引用互斥", fileProviderAuthConfig{
			Mode: "bearer", TokenEnv: "SOME_TOKEN", TokenFile: "/tmp/whatever"}},
		{"引用的 env 为空", fileProviderAuthConfig{Mode: "bearer", TokenEnv: "DEFINITELY_UNSET_ENV"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := validFileProviders()
			fc.Prometheus.Endpoint = "https://metrics.internal"
			fc.Prometheus.Auth = tc.auth
			if _, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction); err == nil {
				t.Fatal("应让启动失败")
			}
		})
	}
}

// endpoint 未配置却声明凭据：通常意味着部署者以为已经接上了。必须启动失败。
func TestCredentialsWithoutEndpointFailStartup(t *testing.T) {
	t.Setenv("SOME_TOKEN", "value")
	fc := validFileProviders()
	fc.Prometheus.Auth = fileProviderAuthConfig{Mode: "bearer", TokenEnv: "SOME_TOKEN"}
	if _, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction); err == nil {
		t.Fatal("未连接却带凭据必须失败")
	}
}

// none 模式不产生任何 header。
func TestNoneModeProducesNoHeader(t *testing.T) {
	fc := validFileProviders()
	fc.Prometheus.Endpoint = "https://metrics.internal"
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.Prometheus.Auth.Mode != AuthModeNone {
		t.Fatalf("默认模式应是 none，得到 %q", config.Prometheus.Auth.Mode)
	}
	if _, _, ok := config.Prometheus.Auth.Header(); ok {
		t.Fatal("none 模式不应产生 header")
	}
}

// assertNoLeak 用全部格式化路径与两种 slog handler 检查泄露。
func assertNoLeak(t *testing.T, config ServiceObservabilityProvidersConfig, secrets ...string) {
	t.Helper()
	renderings := []string{
		config.String(),
		config.Prometheus.String(),
		config.OpenObserve.String(),
		fmt.Sprintf("%v", config),
		fmt.Sprintf("%+v", config),
		fmt.Sprintf("%#v", config),
		fmt.Sprintf("%q", config.String()),
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	renderings = append(renderings, string(encoded))

	for _, newHandler := range []func(*strings.Builder) slog.Handler{
		func(b *strings.Builder) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *strings.Builder) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var builder strings.Builder
		slog.New(newHandler(&builder)).Info("providers", "config", config)
		renderings = append(renderings, builder.String())
	}

	for i, text := range renderings {
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			if strings.Contains(text, secret) {
				t.Fatalf("渲染 %d 泄露了 %q: %s", i, secret, text)
			}
		}
	}
}

// YAML 结构只允许**引用**，不允许装凭据值。
//
// 检查的是字段名本身：`TokenEnv`/`TokenFile`/`PasswordEnv`/`PasswordFile` 是引用（env 名或
// 文件路径），是设计的一部分；而一个恰好叫 Token/Password/Secret 的字段会把明文引到 YAML 里，
// 示例文件一旦被复制成部署配置就会带着凭据进版本库。
func TestYAMLHasNoCredentialValueField(t *testing.T) {
	authType := reflect.TypeOf(fileProviderAuthConfig{})
	forbidden := map[string]struct{}{
		"Token": {}, "Password": {}, "Secret": {}, "Authorization": {}, "APIKey": {},
	}
	for i := 0; i < authType.NumField(); i++ {
		name := authType.Field(i).Name
		if _, bad := forbidden[name]; bad {
			t.Fatalf("YAML auth 配置不应有装凭据值的字段 %s", name)
		}
	}
	// 引用字段必须以 Env 或 File 结尾，形状上就无法承载明文。
	for _, name := range []string{"PasswordEnv", "PasswordFile", "TokenEnv", "TokenFile"} {
		field, ok := authType.FieldByName(name)
		if !ok {
			t.Fatalf("缺少引用字段 %s", name)
		}
		if !strings.HasSuffix(field.Name, "Env") && !strings.HasSuffix(field.Name, "File") {
			t.Fatalf("%s 不是引用形状", field.Name)
		}
	}
	// endpoint 侧同样不该出现凭据字段。
	endpointType := reflect.TypeOf(fileProviderEndpointConfig{})
	for i := 0; i < endpointType.NumField(); i++ {
		name := endpointType.Field(i).Name
		if _, bad := forbidden[name]; bad {
			t.Fatalf("YAML endpoint 配置不应有装凭据值的字段 %s", name)
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
