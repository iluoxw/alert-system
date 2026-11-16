// Package runtimecfg loads the runtime configuration: backend endpoints and
// the auth env name. No credential value is ever stored here — the obs client
// reads the environment at construction time.
package runtimecfg

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Backend struct {
	BaseURL string `yaml:"base_url"`
	// Org is the OpenObserve organization (obs only).
	Org string `yaml:"org"`
	// AuthEnv names the environment variable holding user:pass (obs only).
	// Empty means no auth (thanos).
	AuthEnv string `yaml:"auth_env"`
}

type Config struct {
	Service string  `yaml:"service"`
	Env     string  `yaml:"env"`
	Thanos  Backend `yaml:"thanos"`
	Obs     Backend `yaml:"obs"`
}

// Current is the applied configuration; Load+Apply replace it.
var Current = Default()

func Default() Config {
	return Config{
		Service: "iac-go",
		Env:     "pro",
		Thanos:  Backend{BaseURL: "https://thanos-inner.test.seewo.com"},
		Obs: Backend{
			BaseURL: "https://obs.test.seewo.com",
			Org:     "3JimvcGJ34Rtyh6VHSuWVeWcpKP",
			AuthEnv: "OBS_AUTH",
		},
	}
}

// Load reads path (or $IACCTL_GO_CONFIG) over the defaults. An empty path with
// no env variable returns the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = os.Getenv("IACCTL_GO_CONFIG")
	}
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return cfg, nil
}

// Apply installs cfg as Current.
func (c Config) Apply() {
	Current = c
}

// ThanosPromPath is the Prometheus API path prefix on thanos; the client
// appends "/api/v1/query" itself.
func (b Backend) ThanosPromPath() string { return "" }

// ObsPromPath is the Prometheus API path prefix on OpenObserve.
func (b Backend) ObsPromPath() string { return "/api/" + b.Org + "/prometheus" }

// ObsSearchPath is the SQL search path on OpenObserve.
func (b Backend) ObsSearchPath() string { return "/api/" + b.Org + "/_search" }
