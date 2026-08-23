package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"unicode"

	"alert-system/internal/serviceobs/provider"
)

// 本文件是 provider 认证配置。
//
// 实测输入促成了它的形状：OpenObserve 的凭据是 Basic `user:password`，而第一版配置只有一个
// 泛化 token。把 Basic 硬塞进 bearer token 会让部署者要么手工拼 base64、要么把凭据写进
// endpoint 的 userinfo——两条路都绕过"凭据只在 header 注入"这条边界。所以 auth mode 必须显式。
//
// 四条口径：
//
//  1. mode 显式为 none|basic|bearer。缺项、模式与字段不匹配、未连接却带凭据，全部启动失败；
//  2. 配置里只有**引用**：env 变量名或文件路径，二选一，两者同给即冲突。值本身始终留在被
//     引用的 env 或文件里——也就是说引用目标才是真正需要保护的对象，配置项本身不是凭据；
//     文件必须是常规文件且有界读取；
//  3. username 与 password/token 同等对待。它是上游账号，同样只能来自引用，也同样用
//     provider.Secret 承载，任何渲染路径都拿不到明文；
//  4. 错误信息只写配置项名。不回显 secret、env 值、文件路径、organization 或 endpoint path——
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
//
// Username 也是 Secret 而不是 string：它是上游账号，同样是泄露面。做成 string 时"不泄露"
// 完全依赖 String/GoString/MarshalJSON/LogValue 四条路径都记得掩码它——新增一条输出路径、
// 或者有人直接 slog.String("user", auth.Username)，掩码就断了。类型自带掩码后想拿明文必须
// 显式 Reveal()，调用点因此少且显眼。
type ProviderAuthConfig struct {
	Mode     AuthMode
	Username provider.Secret
	Password provider.Secret
	Token    provider.Secret
}

// String 只输出模式与"是否已配置"，绝不输出 username 或凭据。
func (a ProviderAuthConfig) String() string {
	return fmt.Sprintf("ProviderAuthConfig{Mode:%s, HasUsername:%t, Password:%s, Token:%s}",
		a.Mode, !a.Username.IsZero(), a.Password, a.Token)
}

// GoString 让 %#v 也走安全表示：否则反射会把结构体的内部字段逐个铺开，绕过 String 的收敛。
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
	Mode string `yaml:"mode"`
	// UsernameEnv / UsernameFile 二选一。username 没有直填形式：上游账号名同样值得保护，
	// 留一个直填字段就等于给"把账号写进 YAML 并提交"开了口子。
	UsernameEnv  string `yaml:"username_env"`
	UsernameFile string `yaml:"username_file"`
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

	hasUsernameRef := fc.UsernameEnv != "" || fc.UsernameFile != ""
	hasPasswordRef := fc.PasswordEnv != "" || fc.PasswordFile != ""
	hasTokenRef := fc.TokenEnv != "" || fc.TokenFile != ""

	if !connected {
		if mode != AuthModeNone || hasUsernameRef || hasPasswordRef || hasTokenRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: endpoint 未配置却声明了凭据", name)
		}
		return ProviderAuthConfig{Mode: AuthModeNone}, nil
	}

	auth := ProviderAuthConfig{Mode: mode}
	switch mode {
	case AuthModeNone:
		// none 模式下出现任何凭据引用都是配置矛盾：要么模式写错，要么凭据白写了。
		if hasUsernameRef || hasPasswordRef || hasTokenRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: mode=none 不接受 username/password/token", name)
		}
	case AuthModeBasic:
		if hasTokenRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: mode=basic 不接受 token 引用", name)
		}
		// username 与 password 都走 resolveSecretRef：缺引用时它给出的就是"必须提供 env 或
		// file 引用"，与其他缺项同一口径，不需要在这里再判一次。
		username, err := resolveSecretRef(name, "username", fc.UsernameEnv, fc.UsernameFile)
		if err != nil {
			return ProviderAuthConfig{}, err
		}
		if err := assertHeaderSafeUsername(name, "username", username.Reveal()); err != nil {
			return ProviderAuthConfig{}, err
		}
		password, err := resolveSecretRef(name, "password", fc.PasswordEnv, fc.PasswordFile)
		if err != nil {
			return ProviderAuthConfig{}, err
		}
		if err := assertHeaderSafeSecret(name, "password", password.Reveal()); err != nil {
			return ProviderAuthConfig{}, err
		}
		auth.Username = username
		auth.Password = password
	case AuthModeBearer:
		if hasUsernameRef || hasPasswordRef {
			return ProviderAuthConfig{}, fmt.Errorf(
				"invalid service_observability.providers.%s.auth: mode=bearer 不接受 username/password", name)
		}
		token, err := resolveSecretRef(name, "token", fc.TokenEnv, fc.TokenFile)
		if err != nil {
			return ProviderAuthConfig{}, err
		}
		if err := assertHeaderSafeSecret(name, "token", token.Reveal()); err != nil {
			return ProviderAuthConfig{}, err
		}
		auth.Token = token
	}
	return auth, nil
}

// assertHeaderSafeSecret 在启动时就拒绝无法安全注入 header 的凭据。
//
// 校验必须发生在归一时而不是构造请求时：CR/LF 在 header 值里等于一次 header 注入/请求拆分，
// 而那时它已经是一条在途的出站请求；启动失败是唯一还来得及的失败方式。其余控制字符也一并
// 拒绝——不同代理对它们的处理不一致，"能不能通"取决于链路而不是我们的代码。
func assertHeaderSafeSecret(providerName, field, value string) error {
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf(
				"invalid service_observability.providers.%s.auth.%s: 含控制字符，无法安全注入 header",
				providerName, field)
		}
	}
	return nil
}

// assertHeaderSafeUsername 在控制字符之外再禁止冒号。
//
// Basic 凭据是 base64("user:password")，上游按**第一个**冒号切分。username 里的冒号会把
// 账号的后半段变成密码的前半段，认证于是以一种极难排查的方式失败——或者更糟，正好匹配上
// 另一个账号。
func assertHeaderSafeUsername(providerName, field, value string) error {
	if err := assertHeaderSafeSecret(providerName, field, value); err != nil {
		return err
	}
	if strings.Contains(value, ":") {
		return fmt.Errorf(
			"invalid service_observability.providers.%s.auth.%s: 不允许冒号，它会改变 user:password 的边界",
			providerName, field)
	}
	return nil
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
// 先 Open、再对**同一个 fd** Stat：先按路径 Stat 再按路径读会留一个替换窗口，通过校验的是
// 旧 inode，读到的可能已经是被换成 FIFO 或设备的新目标——secret 文件所在目录往往由挂载的
// secret 管理器写入，这个窗口不是理论问题。
//
// 上界也不靠 Size()：/proc 之类的文件会报 0 而内容无界，只有真正的有界读取算数。
//
// O_NONBLOCK 是为了让"不是常规文件"这条校验真的有机会执行：只读打开一个 FIFO 会一直阻塞到
// 有写入端出现，进程于是永远卡在启动里——比直接拒绝更糟，因为没有任何错误可看。加上它之后
// FIFO 立刻返回，随后被 IsRegular 拒掉。对常规文件这个标志没有作用。
//
// 返回的错误只带稳定原因短语，不带路径与内容。
func readSecretFile(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("不可访问")
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("不可访问")
	}
	// 必须是常规文件：目录、设备、FIFO 都可能让读取阻塞或返回垃圾。
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("不是常规文件")
	}
	// 多读一个字节判断"是否严格超限"：恰好等于上界必须被接受。
	raw, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取失败")
	}
	if len(raw) > maxSecretFileBytes {
		return "", fmt.Errorf("超过 %d 字节上限", maxSecretFileBytes)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("内容为空")
	}
	return value, nil
}

// applyProviderAuthEnvOverrides 允许用环境变量覆盖**引用**，但不接受凭据明文。
//
// 这是刻意的不对称：引用（模式、env 名、文件路径）是部署配置，值只能来自被引用的 env 或
// 文件。少了这条限制，一个 `..._TOKEN=xxx` 或 `..._USERNAME=xxx` 就会让明文出现在进程环境里，
// 而进程环境是最容易被误导出的地方。所以这里只有 `..._ENV` / `..._FILE` 两种后缀。
func applyProviderAuthEnvOverrides(prefix string, target *fileProviderAuthConfig) {
	base := "SERVICE_OBSERVABILITY_" + prefix + "_AUTH_"
	if v := os.Getenv(base + "MODE"); v != "" {
		target.Mode = v
	}
	if v := os.Getenv(base + "USERNAME_ENV"); v != "" {
		target.UsernameEnv = v
	}
	if v := os.Getenv(base + "USERNAME_FILE"); v != "" {
		target.UsernameFile = v
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
		if a.Username.IsZero() || a.Password.IsZero() {
			return "", "", false
		}
		return "Authorization", "Basic " + basicCredential(a.Username.Reveal(), a.Password.Reveal()), true
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
//
// 这里不再校验 username/password 的字符：归一时已经拒过冒号与控制字符，非法凭据根本活不到
// 启动完成。放在这里补一道判断只会把"配置错误"变成一次运行期静默失败。
func basicCredential(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
