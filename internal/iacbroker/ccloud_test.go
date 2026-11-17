package iacbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func ccloudFixture(t *testing.T, body string) *Broker {
	t.Helper()
	b := fakeCLI(t, "exit 99")
	dir := t.TempDir()
	node := filepath.Join(t.TempDir(), "node")
	if e := os.WriteFile(node, []byte("#!/bin/sh\n"+body), 0700); e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{"package.json": []byte(`{"name":"@cvte/ccloud-cli","version":"2.2.1"}`), "dist/ccloud-cli.js": []byte("// fixture")}
	entries := []map[string]string{}
	for name, raw := range files {
		path := filepath.Join(dir, name)
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(raw)
		entries = append(entries, map[string]string{"path": name, "sha256": hex.EncodeToString(sum[:])})
	}
	manifest, _ := json.Marshal(map[string]any{"package": "@cvte/ccloud-cli", "version": "2.2.1", "files": entries})
	sum := sha256.Sum256(manifest)
	if e := os.WriteFile(filepath.Join(dir, "BUNDLE.json"), manifest, 0600); e != nil {
		t.Fatal(e)
	}
	b.ccloud = newCCloud(CCloudOptions{NodeBinary: node, PackageDir: dir, ProxyToken: "test-narrow-token"}, hex.EncodeToString(sum[:]))
	return b
}

const validPods = `{"resource":"app","action":"pods","data":[{"podName":"pod-one","phase":"Running","ready":true,"restartCount":2,"env":{"SECRET":"do-not-leak"},"podIP":"private-address"},{"podName":"pod-two","phase":"Pending","ready":false,"restartCount":1}]}`

func TestCCloudFixedCommandEnvironmentAndIndependentProvenance(t *testing.T) {
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		t.Setenv(key, "test-value")
	}
	for _, key := range []string{"HOME", "NODE_OPTIONS", "OMA_API_KEY", "OBS_AUTH", "IAC_CCLOUD_UPSTREAM_KEY"} {
		t.Setenv(key, "do-not-forward")
	}
	body := `[ "$#" = 7 ] && [ "$2" = app ] && [ "$3" = pods ] && [ "$4" = --app-id ] && [ "$5" = '` + AppID + `' ] && [ "$6" = --cluster-id ] && [ "$7" = 60 ] || exit 10
[ "$CCLOUD_API_KEY" = test-narrow-token ] && [ "$CCLOUD_API_BASE_URL" = '` + ccloudBaseURL + `' ] && [ "$CCLOUD_API_MODE" = real ] && [ "$CCLOUD_DISABLE_UPDATE_CHECK" = 1 ] || exit 11
[ -z "$HOME$NODE_OPTIONS$OMA_API_KEY$OBS_AUTH$IAC_CCLOUD_UPSTREAM_KEY" ] || exit 12
[ "$PATH" = /usr/local/bin:/usr/bin:/bin ] || exit 13
`
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		body += fmt.Sprintf("[ \"$%s\" = test-value ] || exit 14\n", key)
	}
	b := ccloudFixture(t, body+"printf '%s' '"+validPods+"'\n")
	item, e := b.Query(t.Context(), "ccloud_l4")
	if e != nil || item.Status != "ok" {
		t.Fatal("fixed execution rejected", e, item.ReasonCode)
	}
	if item.Provenance.CLIHash == "" || item.Provenance.CLIHash == b.hash || item.Provenance.SourceRevision != ccloudRevision || item.Provenance.PackageManifestSHA256 != b.ccloud.manifestHash {
		t.Fatal("wrong CLI provenance")
	}
	raw, _ := json.Marshal(item)
	if strings.Contains(string(raw), "do-not") || strings.Contains(string(raw), "private-address") || strings.Contains(string(raw), "test-narrow-token") {
		t.Fatal("sensitive data leaked")
	}
	var data struct {
		Count    int `json:"pod_count"`
		Running  int `json:"running"`
		Ready    int `json:"ready_running"`
		Restarts int `json:"restarts_total"`
		Desired  any `json:"desired_replicas"`
	}
	if json.Unmarshal(item.Data, &data) != nil || data.Count != 2 || data.Running != 1 || data.Ready != 1 || data.Restarts != 3 || data.Desired != nil {
		t.Fatal("invalid aggregate")
	}
}
func TestCCloudObservationContractIsStrict(t *testing.T) {
	if _, s, _ := parseCCloud([]byte(validPods)); s != "ok" {
		t.Fatal("official CLI output rejected")
	}
	for _, raw := range []string{`{"code":0,"data":[]}`, `{"resource":"app","action":"other","data":[]}`, `{"resource":"app","action":"pods","code":401,"data":[]}`, `{"resource":"app","action":"pods","code":null,"data":[]}`, `{"resource":"app","action":"pods","data":null}`, `{"resource":"app","action":"pods","data":[{"podName":"x","phase":"Running","ready":true}]}`, strings.Replace(validPods, `"ready":true`, `"ready":"true"`, 1), strings.Replace(validPods, `"restartCount":2`, `"restartCount":-1`, 1), strings.Replace(validPods, `"restartCount":2`, `"restartCount":2.5`, 1), strings.Replace(validPods, `"phase":"Running"`, `"phase":"Garbage"`, 1), strings.Replace(validPods, `"podName":"pod-two"`, `"podName":"pod-one"`, 1)} {
		if _, status, _ := parseCCloud([]byte(raw)); status != "unknown" {
			t.Fatal("invalid observation marked ok")
		}
	}
	if _, s, _ := parseCCloud([]byte(`{"resource":"app","action":"pods","code":0,"data":[]}`)); s != "ok" {
		t.Fatal("explicit code0 denied")
	}
}
func TestCCloudUnavailableTamperedFailureAndBoundsStayUnknown(t *testing.T) {
	if item, e := fakeCLI(t, "exit 0").Query(t.Context(), "ccloud_l4"); e != nil || item.Status != "unknown" || item.ReasonCode != "ccloud_not_configured" {
		t.Fatal("disabled query succeeded")
	}
	for _, kind := range []string{"missing-token", "missing-node", "tamper-entry", "tamper-manifest", "symlink", "extra", "exit", "timeout", "output"} {
		t.Run(kind, func(t *testing.T) {
			body := "printf '%s' '" + validPods + "'"
			switch kind {
			case "exit":
				body += ";exit 2"
			case "timeout":
				body = "while :;do :;done"
			case "output":
				body = "while :;do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx';done"
			}
			b := ccloudFixture(t, body)
			o := &b.ccloud.options
			switch kind {
			case "missing-token":
				o.ProxyToken = ""
			case "missing-node":
				o.NodeBinary = filepath.Join(t.TempDir(), "absent")
			case "tamper-entry":
				_ = os.WriteFile(filepath.Join(o.PackageDir, "dist/ccloud-cli.js"), []byte("bad"), 0600)
			case "tamper-manifest":
				_ = os.WriteFile(filepath.Join(o.PackageDir, "BUNDLE.json"), []byte("{}"), 0600)
			case "symlink":
				_ = os.Remove(filepath.Join(o.PackageDir, "dist/ccloud-cli.js"))
				_ = os.Symlink("/dev/null", filepath.Join(o.PackageDir, "dist/ccloud-cli.js"))
			case "extra":
				_ = os.WriteFile(filepath.Join(o.PackageDir, "extra"), []byte("x"), 0600)
			}
			start := time.Now()
			item, e := b.Query(t.Context(), "ccloud_l4")
			if e != nil || item.Status != "unknown" || item.ReasonCode == "" || string(item.Data) != "null" || time.Since(start) > 3*time.Second {
				t.Fatal("failed query became observation", e, item.Status, item.ReasonCode)
			}
		})
	}
}
func TestCCloudProfilesPreserveLegacyBudgetAndSharedCoverage(t *testing.T) {
	for _, rule := range []string{"iac-sre-oma-canary", "iac-go-system-errors", "iac-go-latency-p99", "iac-go-traffic-zero", "iac-go-metrics-stale", "iac-go-commit-unknown", "iac-go-outbox-dead", "iac-go-outbox-lag", "iac-api-probe-not-all-200", "iac-api-probe-delay-high", "iac-redis002-vip-ras-down"} {
		old, next := Profile(rule), ProfileWithCCloud(rule)
		if slices.Contains(old, "ccloud_l4") || !slices.Contains(next, "ccloud_l4") || len(next) > MaxQueries {
			t.Fatal("profile/budget mismatch")
		}
		for _, id := range []string{"freshness", "entry_coverage", "vip_coverage"} {
			if slices.Contains(old, id) && !slices.Contains(next, id) {
				t.Fatal("lost required profile coverage")
			}
		}
	}
	if len(ProfileWithCCloud("untrusted")) != 0 {
		t.Fatal("untrusted rule gained capability")
	}
}
