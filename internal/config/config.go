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

	// defaultIncidentNudgeInterval 是 Incident 恢复提醒（incident_recovery_nudge）
	// 的默认轮询周期：证据全部解除却无人收尾的 Incident 每隔这么久被提醒一次。
	// <=0 表示关闭，不调度、handler 直接无副作用返回（见 internal/app/incident_nudge.go）。
	defaultIncidentNudgeInterval = 6 * time.Hour

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

	// defaultCCloudSyncInterval 是 ccloud 拓扑/健康度独立周期 loop（不变量 23）的默认
	// 拉取间隔；<=0 表示整条链路关闭，不产生任何外部 IO（R2.1）。
	defaultCCloudSyncInterval = 10 * time.Minute

	// defaultFaultNotifyCooldown 是 R5.3 冷却窗口的默认时长：同一 correlation_key
	// 在这段时间内刚 resolved，本轮不重新通知。
	defaultFaultNotifyCooldown = 30 * time.Minute
	// defaultFaultNotifyFallbackChannel 是 owner team 未配置渠道时的全局兜底渠道
	// 种类；P2 阶段固定走飞书群自定义机器人（只发纯文本，无回调，见 design.md R5.7）。
	defaultFaultNotifyFallbackChannel = "lark_webhook"
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

	// IncidentNudgeInterval 是 incident_recovery_nudge 定时器的轮询周期；<=0 表示关闭。
	IncidentNudgeInterval time.Duration

	SMTP             SMTPConfig      // R3 email 渠道凭据
	PersonnelChannel string          // R3 人员到人通知的默认渠道，默认 "email"
	Retention        RetentionConfig // R5 timers/notification_log 数据保留策略

	// FlapWindowSeconds/FlapThreshold 是 flapping 防抖（aiops-foundation R4）的判定窗口
	// 与翻转次数阈值；FlapWindowSeconds<=0 表示关闭（opt-in，默认关闭）。
	FlapWindowSeconds int
	FlapThreshold     int

	CCloud CCloudConfig // R1/R2 ccloud 拓扑与健康度接入配置

	FaultNotify FaultNotifyConfig // R5 Fault 级通知的冷却与兜底渠道配置

	// WebBaseURL 非空时用于拼出通知正文里的 Fault 详情链接（{WebBaseURL}/faults/{id}）；
	// 为空表示尚未部署可访问的 Web 前端域名，通知不带链接（不是错误，属预期降级）。
	WebBaseURL string
}

// FaultNotifyConfig 是 R5 Fault 级通知的判定与兜底配置（app.FaultOutbox 消费）。
type FaultNotifyConfig struct {
	Cooldown        time.Duration // 冷却窗口：同 correlation_key 在窗口内刚 resolved 不重发（R5.3）
	FallbackChannel string        // owner team 未配置渠道时的兜底渠道种类
	FallbackTarget  string        // 兜底渠道地址；飞书机器人 hook URL 本身是凭据，禁止落日志
}

// String 实现 fmt.Stringer：FallbackTarget 可能是飞书机器人 hook URL（凭据本身，
// 不变量 13/安全约束禁止落日志），脱敏原则与 SMTPConfig.String 一致；Cooldown/
// FallbackChannel 非敏感，原样打印。
func (c FaultNotifyConfig) String() string {
	return fmt.Sprintf("FaultNotifyConfig{Cooldown:%s, FallbackChannel:%q, FallbackTarget:%s}",
		c.Cooldown, c.FallbackChannel, maskSecret(c.FallbackTarget))
}

// CCloudConfig 是 ccloud 平台拓扑/健康度接入的配置（不变量 23：外部 IO 只走独立周期
// loop）。SyncInterval<=0 表示整条链路关闭，此时其余字段即使缺失也不校验，也不会
// 产生任何 IO——这是让接入可整体开关的唯一口径（R2.1）。
//
// TokenEnv 只存放"读取 token 的环境变量名"，不直接存 token 值本身：token 明文只在
// 进程环境变量里，配置结构体、日志与 String() 输出永远看不到它（对齐现有 DSN 密码
// 掩码与 LarkConfig.String 的口径）。
type CCloudConfig struct {
	BaseURL      string        // ccloud API 基础地址
	TokenEnv     string        // 存放 token 的环境变量名（非 token 本身）
	ProjectIDs   []string      // 需要拉取全链路拓扑的项目 ID 列表
	SyncInterval time.Duration // 独立周期 loop 的拉取间隔；<=0 关闭
	// Environment 是导入实体的环境标识（如 production）。平台拓扑响应里没有环境字段，
	// 只能由部署方显式声明；不得用项目 ID 代替——项目 ID 不是环境，且服务目录的
	// 依赖校验要求双方 environment 一致，取错会让业务服务无法依赖导入的节点。
	Environment string
	// Path* 覆盖 client 占位路径（D2）。空则使用 internal/ccloud 的 DefaultPath*。
	PathFullLink string
	PathService  string
	PathHealth   string
}

// String 实现 fmt.Stringer：TokenEnv 只是环境变量名，不是凭据本身，可以原样打印，
// 便于排查"到底读的是哪个环境变量"；BaseURL/ProjectIDs 均非敏感信息。
func (c CCloudConfig) String() string {
	return fmt.Sprintf("CCloudConfig{BaseURL:%q, TokenEnv:%q, ProjectIDs:%v, Environment:%q, SyncInterval:%s, PathFullLink:%q, PathService:%q, PathHealth:%q}",
		c.BaseURL, c.TokenEnv, c.ProjectIDs, c.Environment, c.SyncInterval, c.PathFullLink, c.PathService, c.PathHealth)
}

// Enabled 以 BaseURL 是否配置作为启用开关：BaseURL 为空视为未接入 ccloud，
// 与 SyncInterval 默认值无关，保证现有未配置 ccloud 的部署零影响；SyncInterval<=0
// 是显式关闭，即使配置了 BaseURL 也不产生任何外部 IO（R2.1）。
func (c CCloudConfig) Enabled() bool {
	return strings.TrimSpace(c.BaseURL) != "" && c.SyncInterval > 0
}

// Validate 只在已启用（Enabled）时校验其余必填项，由 config.Load 在启动期调用：
// 配了 BaseURL 却缺 ProjectIDs 属于配置错误，必须启动即失败，而不是等到周期 loop
// 第一轮才暴露。错误信息只提示缺哪个配置项，绝不拼接 TokenEnv 指向的 token 值。
func (c CCloudConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if len(c.ProjectIDs) == 0 {
		return fmt.Errorf("ccloud.project_ids 不能为空（已配置 ccloud.base_url 且 ccloud.sync_interval > 0）")
	}
	if strings.TrimSpace(c.Environment) == "" {
		return fmt.Errorf("ccloud.environment 不能为空（已配置 ccloud.base_url 且 ccloud.sync_interval > 0）")
	}
	return nil
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

// Validate 拒绝会让清理任务失控的取值。NotificationLogDays<1 会立即删光通知日志，
// 而 notification_log 承载发送幂等键，删掉后 at-least-once 重投会变成重复发送；
// SweepInterval 过小会让 handler 自续期空转；BatchSize<1 时 LIMIT 0 永远"删满一批"，
// 触发每分钟一次的追赶续期。
func (c RetentionConfig) Validate() error {
	switch {
	case c.TimersDays < 1:
		return fmt.Errorf("retention.timers_days 必须 >= 1，当前 %d", c.TimersDays)
	case c.NotificationLogDays < 1:
		return fmt.Errorf("retention.notification_log_days 必须 >= 1，当前 %d", c.NotificationLogDays)
	case c.SweepInterval < time.Minute:
		return fmt.Errorf("retention.sweep_interval 必须 >= 1m，当前 %s", c.SweepInterval)
	case c.BatchSize < 1:
		return fmt.Errorf("retention.batch_size 必须 >= 1，当前 %d", c.BatchSize)
	}
	return nil
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

	// IncidentNudgeInterval 与 RepeatInterval 同一套字符串 Duration 风格；空字符串
	// 不出现在 YAML 里时用下方 defaultIncidentNudgeInterval 兜底。
	IncidentNudgeInterval string `yaml:"incident_nudge_interval"`

	SMTP             fileSMTPConfig      `yaml:"smtp"`
	PersonnelChannel string              `yaml:"personnel_channel"`
	Retention        fileRetentionConfig `yaml:"retention"`

	FlapWindowSeconds int `yaml:"flap_window_seconds"`
	FlapThreshold     int `yaml:"flap_threshold"`

	CCloud fileCCloudConfig `yaml:"ccloud"`

	FaultNotify fileFaultNotifyConfig `yaml:"fault_notify"`
	WebBaseURL  string                `yaml:"web_base_url"`
}

type fileFaultNotifyConfig struct {
	Cooldown        string `yaml:"cooldown"`
	FallbackChannel string `yaml:"fallback_channel"`
	FallbackTarget  string `yaml:"fallback_target"`
}

type fileCCloudConfig struct {
	BaseURL      string   `yaml:"base_url"`
	TokenEnv     string   `yaml:"token_env"`
	ProjectIDs   []string `yaml:"project_ids"`
	SyncInterval string   `yaml:"sync_interval"`
	PathFullLink string   `yaml:"path_fulllink"`
	PathService  string   `yaml:"path_service"`
	PathHealth   string   `yaml:"path_health"`
	Environment  string   `yaml:"environment"`
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
		IncidentNudgeInterval: defaultIncidentNudgeInterval.String(),
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
		CCloud: fileCCloudConfig{
			SyncInterval: defaultCCloudSyncInterval.String(),
		},
		FaultNotify: fileFaultNotifyConfig{
			Cooldown:        defaultFaultNotifyCooldown.String(),
			FallbackChannel: defaultFaultNotifyFallbackChannel,
		},
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
	incidentNudgeInterval, err := time.ParseDuration(fc.IncidentNudgeInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid incident_nudge_interval %q: %w", fc.IncidentNudgeInterval, err)
	}
	retentionSweepInterval, err := time.ParseDuration(fc.Retention.SweepInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid retention.sweep_interval %q: %w", fc.Retention.SweepInterval, err)
	}
	corsAllowedOrigins, err := normalizeOrigins(fc.CORSAllowedOrigins)
	if err != nil {
		return Config{}, err
	}
	ccloudSyncInterval, err := time.ParseDuration(fc.CCloud.SyncInterval)
	if err != nil {
		return Config{}, fmt.Errorf("invalid ccloud.sync_interval %q: %w", fc.CCloud.SyncInterval, err)
	}
	faultNotifyCooldown, err := time.ParseDuration(fc.FaultNotify.Cooldown)
	if err != nil {
		return Config{}, fmt.Errorf("invalid fault_notify.cooldown %q: %w", fc.FaultNotify.Cooldown, err)
	}

	retention := RetentionConfig{
		TimersDays:          fc.Retention.TimersDays,
		NotificationLogDays: fc.Retention.NotificationLogDays,
		SweepInterval:       retentionSweepInterval,
		BatchSize:           fc.Retention.BatchSize,
	}
	if err := retention.Validate(); err != nil {
		return Config{}, err
	}

	ccloud := CCloudConfig{
		BaseURL:      fc.CCloud.BaseURL,
		TokenEnv:     fc.CCloud.TokenEnv,
		ProjectIDs:   trimNonEmpty(fc.CCloud.ProjectIDs),
		SyncInterval: ccloudSyncInterval,
		PathFullLink: strings.TrimSpace(fc.CCloud.PathFullLink),
		PathService:  strings.TrimSpace(fc.CCloud.PathService),
		PathHealth:   strings.TrimSpace(fc.CCloud.PathHealth),
		Environment:  strings.TrimSpace(fc.CCloud.Environment),
	}
	if err := ccloud.Validate(); err != nil {
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
		SessionTTL:            sessionTTL,
		SessionIdleTTL:        sessionIdleTTL,
		SessionCookieSecure:   fc.SessionCookieSecure,
		CORSAllowedOrigins:    corsAllowedOrigins,
		RepeatInterval:        repeatInterval,
		IncidentNudgeInterval: incidentNudgeInterval,
		SMTP: SMTPConfig{
			Host:     fc.SMTP.Host,
			Port:     fc.SMTP.Port,
			Username: fc.SMTP.Username,
			Password: fc.SMTP.Password,
			From:     fc.SMTP.From,
			StartTLS: fc.SMTP.StartTLS,
		},
		PersonnelChannel:  fc.PersonnelChannel,
		Retention:         retention,
		FlapWindowSeconds: fc.FlapWindowSeconds,
		FlapThreshold:     fc.FlapThreshold,
		CCloud:            ccloud,
		FaultNotify: FaultNotifyConfig{
			Cooldown:        faultNotifyCooldown,
			FallbackChannel: fc.FaultNotify.FallbackChannel,
			FallbackTarget:  fc.FaultNotify.FallbackTarget,
		},
		WebBaseURL: fc.WebBaseURL,
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
	if v := os.Getenv("INCIDENT_NUDGE_INTERVAL"); v != "" {
		fc.IncidentNudgeInterval = v
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
	if v := os.Getenv("CCLOUD_BASE_URL"); v != "" {
		fc.CCloud.BaseURL = v
	}
	if v := os.Getenv("CCLOUD_TOKEN_ENV"); v != "" {
		fc.CCloud.TokenEnv = v
	}
	if v := os.Getenv("CCLOUD_PROJECT_IDS"); v != "" {
		fc.CCloud.ProjectIDs = strings.Split(v, ",")
	}
	if v := os.Getenv("CCLOUD_SYNC_INTERVAL"); v != "" {
		fc.CCloud.SyncInterval = v
	}
	if v := os.Getenv("CCLOUD_PATH_FULLLINK"); v != "" {
		fc.CCloud.PathFullLink = v
	}
	if v := os.Getenv("CCLOUD_PATH_SERVICE"); v != "" {
		fc.CCloud.PathService = v
	}
	if v := os.Getenv("CCLOUD_PATH_HEALTH"); v != "" {
		fc.CCloud.PathHealth = v
	}
	if v := os.Getenv("CCLOUD_ENVIRONMENT"); v != "" {
		fc.CCloud.Environment = v
	}
	if v := os.Getenv("FAULT_NOTIFY_COOLDOWN"); v != "" {
		fc.FaultNotify.Cooldown = v
	}
	if v := os.Getenv("FAULT_NOTIFY_FALLBACK_CHANNEL"); v != "" {
		fc.FaultNotify.FallbackChannel = v
	}
	if v := os.Getenv("FAULT_NOTIFY_FALLBACK_TARGET"); v != "" {
		fc.FaultNotify.FallbackTarget = v
	}
	if v := os.Getenv("WEB_BASE_URL"); v != "" {
		fc.WebBaseURL = v
	}
}

// trimNonEmpty 去除每一项两端空白并丢弃空字符串，用于 CCLOUD_PROJECT_IDS 这类
// 逗号分隔的列表配置，避免 "a,,b" 或尾随逗号产生空 ID。
func trimNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
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
