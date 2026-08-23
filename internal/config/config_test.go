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

func TestLoad_ServiceObservabilityDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	so := cfg.ServiceObservability
	// profile dir 默认为空：空目录是合法的空 registry，不是启动错误。
	if so.ProfileDir != "" {
		t.Errorf("ProfileDir = %q, want 空（未配置 Profile 目录）", so.ProfileDir)
	}
	if so.CacheTTL != defaultServiceObservabilityCacheTTL {
		t.Errorf("CacheTTL = %v, want %v", so.CacheTTL, defaultServiceObservabilityCacheTTL)
	}
	if so.CacheMaxStale != defaultServiceObservabilityCacheMaxStale {
		t.Errorf("CacheMaxStale = %v, want %v", so.CacheMaxStale, defaultServiceObservabilityCacheMaxStale)
	}
	if so.CacheMaxEntries != defaultServiceObservabilityCacheMaxEntries {
		t.Errorf("CacheMaxEntries = %d, want %d", so.CacheMaxEntries, defaultServiceObservabilityCacheMaxEntries)
	}
	if so.RefreshWorkers != defaultServiceObservabilityRefreshWorkers {
		t.Errorf("RefreshWorkers = %d, want %d", so.RefreshWorkers, defaultServiceObservabilityRefreshWorkers)
	}
	if so.RefreshQueue != defaultServiceObservabilityRefreshQueue {
		t.Errorf("RefreshQueue = %d, want %d", so.RefreshQueue, defaultServiceObservabilityRefreshQueue)
	}
	if so.MaxWindowSeconds != defaultServiceObservabilityMaxWindowSeconds {
		t.Errorf("MaxWindowSeconds = %d, want %d", so.MaxWindowSeconds, defaultServiceObservabilityMaxWindowSeconds)
	}
}

func TestLoad_ServiceObservabilityYAMLAndEnvOverrides(t *testing.T) {
	path := writeTempYAML(t, `
service_observability:
  profile_dir: "/etc/alert-system/profiles"
  cache_ttl: "30s"
  cache_max_stale: "2m"
  cache_max_entries: 128
  refresh_workers: 2
  refresh_queue: 64
  max_window_seconds: 3600
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	so := cfg.ServiceObservability
	if so.ProfileDir != "/etc/alert-system/profiles" || so.CacheTTL != 30*time.Second ||
		so.CacheMaxStale != 2*time.Minute || so.CacheMaxEntries != 128 ||
		so.RefreshWorkers != 2 || so.RefreshQueue != 64 || so.MaxWindowSeconds != 3600 {
		t.Fatalf("YAML 未生效: %+v", so)
	}

	// 环境变量优先级高于 YAML。
	t.Setenv("SERVICE_OBSERVABILITY_PROFILE_DIR", "/srv/profiles")
	t.Setenv("SERVICE_OBSERVABILITY_CACHE_TTL", "10s")
	t.Setenv("SERVICE_OBSERVABILITY_CACHE_MAX_STALE", "90s")
	t.Setenv("SERVICE_OBSERVABILITY_CACHE_MAX_ENTRIES", "256")
	t.Setenv("SERVICE_OBSERVABILITY_REFRESH_WORKERS", "8")
	t.Setenv("SERVICE_OBSERVABILITY_REFRESH_QUEUE", "512")
	t.Setenv("SERVICE_OBSERVABILITY_MAX_WINDOW_SECONDS", "7200")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	so = cfg.ServiceObservability
	if so.ProfileDir != "/srv/profiles" || so.CacheTTL != 10*time.Second ||
		so.CacheMaxStale != 90*time.Second || so.CacheMaxEntries != 256 ||
		so.RefreshWorkers != 8 || so.RefreshQueue != 512 || so.MaxWindowSeconds != 7200 {
		t.Fatalf("环境变量未覆盖 YAML: %+v", so)
	}
}

// 非法值必须让启动失败，而不是静默退回默认值——否则"配置写错"会看起来像"配置生效了"。
func TestLoad_ServiceObservabilityRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"TTL 非正", map[string]string{"SERVICE_OBSERVABILITY_CACHE_TTL": "0s"}, "cache_ttl"},
		{"TTL 无法解析", map[string]string{"SERVICE_OBSERVABILITY_CACHE_TTL": "soon"}, "cache_ttl"},
		{"max stale 小于 TTL", map[string]string{
			"SERVICE_OBSERVABILITY_CACHE_TTL":       "60s",
			"SERVICE_OBSERVABILITY_CACHE_MAX_STALE": "30s",
		}, "cache_max_stale"},
		{"max entries 非正", map[string]string{"SERVICE_OBSERVABILITY_CACHE_MAX_ENTRIES": "0"}, "cache_max_entries"},
		{"max entries 非整数", map[string]string{"SERVICE_OBSERVABILITY_CACHE_MAX_ENTRIES": "many"}, "cache_max_entries"},
		{"worker 非正", map[string]string{"SERVICE_OBSERVABILITY_REFRESH_WORKERS": "-1"}, "refresh_workers"},
		{"queue 非正", map[string]string{"SERVICE_OBSERVABILITY_REFRESH_QUEUE": "0"}, "refresh_queue"},
		{"窗口低于契约下界", map[string]string{"SERVICE_OBSERVABILITY_MAX_WINDOW_SECONDS": "30"}, "max_window_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			_, err := Load("")
			if err == nil {
				t.Fatal("非法配置必须让 Load 失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应指出具体配置项 %q，实际 %v", tc.want, err)
			}
		})
	}
}
