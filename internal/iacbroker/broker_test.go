package iacbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeCLI(t *testing.T, body string) *Broker {
	t.Helper()
	path := filepath.Join(t.TempDir(), "iacctl-go")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Binary: path, SourceRevision: "test-revision", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestCollectRejectsEntireInvalidRequestBeforeExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	b := fakeCLI(t, fmt.Sprintf("touch '%s'\nprintf '[]'\n", marker))
	for _, ids := range [][]string{{}, {"traffic", "traffic"}, {"traffic", "; rm -rf /"}, {"inspect"}, {"sources", "traffic", "errors", "freshness", "latency", "audit_dead", "audit_lag"}} {
		if _, err := b.Collect(context.Background(), ids); err == nil {
			t.Fatalf("accepted IDs: %q", ids)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid request executed a command")
	}
}
func TestQueryFixedArgsScopeEnvironmentAndSecretFiltering(t *testing.T) {
	t.Setenv("IACCTL_GO_CONFIG", "/secret/config")
	t.Setenv("OMA_API_KEY", "oma-test-secret")
	t.Setenv("OBS_AUTH", "user:observation-secret")
	now := time.Now().Unix()
	b := fakeCLI(t, fmt.Sprintf(`
[ "$1" = query ] && [ "$2" = prom ] && [ "$3" = --source ] && [ "$4" = thanos ] && [ "$5" = --expr ] || exit 9
[ -z "$IACCTL_GO_CONFIG" ] && [ -z "$OMA_API_KEY" ] && [ -z "$OBS_AUTH" ] || exit 9
printf '[{"Metric":{"secret":"observation-secret","url":"https://credential@host/?token=secret"},"Values":[{"T":%d,"V":0}]}]'
`, now))
	item, err := b.Query(context.Background(), "traffic")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != "ok" || item.Scope.AppID != AppID || item.Scope.Environment != "pro" || item.Provenance.CLIHash == "" {
		t.Fatalf("bad evidence %+v", item)
	}
	raw, _ := json.Marshal(item)
	for _, secret := range []string{"observation-secret", "credential@host", "token=secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("untrusted labels leaked")
		}
	}
	if !strings.Contains(item.Provenance.Query, `cluster="prd60"`) || !strings.Contains(item.Provenance.Query, AppID) {
		t.Fatal("query lost fixed scope")
	}
}
func TestNoDataFailedOutputAndStaleStayUnknown(t *testing.T) {
	for _, tt := range []struct{ body, reason string }{
		{"printf '[]'", "no_fresh_samples"},
		{"printf '[{\"Values\":[{\"T\":1,\"V\":0}]}]'", "no_fresh_samples"},
		{"printf 'oops secret' >&2; exit 2", "cli_failed"},
		{"printf '{bad json'", "invalid_json"},
		{"printf '[{\"Values\":[]}]'", "no_fresh_samples"},
	} {
		b := fakeCLI(t, tt.body)
		item, err := b.Query(context.Background(), "traffic")
		if err != nil || item.Status != "unknown" || item.ReasonCode != tt.reason {
			t.Fatalf("got %+v %v", item, err)
		}
	}
	b := fakeCLI(t, "printf '[]'")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	item, err := b.Query(ctx, "traffic")
	if err != nil || item.Status != "unknown" || item.ReasonCode != "cancelled" {
		t.Fatal("cancel not unknown")
	}
}
func TestTimeoutOutputLimitAndGateCancellation(t *testing.T) {
	b := fakeCLI(t, "while :; do :; done")
	started := time.Now()
	item, err := b.Query(context.Background(), "traffic")
	if err != nil || item.ReasonCode != "deadline_or_cancelled" || time.Since(started) > 3*time.Second {
		t.Fatalf("timeout: %+v %v", item, err)
	}
	b = fakeCLI(t, "printf '[]'")
	b.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	item, err = b.Query(ctx, "sources")
	<-b.gate
	if err != nil || item.ReasonCode != "cancelled" {
		t.Fatal("waiting execution slot ignored cancellation")
	}
	w := &boundedWriter{limit: 5}
	_, err = w.Write([]byte("123456"))
	if err == nil || !w.exceeded || w.buf.Len() != 5 {
		t.Fatal("output bound broken")
	}
}
func TestSourcesProbeSanitizesErrorsAndNeverConfusesConnectivityWithHealth(t *testing.T) {
	data, status, _ := parseSources([]byte(`[{"Name":"thanos","Auth":"none","Probe":"ok (5ms)"},{"Name":"obs","Auth":"missing OBS_AUTH","Probe":"error: user:secret https://bad"}]`))
	if status != "unknown" || strings.Contains(string(data), "user:secret") || strings.Contains(string(data), "https://bad") {
		t.Fatal("failed probe leaked or marked ok")
	}
	for _, id := range QueryIDs() {
		if _, exists := catalog[id]; !exists {
			t.Fatal("catalog mismatch")
		}
	}
	for _, rule := range []string{"iac-go-system-errors", "iac-go-latency-p99", "iac-go-traffic-zero", "iac-go-metrics-stale", "iac-go-commit-unknown", "iac-go-outbox-dead", "iac-go-outbox-lag"} {
		if len(Profile(rule)) == 0 || len(Profile(rule)) > MaxQueries {
			t.Fatal("invalid profile")
		}
	}
	if len(Profile("untrusted")) != 0 {
		t.Fatal("unknown rule broadened scope")
	}
}

func TestSharedRulesHaveExplicitCoverageGapAndMissingCredentialDoesNotBlockThanos(t *testing.T) {
	t.Setenv("OBS_AUTH", "")
	b := fakeCLI(t, fmt.Sprintf("printf '[{\"Values\":[{\"T\":%d,\"V\":1}]}]'", time.Now().Unix()))
	item, err := b.Query(context.Background(), "traffic")
	if err != nil || item.Status != "ok" {
		t.Fatal("missing OBS credential blocked independent thanos")
	}
	for _, rule := range []string{"iac-api-probe-not-all-200", "iac-api-probe-delay-high", "iac-redis002-vip-ras-down", "iac-sre-oma-canary"} {
		if len(Profile(rule)) == 0 || len(Profile(rule)) > MaxQueries {
			t.Fatal("shared/canary profile missing")
		}
	}
	for _, id := range []string{"entry_coverage", "vip_coverage"} {
		e, err := b.Query(context.Background(), id)
		if err != nil || e.Status != "unknown" || e.ReasonCode != "shared_probe_not_collected" {
			t.Fatal("Go evidence misrepresented shared probe coverage")
		}
	}
}
