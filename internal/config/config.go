// Package config 提供服务配置的加载：默认值 -> YAML 文件 -> 环境变量，
// 环境变量优先级最高。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
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

	// defaultSMTPStartTLS 默认开启，与 SessionCookieSecure 同样的"默认从严、本地联调显式关闭"取舍。
	defaultSMTPStartTLS = true

	// defaultPersonnelChannel 是人员到人通知的默认渠道（R3：email 是首个真实到人渠道）。
	defaultPersonnelChannel = "email"

	// 保留策略默认值（R5）：timers 保留 7 天、notification_log 保留 90 天，
	// 清理任务每小时对账一次，每批最多删 1000 行（防长事务锁表）。
	defaultRetentionTimersDays          = 7
	defaultRetentionNotificationLogDays = 90
	defaultRetentionSweepInterval       = time.Hour
	defaultRetentionBatchSize           = 1000

	// defaultFlapWindowSeconds 默认 0（关闭）：flapping 防抖是 opt-in 能力
	// （aiops-foundation R4/C4），未显式配置窗口时行为与现状逐字节一致。
	defaultFlapWindowSeconds = 0
	// defaultFlapThreshold 是窗口内状态翻转次数阈值，仅在窗口>0 时生效。
	defaultFlapThreshold = 3
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
	CORSAllowedOrigins  []string      // 浏览器跨 origin 直连 API 时允许的精确 Web origin

	RepeatInterval time.Duration // R6.5 重复提醒周期，main 装配注入 notify.LifecycleHandlers

	SMTP             SMTPConfig      // R3 email 渠道凭据
	PersonnelChannel string          // R3 人员到人通知的默认渠道，默认 "email"
	Retention        RetentionConfig // R5 timers/notification_log 数据保留策略

	// FlapWindowSeconds/FlapThreshold 是 flapping 防抖（aiops-foundation R4）的判定窗口
	// 与翻转次数阈值；FlapWindowSeconds<=0 表示关闭（opt-in，默认关闭）。
	FlapWindowSeconds int
	FlapThreshold     int
}

// SMTPConfig 是 email 通知渠道（R3）所需的 SMTP 凭据配置。Host 为空表示未配置，
// 渠道仍照常注册，发送时返回"SMTP 未配置"错误，走既有 markFailed 链路。
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	StartTLS bool
}

// String 实现 fmt.Stringer：Password 是凭据，脱敏原则与 LarkConfig.String 一致。
func (s SMTPConfig) String() string {
	return fmt.Sprintf("SMTPConfig{Host:%q, Port:%d, Username:%q, Password:%s, From:%q, StartTLS:%t}",
		s.Host, s.Port, s.Username, maskSecret(s.Password), s.From, s.StartTLS)
}

// RetentionConfig 是 R5 数据保留清理任务（retention_sweep timer）的策略配置。
type RetentionConfig struct {
	TimersDays          int           // timers 表：终态（fired/failed/canceled）且早于此天数的行会被清理
	NotificationLogDays int           // notification_log 表：早于此天数的行会被清理
	SweepInterval       time.Duration // 无积压时的对账周期
	BatchSize           int           // 单批 DELETE 的最大行数
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

	SessionTTL          string   `yaml:"session_ttl"`
	SessionIdleTTL      string   `yaml:"session_idle_ttl"`
	SessionCookieSecure bool     `yaml:"session_cookie_secure"`
	CORSAllowedOrigins  []string `yaml:"cors_allowed_origins"`

	RepeatInterval string `yaml:"repeat_interval"`

	SMTP             fileSMTPConfig      `yaml:"smtp"`
	PersonnelChannel string              `yaml:"personnel_channel"`
	Retention        fileRetentionConfig `yaml:"retention"`

	FlapWindowSeconds int `yaml:"flap_window_seconds"`
	FlapThreshold     int `yaml:"flap_threshold"`
}

type fileSMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`
	StartTLS bool   `yaml:"start_tls"`
}

type fileRetentionConfig struct {
	TimersDays          int    `yaml:"timers_days"`
	NotificationLogDays int    `yaml:"notification_log_days"`
	SweepInterval       string `yaml:"sweep_interval"`
	BatchSize           int    `yaml:"batch_size"`
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
		SMTP: fileSMTPConfig{
			StartTLS: defaultSMTPStartTLS,
		},
		PersonnelChannel: defaultPersonnelChannel,
		Retention: fileRetentionConfig{
			TimersDays:          defaultRetentionTimersDays,
			NotificationLogDays: defaultRetentionNotificationLogDays,
			SweepInterval:       defaultRetentionSweepInterval.String(),
			BatchSize:           defaultRetentionBatchSize,
		},
		FlapWindowSeconds: defaultFlapWindowSeconds,
		FlapThreshold:     defaultFlapThreshold,
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
	retentionSweepInterval, err := time.ParseDuration(fc.Retention.SweepInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid retention.sweep_interval %q: %w", fc.Retention.SweepInterval, err)
	}
	corsAllowedOrigins, err := normalizeOrigins(fc.CORSAllowedOrigins)
	if err != nil {
		return Config{}, err
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
		CORSAllowedOrigins:  corsAllowedOrigins,
		RepeatInterval:      repeatInterval,
		SMTP: SMTPConfig{
			Host:     fc.SMTP.Host,
			Port:     fc.SMTP.Port,
			Username: fc.SMTP.Username,
			Password: fc.SMTP.Password,
			From:     fc.SMTP.From,
			StartTLS: fc.SMTP.StartTLS,
		},
		PersonnelChannel: fc.PersonnelChannel,
		Retention: RetentionConfig{
			TimersDays:          fc.Retention.TimersDays,
			NotificationLogDays: fc.Retention.NotificationLogDays,
			SweepInterval:       retentionSweepInterval,
			BatchSize:           fc.Retention.BatchSize,
		},
		FlapWindowSeconds: fc.FlapWindowSeconds,
		FlapThreshold:     fc.FlapThreshold,
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
	if v := os.Getenv("CORS_ALLOWED_ORIGINS"); v != "" {
		fc.CORSAllowedOrigins = strings.Split(v, ",")
	}
	if v := os.Getenv("REPEAT_INTERVAL"); v != "" {
		fc.RepeatInterval = v
	}
	if v := os.Getenv("SMTP_HOST"); v != "" {
		fc.SMTP.Host = v
	}
	if v := os.Getenv("SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			fc.SMTP.Port = p
		}
	}
	if v := os.Getenv("SMTP_USERNAME"); v != "" {
		fc.SMTP.Username = v
	}
	if v := os.Getenv("SMTP_PASSWORD"); v != "" {
		fc.SMTP.Password = v
	}
	if v := os.Getenv("SMTP_FROM"); v != "" {
		fc.SMTP.From = v
	}
	if v := os.Getenv("SMTP_STARTTLS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			fc.SMTP.StartTLS = b
		}
	}
	if v := os.Getenv("PERSONNEL_CHANNEL"); v != "" {
		fc.PersonnelChannel = v
	}
	if v := os.Getenv("RETENTION_TIMERS_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			fc.Retention.TimersDays = n
		}
	}
	if v := os.Getenv("RETENTION_NOTIFICATION_LOG_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			fc.Retention.NotificationLogDays = n
		}
	}
	if v := os.Getenv("RETENTION_SWEEP_INTERVAL"); v != "" {
		fc.Retention.SweepInterval = v
	}
	if v := os.Getenv("RETENTION_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			fc.Retention.BatchSize = n
		}
	}
	if v := os.Getenv("FLAP_WINDOW_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			fc.FlapWindowSeconds = n
		}
	}
	if v := os.Getenv("FLAP_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			fc.FlapThreshold = n
		}
	}
}

func normalizeOrigins(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	origins := make([]string, 0, len(values))
	for _, value := range values {
		origin := strings.TrimSpace(value)
		if origin == "" {
			continue
		}
		if origin == "*" {
			return nil, fmt.Errorf("invalid cors_allowed_origins %q: wildcard is incompatible with credentialed requests", origin)
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid cors_allowed_origins %q: expected an http(s) origin without path", origin)
		}
		normalized := u.Scheme + "://" + u.Host
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		origins = append(origins, normalized)
	}
	return origins, nil
}
