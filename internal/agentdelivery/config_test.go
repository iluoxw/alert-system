package agentdelivery

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentConfigValidatesDisabledModeAndRotation(t *testing.T) {
	if c, err := LoadServerConfig(""); c != nil || err != nil {
		t.Fatal("expected default off")
	}
	const sourceID = "00000000-0000-4000-8000-000000000001"
	cfg := func(sub string) string {
		return fmt.Sprintf("routing:\n  source_ids: [%s]\n  fallback: {role: ops, ttl: 1m%s}\nclients:\n  - {client_id: a, role: ops, token_sha256: '%x'}\n  - {client_id: a, role: ops, token_sha256: '%x'}\n", sourceID, sub, sha256.Sum256([]byte("old")), sha256.Sum256([]byte("new")))
	}
	file := filepath.Join(t.TempDir(), "agent.yaml")
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"overlap while rotating", cfg(""), true},
		{"future mode", cfg(", ack_mode: on_complete"), false},
		{"unknown key", cfg(", max_atempts: 3"), false},
		{"empty sources", strings.ReplaceAll(cfg(""), "["+sourceID+"]", "[]"), false},
		{"second document", cfg("") + "---\n{}\n", false},
		{"same machine with conflicting role", strings.Replace(cfg(""), "client_id: a, role: ops", "client_id: a, role: dev", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(file, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadServerConfig(file)
			if tc.valid != (err == nil) {
				t.Fatalf("validation: %v", err)
			}
			if tc.valid && (len(c.principals) != 2 || c.Routing.Fallback.MaxAttempts != 3 || c.Routing.ConfigVersion == "") {
				t.Fatalf("invalid parsed routing or credential bindings")
			}
		})
	}
}

func TestAgentConfigAllowsGrafanaAndValidatesPublicRules(t *testing.T) {
	source := "00000000-0000-4000-8000-000000000001"
	cfg := &ServerConfig{Routing: Routing{SourceIDs: []string{source}, PublicRuleIDs: []string{"iac-go-system-errors"}, Routes: []Rule{{Name: "iac", Source: "grafana", SourceID: source, Role: "iac", TTL: time.Hour}}, Fallback: Rule{Role: "iac", TTL: time.Hour}}, Clients: []Machine{{ClientID: "dispatcher", Role: "iac", TokenSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("local-test")))}}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.PublicRuleIDs = []string{"iac-go-system-errors", "iac-go-system-errors"}
	if cfg.validate() == nil {
		t.Fatal("重复公开规则应拒绝")
	}
}
