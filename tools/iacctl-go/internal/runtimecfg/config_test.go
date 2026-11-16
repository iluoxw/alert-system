package runtimecfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Env != "pro" {
		t.Fatalf("env: %s", cfg.Env)
	}
	if cfg.Thanos.BaseURL != "https://thanos-inner.test.seewo.com" {
		t.Fatalf("thanos: %s", cfg.Thanos.BaseURL)
	}
	if cfg.Obs.AuthEnv != "OBS_AUTH" || cfg.Obs.Org == "" {
		t.Fatalf("obs: %+v", cfg.Obs)
	}
	if p := cfg.Obs.ObsPromPath(); p != "/api/3JimvcGJ34Rtyh6VHSuWVeWcpKP/prometheus" {
		t.Fatalf("obs prom path: %s", p)
	}
	if p := cfg.Thanos.ThanosPromPath(); p != "" {
		t.Fatalf("thanos prom path: %s", p)
	}
}

func TestFileOverrideAndEnvVar(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "iac-go.yaml")
	os.WriteFile(p, []byte(`
env: pro
thanos:
  base_url: https://thanos2.example
obs:
  base_url: https://obs2.example
  org: org42
  auth_env: MY_AUTH
`), 0o644)

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thanos.BaseURL != "https://thanos2.example" {
		t.Fatalf("thanos override: %s", cfg.Thanos.BaseURL)
	}
	if cfg.Obs.Org != "org42" || cfg.Obs.AuthEnv != "MY_AUTH" {
		t.Fatalf("obs override: %+v", cfg.Obs)
	}
	if cfg.Service != "iac-go" {
		t.Fatalf("defaults preserved: %s", cfg.Service)
	}
	if p2 := cfg.Obs.ObsSearchPath(); p2 != "/api/org42/_search" {
		t.Fatalf("search path: %s", p2)
	}

	os.Setenv("IACCTL_GO_CONFIG", p)
	defer os.Unsetenv("IACCTL_GO_CONFIG")
	cfg2, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Obs.Org != "org42" {
		t.Fatalf("env-var config not picked up: %+v", cfg2.Obs)
	}
}

func TestMissingFileFails(t *testing.T) {
	if _, err := Load("/nonexistent/iac-go.yaml"); err == nil {
		t.Fatal("missing file must fail")
	}
}
