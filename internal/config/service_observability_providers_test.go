package config

import (
	"bytes"
	"encoding/base64"
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
	"syscall"
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

// Basic 是实测的 OpenObserve 凭据形态：username 与 password 必须分离，且两者都只来自引用。
func TestBasicAuthUsernamePasswordSeparation(t *testing.T) {
	const username = "sre@example.com"
	const password = "oo-password-plaintext"
	t.Setenv("OO_USERNAME_ENV", username)
	t.Setenv("OO_PASSWORD_ENV", password)

	fc := validFileProviders()
	fc.OpenObserve.Endpoint = "https://logs.internal"
	fc.OpenObserve.Organization = "secret-org"
	fc.OpenObserve.Auth = fileProviderAuthConfig{
		Mode: "basic", UsernameEnv: "OO_USERNAME_ENV", PasswordEnv: "OO_PASSWORD_ENV",
	}
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.OpenObserve.Auth.Username.Reveal() != username {
		t.Fatal("username 应从被引用的环境变量读到")
	}
	name, value, ok := config.OpenObserve.Auth.Header()
	if !ok || name != "Authorization" {
		t.Fatalf("header=(%q,%t)", name, ok)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	if value != want {
		t.Fatal("Basic 凭据应是 base64(user:password)")
	}
	// username、organization 与拼好的 header 都不该出现在任何渲染里。
	assertNoLeak(t, config, password, username, "secret-org", value)
}

// username 必须是 provider.Secret 而不是 string。
//
// 退回 string 时"不泄露"就完全依赖外层 String/GoString/MarshalJSON/LogValue 四条路径都记得
// 掩码它——新增一条输出路径、或者有人直接 slog.String("user", auth.Username)，掩码就没了。
// 类型自带掩码后想要明文必须显式 Reveal()，调用点因此少且显眼。
func TestAuthUsernameIsSecretTyped(t *testing.T) {
	field, ok := reflect.TypeOf(ProviderAuthConfig{}).FieldByName("Username")
	if !ok {
		t.Fatal("ProviderAuthConfig 缺少 Username 字段")
	}
	if field.Type != reflect.TypeOf(provider.Secret{}) {
		t.Fatalf("Username 类型是 %s，必须是 provider.Secret", field.Type)
	}
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
	if err := os.WriteFile(oversize, bytes.Repeat([]byte("a"), maxSecretFileBytes+1), 0o600); err != nil {
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

// 字节上界必须是严格的：恰好等于上界被接受，多一个字节被拒绝。
//
// 这一条单独立测是因为它区分了两种实现：靠 Stat().Size() 预判会在 /proc 这类报 0 的文件上
// 失效，只有按 fd 读 max+1 字节才真的有界。
func TestFileSecretRefSizeBoundaryIsExact(t *testing.T) {
	dir := t.TempDir()
	// 内容全是非空白字符：trim 之后长度不变，测到的才是上界本身。
	atLimit := filepath.Join(dir, "at-limit")
	if err := os.WriteFile(atLimit, bytes.Repeat([]byte("a"), maxSecretFileBytes), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	fc := validFileProviders()
	fc.Prometheus.Endpoint = "https://metrics.internal"
	fc.Prometheus.Auth = fileProviderAuthConfig{Mode: "bearer", TokenFile: atLimit}
	config, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
	if err != nil {
		t.Fatalf("恰好等于上界必须被接受: %v", err)
	}
	if got := len(config.Prometheus.Auth.Token.Reveal()); got != maxSecretFileBytes {
		t.Fatalf("读到 %d 字节，want %d", got, maxSecretFileBytes)
	}

	overLimit := filepath.Join(dir, "over-limit")
	if err := os.WriteFile(overLimit, bytes.Repeat([]byte("a"), maxSecretFileBytes+1), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	fc.Prometheus.Auth = fileProviderAuthConfig{Mode: "bearer", TokenFile: overLimit}
	if _, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction); err == nil {
		t.Fatal("超过上界一个字节就必须被拒绝")
	}
}

// 无法安全注入 header 的凭据必须在启动时就被拒绝。
//
// 等到构造请求时才发现已经太晚：CR/LF 那时已经是一次可被拆分的出站请求，username 里的冒号
// 已经把 user:password 的边界挪走了。
func TestHostileCredentialRejectedAtStartup(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
		token    string
		// viaFile 走文件引用而不是 env 引用。NUL 只能这样注入：POSIX 的 environ 以 NUL
		// 分隔，setenv 会直接拒绝含 NUL 的值——但挂载进来的 secret 文件没有这个限制，
		// 所以这条路径反而是真实的攻击面。
		viaFile bool
	}{
		{name: "username 带冒号", username: "sre:admin", password: "ok-password"},
		{name: "username 带 LF", username: "sre\nadmin", password: "ok-password"},
		{name: "username 带 NUL", username: "sre\x00admin", password: "ok-password", viaFile: true},
		{name: "password 带 CRLF", username: "sre", password: "pa\r\nss"},
		{name: "token 带 LF", token: "to\nken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := validFileProviders()
			fc.OpenObserve.Endpoint = "https://logs.internal"
			switch {
			case tc.token != "":
				t.Setenv("HOSTILE_TOKEN_ENV", tc.token)
				fc.OpenObserve.Auth = fileProviderAuthConfig{
					Mode: "bearer", TokenEnv: "HOSTILE_TOKEN_ENV"}
			case tc.viaFile:
				path := filepath.Join(t.TempDir(), "username")
				if err := os.WriteFile(path, []byte(tc.username), 0o600); err != nil {
					t.Fatalf("写文件失败: %v", err)
				}
				t.Setenv("HOSTILE_PASSWORD_ENV", tc.password)
				fc.OpenObserve.Auth = fileProviderAuthConfig{
					Mode:         "basic",
					UsernameFile: path,
					PasswordEnv:  "HOSTILE_PASSWORD_ENV",
				}
			default:
				t.Setenv("HOSTILE_USERNAME_ENV", tc.username)
				t.Setenv("HOSTILE_PASSWORD_ENV", tc.password)
				fc.OpenObserve.Auth = fileProviderAuthConfig{
					Mode:        "basic",
					UsernameEnv: "HOSTILE_USERNAME_ENV",
					PasswordEnv: "HOSTILE_PASSWORD_ENV",
				}
			}
			_, err := normalizeServiceObservabilityProviders(fc, provider.ModeProduction)
			if err == nil {
				t.Fatal("恶意凭据必须让启动失败")
			}
			for _, value := range []string{tc.username, tc.password, tc.token} {
				if value != "" && strings.Contains(err.Error(), value) {
					t.Fatalf("错误信息回显了凭据值: %q", err.Error())
				}
			}
			if !strings.Contains(err.Error(), "openobserve") {
				t.Fatalf("错误信息应指出是哪一项配置: %v", err)
			}
		})
	}
}

// 认证模式的必填与冲突组合必须全部 fail closed。
func TestAuthModeCombinations(t *testing.T) {
	t.Setenv("SOME_TOKEN", "value")
	t.Setenv("SOME_PASSWORD", "value")
	t.Setenv("SOME_USERNAME", "value")

	cases := []struct {
		name string
		auth fileProviderAuthConfig
	}{
		{"未知模式", fileProviderAuthConfig{Mode: "mtls"}},
		{"basic 缺 username 引用", fileProviderAuthConfig{
			Mode: "basic", PasswordEnv: "SOME_PASSWORD"}},
		{"basic 缺 password 引用", fileProviderAuthConfig{
			Mode: "basic", UsernameEnv: "SOME_USERNAME"}},
		{"basic 带 token 引用", fileProviderAuthConfig{
			Mode: "basic", UsernameEnv: "SOME_USERNAME", PasswordEnv: "SOME_PASSWORD",
			TokenEnv: "SOME_TOKEN"}},
		{"username 的 env 与 file 引用互斥", fileProviderAuthConfig{
			Mode: "basic", UsernameEnv: "SOME_USERNAME", UsernameFile: "/tmp/whatever",
			PasswordEnv: "SOME_PASSWORD"}},
		{"引用的 username env 为空", fileProviderAuthConfig{
			Mode: "basic", UsernameEnv: "DEFINITELY_UNSET_ENV", PasswordEnv: "SOME_PASSWORD"}},
		{"bearer 缺 token 引用", fileProviderAuthConfig{Mode: "bearer"}},
		{"bearer 带 username 引用", fileProviderAuthConfig{
			Mode: "bearer", UsernameEnv: "SOME_USERNAME", TokenEnv: "SOME_TOKEN"}},
		{"bearer 带 username file 引用", fileProviderAuthConfig{
			Mode: "bearer", UsernameFile: "/tmp/whatever", TokenEnv: "SOME_TOKEN"}},
		{"bearer 带 password 引用", fileProviderAuthConfig{
			Mode: "bearer", TokenEnv: "SOME_TOKEN", PasswordEnv: "SOME_PASSWORD"}},
		{"none 却带凭据", fileProviderAuthConfig{Mode: "none", TokenEnv: "SOME_TOKEN"}},
		{"none 带 username 引用", fileProviderAuthConfig{
			Mode: "none", UsernameEnv: "SOME_USERNAME"}},
		{"none 带 password 引用", fileProviderAuthConfig{
			Mode: "none", PasswordEnv: "SOME_PASSWORD"}},
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
		config.GoString(),
		config.Prometheus.String(),
		config.Prometheus.GoString(),
		config.OpenObserve.String(),
		config.OpenObserve.GoString(),
		config.Prometheus.Auth.String(),
		config.Prometheus.Auth.GoString(),
		config.OpenObserve.Auth.String(),
		config.OpenObserve.Auth.GoString(),
		fmt.Sprintf("%v", config),
		fmt.Sprintf("%+v", config),
		fmt.Sprintf("%#v", config),
		fmt.Sprintf("%v", config.OpenObserve.Auth),
		fmt.Sprintf("%+v", config.OpenObserve.Auth),
		fmt.Sprintf("%#v", config.OpenObserve.Auth),
		fmt.Sprintf("%q", config.String()),
	}
	for _, value := range []any{config, config.OpenObserve, config.OpenObserve.Auth} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		renderings = append(renderings, string(encoded))
	}

	// 两种 handler 都要试：TextHandler 会走 TextMarshaler，JSONHandler 不一定，
	// 只测一种曾让"掩码其实靠 MarshalText 侥幸成立"这个 bug 溜过去。
	for _, newHandler := range []func(*strings.Builder) slog.Handler{
		func(b *strings.Builder) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *strings.Builder) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var builder strings.Builder
		slog.New(newHandler(&builder)).Info("providers",
			"config", config, "auth", config.OpenObserve.Auth,
			"username", config.OpenObserve.Auth.Username)
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
// 示例文件一旦被复制成部署配置就会带着凭据进版本库。Username 同在禁列：上游账号名也不该有
// 直填形式。
func TestYAMLHasNoCredentialValueField(t *testing.T) {
	authType := reflect.TypeOf(fileProviderAuthConfig{})
	forbidden := map[string]struct{}{
		"Token": {}, "Password": {}, "Secret": {}, "Authorization": {}, "APIKey": {},
		"Username": {},
	}
	for i := 0; i < authType.NumField(); i++ {
		name := authType.Field(i).Name
		if _, bad := forbidden[name]; bad {
			t.Fatalf("YAML auth 配置不应有装凭据值的字段 %s", name)
		}
	}
	// 引用字段必须以 Env 或 File 结尾，形状上就无法承载明文。
	for _, name := range []string{
		"UsernameEnv", "UsernameFile", "PasswordEnv", "PasswordFile", "TokenEnv", "TokenFile",
	} {
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

// auth 的环境变量只覆盖**引用**：只有 `..._ENV` / `..._FILE` 两种后缀。
//
// 反向断言同样重要：一个直填的 `..._AUTH_USERNAME`、或任何 `OBS_*` 简写别名，都必须无效。
// 它们会把值本身搬进进程环境，而进程环境是最容易被误导出的地方；一旦有人"顺手加个兼容"，
// 这里就会红。
func TestProviderAuthRefEnvOverrides(t *testing.T) {
	const username = "env-ref-user"
	const password = "env-ref-password"
	t.Setenv("OO_USERNAME_SOURCE", username)
	t.Setenv("OO_PASSWORD_SOURCE", password)
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_MODE", "basic")
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_USERNAME_ENV", "OO_USERNAME_SOURCE")
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_PASSWORD_ENV", "OO_PASSWORD_SOURCE")
	// 直填与简写别名都不该被认；配上它们不会改变任何结果。
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_USERNAME", "direct-value-user")
	t.Setenv("OBS_AUTH", "direct-value-token")
	t.Setenv("OBS_ENDPOINT", "https://sneaky.internal")

	fc := fileConfig{ServiceObservability: fileServiceObservabilityConfig{Providers: validFileProviders()}}
	fc.ServiceObservability.Providers.OpenObserve.Endpoint = "https://logs.internal"
	applyServiceObservabilityProviderEnvOverrides(&fc)
	if fc.ServiceObservability.Providers.OpenObserve.Auth.UsernameEnv != "OO_USERNAME_SOURCE" {
		t.Fatal("USERNAME_ENV 应覆盖 username 引用")
	}
	config, err := normalizeServiceObservabilityProviders(
		fc.ServiceObservability.Providers, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.OpenObserve.Auth.Username.Reveal() != username {
		t.Fatal("username 应来自 USERNAME_ENV 指向的环境变量，而不是任何直填值")
	}
	if config.OpenObserve.Endpoint.Host() != "logs.internal" {
		t.Fatalf("Host=%q，OBS_ENDPOINT 这类别名不该生效", config.OpenObserve.Endpoint.Host())
	}
	assertNoLeak(t, config, username, password, "direct-value-user", "direct-value-token")
}

func TestUsernameFileEnvOverride(t *testing.T) {
	const username = "file-ref-user"
	path := filepath.Join(t.TempDir(), "username")
	if err := os.WriteFile(path, []byte(username+"\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	t.Setenv("OO_PASSWORD_SOURCE", "file-ref-password")
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_MODE", "basic")
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_USERNAME_FILE", path)
	t.Setenv("SERVICE_OBSERVABILITY_OPENOBSERVE_AUTH_PASSWORD_ENV", "OO_PASSWORD_SOURCE")

	fc := fileConfig{ServiceObservability: fileServiceObservabilityConfig{Providers: validFileProviders()}}
	fc.ServiceObservability.Providers.OpenObserve.Endpoint = "https://logs.internal"
	applyServiceObservabilityProviderEnvOverrides(&fc)
	config, err := normalizeServiceObservabilityProviders(
		fc.ServiceObservability.Providers, provider.ModeProduction)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if config.OpenObserve.Auth.Username.Reveal() != username {
		t.Fatal("username 应从被引用的文件读到并 trim")
	}
	assertNoLeak(t, config, username, "file-ref-password")
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

// FIFO 路径必须立刻被拒绝，而不是把启动挂死。
//
// 只读打开一个没有写入端的 FIFO 会永久阻塞，"不是常规文件"这条校验于是永远执行不到，
// 进程卡在启动里且没有任何错误可看——比拒绝更难排查。这条测试用 2 秒超时把它钉住。
func TestFIFOSecretFileIsRejectedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo-secret")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("当前环境无法创建 FIFO: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readSecretFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO 必须被拒绝")
		}
		if !strings.Contains(err.Error(), "不是常规文件") {
			t.Fatalf("应因非常规文件被拒，实际 %v", err)
		}
		if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), dir) {
			t.Fatalf("错误泄露了路径: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readSecretFile 在 FIFO 上阻塞，启动会被挂死")
	}
}
