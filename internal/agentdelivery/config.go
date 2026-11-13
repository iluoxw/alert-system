package agentdelivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

type Machine struct {
	ClientID    string `yaml:"client_id"`
	Role        string `yaml:"role"`
	TokenSHA256 string `yaml:"token_sha256"`
}

type ServerConfig struct {
	Routing    Routing   `yaml:"routing"`
	Clients    []Machine `yaml:"clients"`
	principals map[[32]byte]Principal
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func validID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

// LoadServerConfig takes a startup snapshot. An unset path disables the channel.
func LoadServerConfig(path string) (*ServerConfig, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := new(ServerConfig)
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(cfg); err != nil {
		return nil, errors.New("invalid agent delivery YAML")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("agent config must be one YAML document")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.Routing.ConfigVersion = fmt.Sprintf("%x", sha256.Sum256(data))
	return cfg, nil
}

func (c *ServerConfig) validate() error {
	if len(c.Routing.SourceIDs) == 0 || len(c.Clients) == 0 {
		return errors.New("agent config needs source_ids and clients")
	}
	sources := make(map[string]bool)
	for _, id := range c.Routing.SourceIDs {
		if !validID(id) || sources[id] {
			return errors.New("invalid or repeated routing.source_ids")
		}
		sources[id] = true
	}
	c.principals = make(map[[32]byte]Principal)
	clients := map[string]string{}
	for _, m := range c.Clients {
		key, err := hex.DecodeString(m.TokenSHA256)
		if err != nil || len(key) != 32 || !namePattern.MatchString(m.ClientID) || !namePattern.MatchString(m.Role) {
			return errors.New("invalid agent credential binding")
		}
		digest := [32]byte(key)
		if _, exists := c.principals[digest]; exists {
			return errors.New("duplicate agent credential")
		}
		if role, exists := clients[m.ClientID]; exists && role != m.Role {
			return errors.New("one client identity cannot have multiple roles")
		}
		clients[m.ClientID] = m.Role
		c.principals[digest] = Principal{ClientID: m.ClientID, Role: m.Role}
	}
	f := &c.Routing.Fallback
	if f.Source != "" || f.SourceID != "" || f.EventType != "" || f.Severity != "" || len(f.Labels) > 0 {
		return errors.New("fallback cannot have match conditions")
	}
	if f.Name == "" {
		f.Name = "fallback"
	}
	names := map[string]bool{}
	rules := []*Rule{f}
	for i := range c.Routing.Routes {
		rules = append(rules, &c.Routing.Routes[i])
	}
	for _, r := range rules {
		if !namePattern.MatchString(r.Name) || names[r.Name] || !namePattern.MatchString(r.Role) {
			return errors.New("invalid route name, duplicate name, or invalid role")
		}
		names[r.Name] = true
		if r.Source != "" && r.Source != "alertmanager" || r.SourceID != "" && !sources[r.SourceID] {
			return fmt.Errorf("invalid source in route %s", r.Name)
		}
		if r.EventType != "" && r.EventType != "alert.resolved" && r.EventType != "alert.firing" {
			return fmt.Errorf("invalid event_type in route %s", r.Name)
		}
		if r.Severity != "" && r.Severity != "info" && r.Severity != "warning" && r.Severity != "critical" {
			return fmt.Errorf("invalid severity in route %s", r.Name)
		}
		if r.TTL <= 0 || r.TTL > 30*24*time.Hour {
			return fmt.Errorf("invalid TTL in route %s", r.Name)
		}
		if r.MaxAttempts == 0 {
			r.MaxAttempts = 3
		}
		if r.MaxAttempts < 1 || r.MaxAttempts > 100 || r.Priority < 0 || r.Priority > 1000 {
			return fmt.Errorf("invalid limits in route %s", r.Name)
		}
		if r.AckMode != "" && r.AckMode != "on_receive" {
			return fmt.Errorf("unsupported ack_mode in route %s", r.Name)
		}
	}
	return nil
}
