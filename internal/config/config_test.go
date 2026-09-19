package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPPort != defaultHTTPPort {
		t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, defaultHTTPPort)
	}
	if cfg.DBDSN != defaultDBDSN {
		t.Errorf("DBDSN = %q, want %q", cfg.DBDSN, defaultDBDSN)
	}
	if cfg.SchedulerPollInterval != defaultPollInterval {
		t.Errorf("SchedulerPollInterval = %v, want %v", cfg.SchedulerPollInterval, defaultPollInterval)
	}
}

func TestLoad_YAMLOverridesDefaults(t *testing.T) {
	path := writeTempYAML(t, `
http_port: 9090
db_dsn: "postgres://yaml-host:5432/db"
scheduler_poll_interval: "10s"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPPort != 9090 {
		t.Errorf("HTTPPort = %d, want 9090", cfg.HTTPPort)
	}
	if cfg.DBDSN != "postgres://yaml-host:5432/db" {
		t.Errorf("DBDSN = %q, want yaml value", cfg.DBDSN)
	}
	if cfg.SchedulerPollInterval != 10*time.Second {
		t.Errorf("SchedulerPollInterval = %v, want 10s", cfg.SchedulerPollInterval)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	path := writeTempYAML(t, `
http_port: 9090
db_dsn: "postgres://yaml-host:5432/db"
scheduler_poll_interval: "10s"
`)

	t.Setenv("HTTP_PORT", "7070")
	t.Setenv("DB_DSN", "postgres://env-host:5432/db")
	t.Setenv("SCHEDULER_POLL_INTERVAL", "30s")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPPort != 7070 {
		t.Errorf("HTTPPort = %d, want 7070 (env should win over yaml)", cfg.HTTPPort)
	}
	if cfg.DBDSN != "postgres://env-host:5432/db" {
		t.Errorf("DBDSN = %q, want env value (env should win over yaml)", cfg.DBDSN)
	}
	if cfg.SchedulerPollInterval != 30*time.Second {
		t.Errorf("SchedulerPollInterval = %v, want 30s (env should win over yaml)", cfg.SchedulerPollInterval)
	}
}

func TestLoad_CORSAllowedOriginsFromEnv(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "http://localhost:5173, https://web.example.com")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"http://localhost:5173", "https://web.example.com"}
	if len(cfg.CORSAllowedOrigins) != len(want) {
		t.Fatalf("CORSAllowedOrigins = %#v, want %#v", cfg.CORSAllowedOrigins, want)
	}
	for i := range want {
		if cfg.CORSAllowedOrigins[i] != want[i] {
			t.Fatalf("CORSAllowedOrigins[%d] = %q, want %q", i, cfg.CORSAllowedOrigins[i], want[i])
		}
	}
}

func TestLoad_RejectsUnsafeCORSOrigins(t *testing.T) {
	for _, value := range []string{"*", "web.example.com", "https://web.example.com/path"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CORS_ALLOWED_ORIGINS", value)
			if _, err := Load(""); err == nil {
				t.Fatalf("Load() error = nil for %q", value)
			}
		})
	}
}

func TestLoad_MissingFileFallsBackToDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPPort != defaultHTTPPort {
		t.Errorf("HTTPPort = %d, want default %d", cfg.HTTPPort, defaultHTTPPort)
	}
}

func TestLoad_InvalidPollInterval(t *testing.T) {
	path := writeTempYAML(t, `scheduler_poll_interval: "not-a-duration"`)

	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want error for invalid scheduler_poll_interval")
	}
}

func TestLoad_LarkDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Lark != (LarkConfig{}) {
		t.Errorf("Lark = %+v, want 零值", cfg.Lark)
	}
}

func TestLoad_LarkYAMLOverridesDefaults(t *testing.T) {
	path := writeTempYAML(t, `
lark:
  app_id: "cli_yaml"
  app_secret: "yaml-secret"
  verification_token: "yaml-token"
  encrypt_key: "yaml-encrypt-key"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := LarkConfig{
		AppID:             "cli_yaml",
		AppSecret:         "yaml-secret",
		VerificationToken: "yaml-token",
		EncryptKey:        "yaml-encrypt-key",
	}
	if cfg.Lark != want {
		t.Errorf("Lark = %+v, want %+v", cfg.Lark, want)
	}
}

func TestLoad_LarkEnvOverridesYAML(t *testing.T) {
	path := writeTempYAML(t, `
lark:
  app_id: "cli_yaml"
  app_secret: "yaml-secret"
  verification_token: "yaml-token"
  encrypt_key: "yaml-encrypt-key"
`)

	t.Setenv("LARK_APP_ID", "cli_env")
	t.Setenv("LARK_APP_SECRET", "env-secret")
	t.Setenv("LARK_VERIFICATION_TOKEN", "env-token")
	t.Setenv("LARK_ENCRYPT_KEY", "env-encrypt-key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := LarkConfig{
		AppID:             "cli_env",
		AppSecret:         "env-secret",
		VerificationToken: "env-token",
		EncryptKey:        "env-encrypt-key",
	}
	if cfg.Lark != want {
		t.Errorf("Lark = %+v, want %+v (env should win over yaml)", cfg.Lark, want)
	}
}

// LarkConfig.String() 是硬约束的关键防线：Sender/日志一旦不小心用 %v/%s 打印
// 整个 LarkConfig，绝不能把凭据原文带出去。
func TestLarkConfig_String_MasksSecrets(t *testing.T) {
	lc := LarkConfig{
		AppID:             "cli_visible",
		AppSecret:         "super-secret-value",
		VerificationToken: "verify-token-value",
		EncryptKey:        "encrypt-key-value",
	}
	s := lc.String()

	if !strings.Contains(s, "cli_visible") {
		t.Errorf("String() = %q, 应保留非敏感的 AppID", s)
	}
	for _, secret := range []string{"super-secret-value", "verify-token-value", "encrypt-key-value"} {
		if strings.Contains(s, secret) {
			t.Errorf("String() = %q, 泄露了凭据原文 %q", s, secret)
		}
	}
	if !strings.Contains(s, "****") {
		t.Errorf("String() = %q, 期望包含掩码占位符 ****", s)
	}
}

func TestLarkConfig_String_EmptySecretsNotMaskedAsPresent(t *testing.T) {
	s := LarkConfig{AppID: "cli_x"}.String()
	if strings.Contains(s, "****") {
		t.Errorf("String() = %q, 空凭据不应显示为已配置（****）", s)
	}
}

func TestLoad_SMTPAndRetentionDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wantSMTP := SMTPConfig{StartTLS: defaultSMTPStartTLS}
	if cfg.SMTP != wantSMTP {
		t.Errorf("SMTP = %+v, want %+v", cfg.SMTP, wantSMTP)
	}
	if cfg.PersonnelChannel != defaultPersonnelChannel {
		t.Errorf("PersonnelChannel = %q, want %q", cfg.PersonnelChannel, defaultPersonnelChannel)
	}
	wantRetention := RetentionConfig{
		TimersDays:          defaultRetentionTimersDays,
		NotificationLogDays: defaultRetentionNotificationLogDays,
		SweepInterval:       defaultRetentionSweepInterval,
		BatchSize:           defaultRetentionBatchSize,
	}
	if cfg.Retention != wantRetention {
		t.Errorf("Retention = %+v, want %+v", cfg.Retention, wantRetention)
	}
}

func TestLoad_SMTPAndRetentionEnvOverrides(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("SMTP_USERNAME", "alert-bot")
	t.Setenv("SMTP_PASSWORD", "s3cr3t")
	t.Setenv("SMTP_FROM", "alert-bot@example.com")
	t.Setenv("SMTP_STARTTLS", "false")
	t.Setenv("PERSONNEL_CHANNEL", "lark")
	t.Setenv("RETENTION_TIMERS_DAYS", "3")
	t.Setenv("RETENTION_NOTIFICATION_LOG_DAYS", "30")
	t.Setenv("RETENTION_SWEEP_INTERVAL", "10m")
	t.Setenv("RETENTION_BATCH_SIZE", "500")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantSMTP := SMTPConfig{
		Host:     "smtp.example.com",
		Port:     587,
		Username: "alert-bot",
		Password: "s3cr3t",
		From:     "alert-bot@example.com",
		StartTLS: false,
	}
	if cfg.SMTP != wantSMTP {
		t.Errorf("SMTP = %+v, want %+v", cfg.SMTP, wantSMTP)
	}
	if cfg.PersonnelChannel != "lark" {
		t.Errorf("PersonnelChannel = %q, want %q", cfg.PersonnelChannel, "lark")
	}
	wantRetention := RetentionConfig{
		TimersDays:          3,
		NotificationLogDays: 30,
		SweepInterval:       10 * time.Minute,
		BatchSize:           500,
	}
	if cfg.Retention != wantRetention {
		t.Errorf("Retention = %+v, want %+v", cfg.Retention, wantRetention)
	}
}

// SMTPConfig.String() 是硬约束的关键防线：Password 一旦被 %v/%s 打印整个结构体，
// 绝不能把密码原文带出去（与 LarkConfig.String 同一防线）。
func TestSMTPConfig_String_MasksPassword(t *testing.T) {
	s := SMTPConfig{
		Host:     "smtp.example.com",
		Port:     587,
		Username: "alert-bot",
		Password: "super-secret-password",
		From:     "alert-bot@example.com",
		StartTLS: true,
	}.String()

	if strings.Contains(s, "super-secret-password") {
		t.Errorf("String() = %q, 泄露了密码原文", s)
	}
	if !strings.Contains(s, "****") {
		t.Errorf("String() = %q, 期望包含掩码占位符 ****", s)
	}
	for _, visible := range []string{"smtp.example.com", "alert-bot", "alert-bot@example.com"} {
		if !strings.Contains(s, visible) {
			t.Errorf("String() = %q, 应保留非敏感字段 %q", s, visible)
		}
	}
}

func TestSMTPConfig_String_EmptyPasswordNotMaskedAsPresent(t *testing.T) {
	s := SMTPConfig{Host: "smtp.example.com"}.String()
	if strings.Contains(s, "****") {
		t.Errorf("String() = %q, 空密码不应显示为已配置（****）", s)
	}
}

func TestLoad_FlapDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FlapWindowSeconds != defaultFlapWindowSeconds {
		t.Errorf("FlapWindowSeconds = %d, want %d（默认关闭）", cfg.FlapWindowSeconds, defaultFlapWindowSeconds)
	}
	if cfg.FlapThreshold != defaultFlapThreshold {
		t.Errorf("FlapThreshold = %d, want %d", cfg.FlapThreshold, defaultFlapThreshold)
	}
}

func TestLoad_FlapEnvOverrides(t *testing.T) {
	t.Setenv("FLAP_WINDOW_SECONDS", "600")
	t.Setenv("FLAP_THRESHOLD", "5")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FlapWindowSeconds != 600 {
		t.Errorf("FlapWindowSeconds = %d, want 600", cfg.FlapWindowSeconds)
	}
	if cfg.FlapThreshold != 5 {
		t.Errorf("FlapThreshold = %d, want 5", cfg.FlapThreshold)
	}
}

func TestLoad_RejectsUnsafeRetention(t *testing.T) {
	cases := map[string][2]string{
		"日志保留 0 天会删光幂等记录": {"RETENTION_NOTIFICATION_LOG_DAYS", "0"},
		"timers 保留 0 天":   {"RETENTION_TIMERS_DAYS", "0"},
		"清理间隔 0 会空转":      {"RETENTION_SWEEP_INTERVAL", "0s"},
		"清理间隔小于 1 分钟":     {"RETENTION_SWEEP_INTERVAL", "30s"},
		"批量 0 会反复追赶":      {"RETENTION_BATCH_SIZE", "0"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if _, err := Load(""); err == nil {
				t.Fatalf("Load() error = nil for %s=%s", env[0], env[1])
			}
		})
	}
}

func TestLoad_DefaultRetentionIsValid(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.Retention.Validate(); err != nil {
		t.Fatalf("默认留存配置应合法: %v", err)
	}
}

func TestLoad_CCloudDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CCloud.SyncInterval != defaultCCloudSyncInterval {
		t.Errorf("CCloud.SyncInterval = %v, want %v", cfg.CCloud.SyncInterval, defaultCCloudSyncInterval)
	}
	if cfg.CCloud.BaseURL != "" || cfg.CCloud.TokenEnv != "" || len(cfg.CCloud.ProjectIDs) != 0 {
		t.Errorf("CCloud 默认应全部为空: %+v", cfg.CCloud)
	}
}

// TestLoad_CCloudSyncIntervalZeroShortCircuits 覆盖「0 = 完全关闭且不产生任何 IO」：
// 即使 BaseURL/ProjectIDs 缺失，interval=0 时 Load() 与显式 Validate() 都不应报错。
func TestLoad_CCloudSyncIntervalZeroShortCircuits(t *testing.T) {
	t.Setenv("CCLOUD_SYNC_INTERVAL", "0s")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v, interval=0 应短路校验", err)
	}
	if cfg.CCloud.SyncInterval != 0 {
		t.Errorf("CCloud.SyncInterval = %v, want 0", cfg.CCloud.SyncInterval)
	}
	if err := cfg.CCloud.Validate(); err != nil {
		t.Fatalf("CCloud.Validate() error = %v, interval=0 应短路校验", err)
	}
}

func TestLoad_CCloudEnvOverrides(t *testing.T) {
	t.Setenv("CCLOUD_BASE_URL", "https://ccloud.example.com")
	t.Setenv("CCLOUD_TOKEN_ENV", "CCLOUD_API_TOKEN")
	t.Setenv("CCLOUD_PROJECT_IDS", "proj-a, proj-b,,proj-c ")
	t.Setenv("CCLOUD_SYNC_INTERVAL", "5m")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CCloud.BaseURL != "https://ccloud.example.com" {
		t.Errorf("CCloud.BaseURL = %q", cfg.CCloud.BaseURL)
	}
	if cfg.CCloud.TokenEnv != "CCLOUD_API_TOKEN" {
		t.Errorf("CCloud.TokenEnv = %q", cfg.CCloud.TokenEnv)
	}
	wantIDs := []string{"proj-a", "proj-b", "proj-c"}
	if len(cfg.CCloud.ProjectIDs) != len(wantIDs) {
		t.Fatalf("CCloud.ProjectIDs = %v, want %v", cfg.CCloud.ProjectIDs, wantIDs)
	}
	for i, id := range wantIDs {
		if cfg.CCloud.ProjectIDs[i] != id {
			t.Errorf("CCloud.ProjectIDs[%d] = %q, want %q", i, cfg.CCloud.ProjectIDs[i], id)
		}
	}
	if cfg.CCloud.SyncInterval != 5*time.Minute {
		t.Errorf("CCloud.SyncInterval = %v, want 5m", cfg.CCloud.SyncInterval)
	}
}

// TestLoad_CCloudRequiresBaseURLAndProjectIDsWhenEnabled 覆盖「interval>0 但缺
// BASE_URL/PROJECT_IDS 时启动期报错」——Load() 本身不报错（不影响未配置 ccloud 的
// 现有部署），由装配方在启动期显式调用 CCloud.Validate() 触发。
func TestLoad_CCloudEnableSwitch(t *testing.T) {
	cases := []struct {
		name    string
		envs    map[string]string
		wantErr bool
		enabled bool
	}{
		// 未配置 BASE_URL 即未接入：默认 interval 10m 也不得让现有部署启动失败。
		{"未配 BASE_URL 视为关闭", map[string]string{"CCLOUD_SYNC_INTERVAL": "5m", "CCLOUD_PROJECT_IDS": "proj-a"}, false, false},
		{"默认配置视为关闭", map[string]string{}, false, false},
		// 配了 BASE_URL 却缺 PROJECT_IDS 是配置错误，必须启动期失败。
		{"配 BASE_URL 缺 PROJECT_IDS 启动失败", map[string]string{"CCLOUD_BASE_URL": "https://ccloud.example.com"}, true, false},
		{"interval=0 显式关闭", map[string]string{"CCLOUD_BASE_URL": "https://ccloud.example.com", "CCLOUD_SYNC_INTERVAL": "0s"}, false, false},
		{"完整配置启用", map[string]string{"CCLOUD_BASE_URL": "https://ccloud.example.com", "CCLOUD_PROJECT_IDS": "proj-a"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.envs {
				t.Setenv(k, v)
			}
			cfg, err := Load("")
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load() error = nil, want 启动期报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := cfg.CCloud.Enabled(); got != tc.enabled {
				t.Fatalf("Enabled() = %v, want %v", got, tc.enabled)
			}
		})
	}
}

// TestLoad_CCloudTokenNeverInErrorMessage 覆盖「token 不出现在日志/错误信息中」：
// TOKEN_ENV 只是环境变量名，真正的 token 值即便被设置也绝不能出现在校验失败的
// 错误信息里（这里连 BASE_URL/PROJECT_IDS 一起缺失触发报错，确认 token 明文不泄露）。
func TestLoad_CCloudTokenNeverInErrorMessage(t *testing.T) {
	const secretToken = "super-secret-ccloud-token-value"
	t.Setenv("CCLOUD_BASE_URL", "https://ccloud.example.com")
	t.Setenv("CCLOUD_TOKEN_ENV", "CCLOUD_API_TOKEN")
	t.Setenv("CCLOUD_API_TOKEN", secretToken)

	_, verr := Load("")
	if verr == nil {
		t.Fatalf("Load() error = nil, want 缺 PROJECT_IDS 报错")
	}
	if strings.Contains(verr.Error(), secretToken) {
		t.Fatalf("错误信息泄露了 token 明文: %v", verr)
	}
}

// TestCCloudConfig_String_NeverLeaksToken 覆盖 String() 输出不落 token：TokenEnv
// 只是变量名，即便该环境变量当前被设置为敏感值，CCloudConfig 结构体本身也不持有
// 该值，String() 自然无从输出。
func TestCCloudConfig_String_NeverLeaksToken(t *testing.T) {
	const secretToken = "super-secret-ccloud-token-value"
	t.Setenv("CCLOUD_API_TOKEN", secretToken)

	cfg := CCloudConfig{
		BaseURL:      "https://ccloud.example.com",
		TokenEnv:     "CCLOUD_API_TOKEN",
		ProjectIDs:   []string{"proj-a"},
		SyncInterval: 5 * time.Minute,
	}
	s := cfg.String()
	if strings.Contains(s, secretToken) {
		t.Fatalf("CCloudConfig.String() 泄露了 token 明文: %v", s)
	}
	if !strings.Contains(s, "CCLOUD_API_TOKEN") {
		t.Errorf("CCloudConfig.String() 应包含 TokenEnv 变量名本身: %v", s)
	}
}

// TestLoad_FaultNotifyDefaults 覆盖 R5.2/R5.3 的默认值：冷却 30 分钟，兜底渠道
// lark_webhook，兜底 target 默认为空（未配置视为"无兜底"，由 FaultOutbox 据此
// 决定是否写"无可用通知目标"时间线）。
func TestLoad_FaultNotifyDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FaultNotify.Cooldown != defaultFaultNotifyCooldown {
		t.Errorf("FaultNotify.Cooldown = %v, want %v", cfg.FaultNotify.Cooldown, defaultFaultNotifyCooldown)
	}
	if cfg.FaultNotify.FallbackChannel != defaultFaultNotifyFallbackChannel {
		t.Errorf("FaultNotify.FallbackChannel = %q, want %q", cfg.FaultNotify.FallbackChannel, defaultFaultNotifyFallbackChannel)
	}
	if cfg.FaultNotify.FallbackTarget != "" {
		t.Errorf("FaultNotify.FallbackTarget 默认应为空，got %q", cfg.FaultNotify.FallbackTarget)
	}
	if cfg.WebBaseURL != "" {
		t.Errorf("WebBaseURL 默认应为空，got %q", cfg.WebBaseURL)
	}
}

// TestLoad_FaultNotifyEnvOverrides 覆盖三个环境变量与 WEB_BASE_URL 的覆盖生效。
func TestLoad_FaultNotifyEnvOverrides(t *testing.T) {
	t.Setenv("FAULT_NOTIFY_COOLDOWN", "10m")
	t.Setenv("FAULT_NOTIFY_FALLBACK_CHANNEL", "lark")
	t.Setenv("FAULT_NOTIFY_FALLBACK_TARGET", "chat_id:oc_test")
	t.Setenv("WEB_BASE_URL", "https://sre.example.com")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FaultNotify.Cooldown != 10*time.Minute {
		t.Errorf("FaultNotify.Cooldown = %v, want 10m", cfg.FaultNotify.Cooldown)
	}
	if cfg.FaultNotify.FallbackChannel != "lark" {
		t.Errorf("FaultNotify.FallbackChannel = %q, want lark", cfg.FaultNotify.FallbackChannel)
	}
	if cfg.FaultNotify.FallbackTarget != "chat_id:oc_test" {
		t.Errorf("FaultNotify.FallbackTarget = %q, want chat_id:oc_test", cfg.FaultNotify.FallbackTarget)
	}
	if cfg.WebBaseURL != "https://sre.example.com" {
		t.Errorf("WebBaseURL = %q, want https://sre.example.com", cfg.WebBaseURL)
	}
}

// TestLoad_FaultNotifyInvalidCooldown 覆盖非法 duration 启动即失败（不允许猜测默认值）。
func TestLoad_FaultNotifyInvalidCooldown(t *testing.T) {
	t.Setenv("FAULT_NOTIFY_COOLDOWN", "not-a-duration")
	if _, err := Load(""); err == nil {
		t.Fatal("非法 fault_notify.cooldown 应导致 Load() 报错")
	}
}

// TestFaultNotifyConfig_String_NeverLeaksFallbackTarget 覆盖安全约束：兜底 target
// 可能是飞书机器人 hook URL（凭据本身），String() 输出必须脱敏，不能被 %v/%s
// 打印整个结构体时泄露（不变量 13、安全约束）。
func TestFaultNotifyConfig_String_NeverLeaksFallbackTarget(t *testing.T) {
	const secretHookURL = "https://open.feishu.cn/open-apis/bot/v2/hook/super-secret-token"
	c := FaultNotifyConfig{Cooldown: 30 * time.Minute, FallbackChannel: "lark_webhook", FallbackTarget: secretHookURL}
	s := c.String()
	if strings.Contains(s, secretHookURL) {
		t.Fatalf("FaultNotifyConfig.String() 泄露了兜底 target 明文: %v", s)
	}
	if !strings.Contains(s, "lark_webhook") {
		t.Errorf("FaultNotifyConfig.String() 应保留非敏感的 FallbackChannel: %v", s)
	}
}

// TestFaultNotifyConfig_String_EmptyTargetNotMaskedAsPresent 覆盖空 target 不应
// 被误打印成"已配置但脱敏"，与 SMTPConfig.String 的既有口径一致。
func TestFaultNotifyConfig_String_EmptyTargetNotMaskedAsPresent(t *testing.T) {
	s := FaultNotifyConfig{FallbackChannel: "lark_webhook"}.String()
	if !strings.Contains(s, `FallbackTarget:""`) {
		t.Errorf("空 FallbackTarget 应原样显示为空串，got %v", s)
	}
}
