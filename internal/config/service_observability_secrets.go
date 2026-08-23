package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"alert-system/internal/serviceobs/provider"
)

// 本文件是 provider 认证配置。
//
// 实测输入促成了它的形状：OpenObserve 的凭据是 Basic `user:password`，而第一版配置只有一个
// 泛化 token。把 Basic 硬塞进 bearer token 会让部署者要么手工拼 base64、要么把凭据写进
// endpoint 的 userinfo——两条路都绕过"凭据只在 header 注入"这条边界。所以 auth mode 必须显式。
//
// 三条口径：
//
//  1. mode 显式为 none|basic|bearer。缺项、模式与字段不匹配、未连接却带凭据，全部启动失败；
//  2. 每个 secret 只能从 env 名**或**文件路径二选一读取，两者同给即冲突；文件必须是常规
//     文件且有界读取；
//  3. 错误信息只写配置项名。不回显 secret、env 值、文件路径、organization 或 endpoint path——
//     这些都会进启动日志。

// AuthMode 是 provider 的认证方式。
type AuthMode string

const (
	// AuthModeNone 表示上游不需要凭据（例如集群内无鉴权的只读 endpoint）。
	AuthModeNone AuthMode = "none"
	// AuthModeBasic 是 HTTP Basic：username + password。
	AuthModeBasic AuthMode = "basic"
	// AuthModeBearer 是 Bearer token。
	AuthModeBearer AuthMode = "bearer"
)

// IsValid 判断是否是已知模式。空值由 normalize 补成 none。
func (m AuthMode) IsValid() bool {
	switch m {
	case AuthModeNone, AuthModeBasic, AuthModeBearer:
		return true
	default:
		return false
	}
}

// maxSecretFileBytes 是 secret 文件的读取上界。
//
// 4 KiB 远大于任何真实 token，又足够小到"误把整个日志文件当 secret"会立刻失败而不是
// 把几百 MB 读进内存。
const maxSecretFileBytes = 4 * 1024

// ProviderAuthConfig 是解析后的认证配置。凭据全部用 provider.Secret 承载。
type ProviderAuthConfig struct {
	Mode     AuthMode
	Username string
	Password provider.Secret
	Token    provider.Secret
}

// String 只输出模式与"是否已配置"，绝不输出 username 或凭据。
//
// username 也不输出：它属于上游账号信息，进日志同样是泄露面。
func (a ProviderAuthConfig) String() string {
	return fmt.Sprintf("ProviderAuthConfig{Mode:%s, HasUsername:%t, Password:%s, Token:%s}",
		a.Mode, a.Username != "", a.Password, a.Token)
}

// GoString 让 %#v 也走安全表示：否则反射会把 username 与内部字段一起打出来。
func (a ProviderAuthConfig) GoString() string { return "config." + a.String() }

// MarshalJSON / LogValue 堵住 JSON 与 slog 两条输出路径。username 也在收敛范围内：
// 它是上游账号信息，进日志同样是泄露面。
func (a ProviderAuthConfig) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

func (a ProviderAuthConfig) LogValue() slog.Value { return slog.StringValue(a.String()) }

var (
	_ slog.LogValuer = ProviderAuthConfig{}
	_ json.Marshaler = ProviderAuthConfig{}
)

// fileProviderAuthConfig 是 YAML 侧的认证声明。
//
// 它刻意只有**引用**没有值：YAML 里不给凭据留位置，避免示例文件被复制成带明文的部署配置。
type fileProviderAuthConfig struct {
	Mode     string `yaml:"mode"`
	Username string `yaml:"username"`
	// PasswordEnv / PasswordFile 二选一。
	PasswordEnv  string `yaml:"password_env"`
	PasswordFile string `yaml:"password_file"`
	// TokenEnv / TokenFile 二选一。
	TokenEnv  string `yaml:"token_env"`
	TokenFile string `yaml:"token_file"`
}

// normalizeProviderAuth 解析并校验认证配置。
//
// connected 为 false（endpoint 未配置）时不允许携带任何凭据引用：那通常意味着部署者以为
// provider 已经接上了，实际请求根本不会发生。让它启动失败比让页面静默 Unknown 更好。
func normalizeProviderAuth(
	name string, fc fileProviderAuthConfig, connected bool,
) (ProviderAuthConfig, error) {
	mode := AuthMode(strings.TrimSpace(fc.Mode))
	if mode == "" {
		mode = AuthModeNone
	}
	if !mode.IsValid() {
		return ProviderAuthConfig{}, fmt.Errorf(
			"invalid service_observability.providers.%s.auth.mode: must be none, basic or bearer", name)
	}

	hasPasswordRef := fc.PasswordEnv != "" || fc.PasswordFile != ""
	hasTokenRef := fc.TokenEnv != "" || fc.TokenFile != ""

	if !connected {
		if mode != AuthModeNone || fc.Username != "" || hasPasswordRef || hasTokenRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: endpoint 未配置却声明了凭据", name)
		}
		return ProviderAuthConfig{Mode: AuthModeNone}, nil
	}

	auth := ProviderAuthConfig{Mode: mode}
	switch mode {
	case AuthModeNone:
		// none 模式下出现任何凭据引用都是配置矛盾：要么模式写错，要么凭据白写了。
		if fc.Username != "" || hasPasswordRef || hasTokenRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: mode=none 不接受 username/password/token", name)
		}
	case AuthModeBasic:
		if hasTokenRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: mode=basic 不接受 token 引用", name)
		}
		if strings.TrimSpace(fc.Username) == "" {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth.username: mode=basic 必须提供", name)
		}
		password, err := resolveSecretRef(name, "password", fc.PasswordEnv, fc.PasswordFile)
		if err != nil {
			return ProviderAuthConfig{}, err
		}
		auth.Username = strings.TrimSpace(fc.Username)
		auth.Password = password
	case AuthModeBearer:
		if fc.Username != "" || hasPasswordRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: mode=bearer 不接受 username/password", name)
		}
		token, err := resolveSecretRef(name, "token", fc.TokenEnv, fc.TokenFile)
		if err != nil {
			return ProviderAuthConfig{}, err
		}
		auth.Token = token
	}
	return auth, nil
}

// resolveSecretRef 从 env 名或文件路径读取一个凭据。
//
// 两者互斥是刻意的：同时给出时无法确定哪个是权威来源，"猜一个"会让一次凭据轮换看起来
// 生效了其实没有。
func resolveSecretRef(providerName, field, envName, filePath string) (provider.Secret, error) {
	envName = strings.TrimSpace(envName)
	filePath = strings.TrimSpace(filePath)
	switch {
	case envName != "" && filePath != "":
		return provider.Secret{}, fmt.Errorf(
			"invalid service_observability.providers.%s.auth.%s: env 与 file 引用互斥，只能给一个",
			providerName, field)
	case envName != "":
		value := strings.TrimSpace(os.Getenv(envName))
		if value == "" {
			// 只说"哪个配置项的 env 是空的"，不回显 env 名本身的值。
			return provider.Secret{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth.%s: 引用的环境变量为空",
				providerName, field)
		}
		return provider.NewSecret(value), nil
	case filePath != "":
		value, err := readSecretFile(filePath)
		if err != nil {
			// 绝不回显路径：它会暴露部署布局，而且错误信息会进启动日志。
			return provider.Secret{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth.%s: 无法从文件读取凭据（%s）",
				providerName, field, err)
		}
		return provider.NewSecret(value), nil
	default:
		return provider.Secret{}, fmt.Errorf(
			"invalid service_observability.providers.%s.auth.%s: 必须提供 env 或 file 引用",
			providerName, field)
	}
}

// readSecretFile 有界读取一个常规文件。
//
// 返回的错误只带稳定原因短语，不带路径与内容。
func readSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("不可访问")
	}
	// 必须是常规文件：目录、设备、FIFO 都可能让读取阻塞或返回垃圾。
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("不是常规文件")
	}
	if info.Size() > maxSecretFileBytes {
		return "", fmt.Errorf("超过 %d 字节上限", maxSecretFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取失败")
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("内容为空")
	}
	return value, nil
}

// applyProviderAuthEnvOverrides 允许用环境变量覆盖**引用**，但不接受凭据明文。
//
// 这是刻意的不对称：引用（env 名/文件路径/模式/用户名）是部署配置，凭据本身只能来自
// 被引用的 env 或文件。少了这条限制，一个 `..._TOKEN=xxx` 就会让明文出现在进程环境里，
// 而进程环境是最容易被误导出的地方。
func applyProviderAuthEnvOverrides(prefix string, target *fileProviderAuthConfig) {
	base := "SERVICE_OBSERVABILITY_" + prefix + "_AUTH_"
	if v := os.Getenv(base + "MODE"); v != "" {
		target.Mode = v
	}
	if v := os.Getenv(base + "USERNAME"); v != "" {
		target.Username = v
	}
	if v := os.Getenv(base + "PASSWORD_ENV"); v != "" {
		target.PasswordEnv = v
	}
	if v := os.Getenv(base + "PASSWORD_FILE"); v != "" {
		target.PasswordFile = v
	}
	if v := os.Getenv(base + "TOKEN_ENV"); v != "" {
		target.TokenEnv = v
	}
	if v := os.Getenv(base + "TOKEN_FILE"); v != "" {
		target.TokenFile = v
	}
}

// Header 构造该认证模式对应的请求头。
//
// 凭据只在这里进入 header，这是唯一通道：endpoint 的 userinfo 已被 ParseEndpoint 拒绝。
func (a ProviderAuthConfig) Header() (string, string, bool) {
	switch a.Mode {
	case AuthModeBasic:
		if a.Username == "" || a.Password.IsZero() {
			return "", "", false
		}
		return "Authorization", "Basic " + basicCredential(a.Username, a.Password.Reveal()), true
	case AuthModeBearer:
		if a.Token.IsZero() {
			return "", "", false
		}
		return "Authorization", "Bearer " + a.Token.Reveal(), true
	default:
		return "", "", false
	}
}

// basicCredential 生成 Basic 凭据。它只在 Header() 内被调用，结果直接进 request header。
func basicCredential(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
