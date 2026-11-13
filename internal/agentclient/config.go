// Package agentclient runs the role-scoped analysis consumer.
package agentclient

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxBatch = 8

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Config struct {
	Server        string        `yaml:"server"`
	ClientID      string        `yaml:"client_id"`
	TokenEnv      string        `yaml:"token_env"`
	Concurrency   int           `yaml:"concurrency"`
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
	ResultDir     string        `yaml:"result_dir"`
	Handlers      []Handler     `yaml:"handlers"`
}

type Handler struct {
	Match struct {
		EventType string `yaml:"event_type"`
		Severity  string `yaml:"severity"`
	} `yaml:"match"`
	Exec    []string      `yaml:"exec"`
	Timeout time.Duration `yaml:"timeout"`
}

func LoadConfig(filename string) (Config, error) {
	f, err := os.Open(filename)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	cfg := Config{TokenEnv: "AGENT_TOKEN", Concurrency: 1, ShutdownGrace: 30 * time.Second}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid client YAML; check field names and types")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("expected one YAML document")
	}
	return cfg, cfg.validate()
}

func (cfg Config) validate() error {
	u, err := url.Parse(cfg.Server)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("server must be an HTTPS origin")
	}
	if strings.TrimSpace(cfg.ClientID) == "" || len(cfg.ClientID) > 128 || !envName.MatchString(cfg.TokenEnv) {
		return errors.New("invalid client_id or token_env")
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 128 {
		return errors.New("concurrency must be 1..128")
	}
	if cfg.ShutdownGrace <= 0 || cfg.ShutdownGrace > time.Hour {
		return errors.New("shutdown_grace must be positive and at most 1h")
	}
	if len(cfg.Handlers) == 0 {
		return errors.New("at least one handler is required")
	}
	for i, h := range cfg.Handlers {
		if len(h.Exec) == 0 || !filepath.IsAbs(h.Exec[0]) || h.Timeout <= 0 || h.Timeout > 24*time.Hour {
			return fmt.Errorf("handler %d requires an absolute executable path and a timeout of up to 24h", i+1)
		}
		if _, err := exec.LookPath(h.Exec[0]); err != nil {
			return fmt.Errorf("handler %d executable is unavailable", i+1)
		}
		for _, arg := range h.Exec {
			if strings.ContainsRune(arg, 0) {
				return fmt.Errorf("handler %d contains NUL in argv", i+1)
			}
		}
		if h.Match.EventType == "" {
			return fmt.Errorf("handler %d requires match.event_type", i+1)
		}
		if _, err := path.Match(h.Match.EventType, ""); err != nil {
			return fmt.Errorf("handler %d contains an invalid event_type glob", i+1)
		}
	}
	if cfg.ResultDir != "" {
		info, err := os.Lstat(cfg.ResultDir)
		if err != nil || !filepath.IsAbs(cfg.ResultDir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("result_dir must be an existing private directory with an absolute path and no group/other permissions")
		}
	}
	return nil
}

func (cfg Config) match(eventType, severity string) *Handler {
	for i := range cfg.Handlers {
		h := &cfg.Handlers[i]
		ok, _ := path.Match(h.Match.EventType, eventType)
		if ok && (h.Match.Severity == "" || h.Match.Severity == severity) {
			return h
		}
	}
	return nil
}
