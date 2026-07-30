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
