// Package config 提供服务配置的加载：默认值 -> YAML 文件 -> 环境变量，
// 环境变量优先级最高。
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultHTTPPort     = 8080
	defaultDBDSN        = "postgres://localhost:5432/alert_system?sslmode=disable"
	defaultPollInterval = 5 * time.Second

	// defaultSessionTTL 是登录会话的固定过期时长（自登录起算，不受滑动续期影响）。
	defaultSessionTTL = 7 * 24 * time.Hour
	// defaultSessionIdleTTL 是滑动续期窗口：会话超过这个时长无请求即失效，即使
	// 固定过期时间还没到（R8.3"固定过期 + 滑动续期"）。
	defaultSessionIdleTTL = 30 * time.Minute
	// defaultSessionCookieSecure 默认为 true；本地 http 联调需要通过环境变量关闭。
	defaultSessionCookieSecure = true

	// defaultRepeatInterval 是 R6.5 重复提醒的默认周期（MVP 阶段尚未按路由配置化，
	// 由 main 装配统一注入 notify.LifecycleHandlers，见该类型构造函数文档）。
	defaultRepeatInterval = 30 * time.Minute
)

// Config 是服务启动所需的全部配置项。
type Config struct {
	HTTPPort              int
	DBDSN                 string
	SchedulerPollInterval time.Duration
	Lark                  LarkConfig

	SessionTTL          time.Duration // 会话固定过期时长
	SessionIdleTTL      time.Duration // 会话滑动续期窗口
	SessionCookieSecure bool          // session cookie 的 Secure 标志，本地 http 联调可关闭

	RepeatInterval time.Duration // R6.5 重复提醒周期，main 装配注入 notify.LifecycleHandlers
}

// LarkConfig 是飞书开放平台凭据配置（R6.2/R6.4 飞书渠道与回调验签）。
// VerificationToken/EncryptKey 为空表示不启用对应校验/加解密（与 SDK 自身的
// no-op-when-empty 约定一致）。
type LarkConfig struct {
	AppID             string
	AppSecret         string
	VerificationToken string
	EncryptKey        string
}

// String 实现 fmt.Stringer：AppSecret/VerificationToken/EncryptKey 是凭据，打印/记录
// 配置时必须脱敏（与 maskDSN 对 DSN 密码的处理原则一致，避免敏感信息落日志）；
// AppID 不敏感，原样保留便于排查是哪个应用。
func (l LarkConfig) String() string {
	return fmt.Sprintf("LarkConfig{AppID:%q, AppSecret:%s, VerificationToken:%s, EncryptKey:%s}",
		l.AppID, maskSecret(l.AppSecret), maskSecret(l.VerificationToken), maskSecret(l.EncryptKey))
}

// maskSecret 把非空敏感值替换为固定占位符，空值原样标出，不泄露"是否配置了该项"之外的
// 任何信息。
func maskSecret(s string) string {
	if s == "" {
		return `""`
	}
	return `"****"`
}

// fileConfig 对应 YAML 文件字段，均使用基础类型，便于与环境变量按同一套逻辑覆盖。
type fileConfig struct {
	HTTPPort              int            `yaml:"http_port"`
	DBDSN                 string         `yaml:"db_dsn"`
	SchedulerPollInterval string         `yaml:"scheduler_poll_interval"`
	Lark                  fileLarkConfig `yaml:"lark"`

	SessionTTL          string `yaml:"session_ttl"`
	SessionIdleTTL      string `yaml:"session_idle_ttl"`
	SessionCookieSecure bool   `yaml:"session_cookie_secure"`

	RepeatInterval string `yaml:"repeat_interval"`
}

type fileLarkConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
}

// Load 加载配置：先应用默认值，再用 YAML 文件（若存在）覆盖，最后用环境变量覆盖。
// path 为空字符串或文件不存在时跳过文件加载。
func Load(path string) (Config, error) {
	fc := fileConfig{
		HTTPPort:              defaultHTTPPort,
		DBDSN:                 defaultDBDSN,
		SchedulerPollInterval: defaultPollInterval.String(),
		SessionTTL:            defaultSessionTTL.String(),
		SessionIdleTTL:        defaultSessionIdleTTL.String(),
		SessionCookieSecure:   defaultSessionCookieSecure,
		RepeatInterval:        defaultRepeatInterval.String(),
	}

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &fc); err != nil {
				return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// 无配置文件，使用默认值 + 环境变量
		default:
			return Config{}, fmt.Errorf("read config file %s: %w", path, err)
		}
	}

	applyEnvOverrides(&fc)

	poll, err := time.ParseDuration(fc.SchedulerPollInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid scheduler_poll_interval %q: %w", fc.SchedulerPollInterval, err)
	}
	sessionTTL, err := time.ParseDuration(fc.SessionTTL)
	if err != nil {
		return Config{}, fmt.Errorf("invalid session_ttl %q: %w", fc.SessionTTL, err)
	}
	sessionIdleTTL, err := time.ParseDuration(fc.SessionIdleTTL)
	if err != nil {
		return Config{}, fmt.Errorf("invalid session_idle_ttl %q: %w", fc.SessionIdleTTL, err)
	}
	repeatInterval, err := time.ParseDuration(fc.RepeatInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid repeat_interval %q: %w", fc.RepeatInterval, err)
	}

	return Config{
		HTTPPort:              fc.HTTPPort,
		DBDSN:                 fc.DBDSN,
		SchedulerPollInterval: poll,
		Lark: LarkConfig{
			AppID:             fc.Lark.AppID,
			AppSecret:         fc.Lark.AppSecret,
			VerificationToken: fc.Lark.VerificationToken,
			EncryptKey:        fc.Lark.EncryptKey,
		},
		SessionTTL:          sessionTTL,
		SessionIdleTTL:      sessionIdleTTL,
		SessionCookieSecure: fc.SessionCookieSecure,
		RepeatInterval:      repeatInterval,
	}, nil
}

// applyEnvOverrides 用环境变量覆盖 fc 中的同名项，环境变量优先级高于 YAML 文件。
func applyEnvOverrides(fc *fileConfig) {
	if v := os.Getenv("HTTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			fc.HTTPPort = p
		}
	}
	if v := os.Getenv("DB_DSN"); v != "" {
		fc.DBDSN = v
	}
	if v := os.Getenv("SCHEDULER_POLL_INTERVAL"); v != "" {
		fc.SchedulerPollInterval = v
	}
	if v := os.Getenv("LARK_APP_ID"); v != "" {
		fc.Lark.AppID = v
	}
	if v := os.Getenv("LARK_APP_SECRET"); v != "" {
		fc.Lark.AppSecret = v
	}
	if v := os.Getenv("LARK_VERIFICATION_TOKEN"); v != "" {
		fc.Lark.VerificationToken = v
	}
	if v := os.Getenv("LARK_ENCRYPT_KEY"); v != "" {
		fc.Lark.EncryptKey = v
	}
	if v := os.Getenv("SESSION_TTL"); v != "" {
		fc.SessionTTL = v
	}
	if v := os.Getenv("SESSION_IDLE_TTL"); v != "" {
		fc.SessionIdleTTL = v
	}
	if v := os.Getenv("SESSION_COOKIE_SECURE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			fc.SessionCookieSecure = b
		}
	}
	if v := os.Getenv("REPEAT_INTERVAL"); v != "" {
		fc.RepeatInterval = v
	}
}
