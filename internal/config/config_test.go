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
