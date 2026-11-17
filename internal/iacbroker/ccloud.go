package iacbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const CCloudManifestSHA256 = "036011de6bb8af591eb38b1119d54f5956d9ba44dd46731df523ee31e2690c28"
const ccloudBaseURL = "https://sre-control.test.seewo.com/api/v1/iac-ccloud"
const ccloudRevision = "npm:@cvte/ccloud-cli@2.2.1"

type CCloudOptions struct{ NodeBinary, PackageDir, ProxyToken string }
type ccloudRuntime struct {
	options               CCloudOptions
	manifestHash, baseURL string
}

func newCCloud(o CCloudOptions, hash string) *ccloudRuntime {
	if o.NodeBinary == "" {
		o.NodeBinary = "/usr/local/bin/node"
	}
	if o.PackageDir == "" {
		o.PackageDir = "/opt/ccloud-cli"
	}
	o.NodeBinary = filepath.Clean(o.NodeBinary)
	o.PackageDir = filepath.Clean(o.PackageDir)
	return &ccloudRuntime{options: o, manifestHash: hash, baseURL: ccloudBaseURL}
}
func fileDigest(ctx context.Context, path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 32*1024)
	total := 0
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		total += n
		if total > 16*1024*1024 {
			return "", fmt.Errorf("file limit")
		}
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify the independent pinned npm manifest and every package file before execution.
// No token, HOME/config, native addon or arbitrary executable is discovered from the environment.
func (c *ccloudRuntime) verify(ctx context.Context) (string, error) {
	o := c.options
	// Keep the traversal comparison in the same lexical form as Join/Dir, even for direct callers.
	o.PackageDir = filepath.Clean(o.PackageDir)
	if !filepath.IsAbs(o.NodeBinary) || !filepath.IsAbs(o.PackageDir) {
		return "", fmt.Errorf("invalid runtime path")
	}
	info, e := os.Stat(o.NodeBinary)
	if e != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", fmt.Errorf("node unavailable")
	}
	root, e := os.Lstat(o.PackageDir)
	if e != nil || !root.IsDir() {
		return "", fmt.Errorf("package directory unavailable")
	}
	manifestPath := filepath.Join(o.PackageDir, "BUNDLE.json")
	info, e = os.Lstat(manifestPath)
	if e != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("manifest unavailable")
	}
	f, e := os.Open(manifestPath)
	if e != nil {
		return "", e
	}
	raw, e := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	_ = f.Close()
	if e != nil || len(raw) > 2*1024*1024 {
		return "", fmt.Errorf("manifest unavailable")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != c.manifestHash {
		return "", fmt.Errorf("manifest mismatch")
	}
	var manifest struct {
		Package, Version string
		Files            []struct{ Path, SHA256 string }
	}
	if json.Unmarshal(raw, &manifest) != nil || manifest.Package != "@cvte/ccloud-cli" || manifest.Version != "2.2.1" || len(manifest.Files) == 0 || len(manifest.Files) > 5000 {
		return "", fmt.Errorf("invalid manifest")
	}
	files := map[string]bool{"BUNDLE.json": true}
	entryHash := ""
	for _, f := range manifest.Files {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if f.Path == "" || filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || strings.Contains(f.Path, "\\") || f.Path == ".." || strings.HasPrefix(f.Path, "../") || files[f.Path] || strings.HasSuffix(f.Path, ".node") {
			return "", fmt.Errorf("unsafe manifest")
		}
		files[f.Path] = true
		path := filepath.Join(o.PackageDir, f.Path)
		for parent := filepath.Dir(path); parent != o.PackageDir; parent = filepath.Dir(parent) {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if parent == filepath.Dir(parent) {
				return "", fmt.Errorf("package path outside root")
			}
			i, e := os.Lstat(parent)
			if e != nil || !i.IsDir() {
				return "", fmt.Errorf("unsafe file parent")
			}
		}
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("package file unavailable")
		}
		hash, e := fileDigest(ctx, path)
		if e != nil || hash != f.SHA256 {
			return "", fmt.Errorf("package file mismatch")
		}
		if f.Path == "dist/ccloud-cli.js" {
			entryHash = hash
		}
	}
	if entryHash == "" || !files["package.json"] {
		return "", fmt.Errorf("missing entrypoint")
	}
	if e := filepath.WalkDir(o.PackageDir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink denied")
		}
		if !d.IsDir() {
			rel, e := filepath.Rel(o.PackageDir, path)
			if e != nil || !files[rel] || !d.Type().IsRegular() {
				return fmt.Errorf("unexpected package file")
			}
		}
		return nil
	}); e != nil {
		return "", e
	}
	pkg, e := os.ReadFile(filepath.Join(o.PackageDir, "package.json"))
	var p struct{ Name, Version string }
	if e != nil || json.Unmarshal(pkg, &p) != nil || p.Name != "@cvte/ccloud-cli" || p.Version != "2.2.1" {
		return "", fmt.Errorf("package identity mismatch")
	}
	return entryHash, nil
}
func (b *Broker) queryCCloud(ctx context.Context, item Evidence) (Evidence, error) {
	item.Provenance = Provenance{SourceRevision: ccloudRevision, Query: "app pods --app-id " + AppID + " --cluster-id 60", Window: "instant pod-list observation; desired replicas unknown"}
	if b.ccloud == nil || b.ccloud.options.ProxyToken == "" {
		item.ReasonCode = "ccloud_not_configured"
		return item, nil
	}
	c := b.ccloud
	item.Provenance.PackageManifestSHA256 = c.manifestHash
	select {
	case b.gate <- struct{}{}:
		defer func() { <-b.gate }()
	case <-ctx.Done():
		item.ReasonCode = "cancelled"
		return item, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	hash, e := c.verify(runCtx)
	if e != nil {
		item.ReasonCode = "ccloud_runtime_unavailable"
		if runCtx.Err() != nil {
			item.ReasonCode = "deadline_or_cancelled"
		}
		return item, nil
	}
	item.Provenance.CLIHash = hash
	cmd := exec.CommandContext(runCtx, c.options.NodeBinary, filepath.Join(c.options.PackageDir, "dist/ccloud-cli.js"), "app", "pods", "--app-id", AppID, "--cluster-id", "60")
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TZ=UTC", "CCLOUD_API_MODE=real", "CCLOUD_DISABLE_UPDATE_CHECK=1", "CCLOUD_API_BASE_URL=" + c.baseURL, "CCLOUD_API_KEY=" + c.options.ProxyToken}
	for _, key := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	stdout, stderr := &boundedWriter{limit: maxOutput}, &boundedWriter{limit: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Bound pipe settlement too, including an unexpected child retaining inherited FDs.
	cmd.WaitDelay = time.Second
	e = cmd.Run()
	item.ObservedAt = time.Now().UTC()
	switch {
	case runCtx.Err() != nil:
		item.ReasonCode = "deadline_or_cancelled"
	case stdout.exceeded || stderr.exceeded:
		item.ReasonCode = "output_limit"
	case e != nil:
		item.ReasonCode = "ccloud_cli_failed"
	default:
		item.Data, item.Status, item.ReasonCode = parseCCloud(stdout.buf.Bytes())
	}
	return item, nil
}
func parseCCloud(raw []byte) (json.RawMessage, string, string) {
	bad := func() (json.RawMessage, string, string) {
		return json.RawMessage(`null`), "unknown", "invalid_ccloud_observation"
	}
	var envelope struct {
		Resource, Action string
		Code             json.RawMessage
		Data             json.RawMessage
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Resource != "app" || envelope.Action != "pods" {
		return bad()
	}
	if len(envelope.Code) > 0 {
		var code int
		if string(envelope.Code) == "null" || json.Unmarshal(envelope.Code, &code) != nil || code != 0 {
			return bad()
		}
	}
	var rows []struct {
		Name     *string `json:"podName"`
		Phase    *string `json:"phase"`
		Ready    *bool   `json:"ready"`
		Restarts *int64  `json:"restartCount"`
	}
	if json.Unmarshal(envelope.Data, &rows) != nil || rows == nil || len(rows) > 500 {
		return bad()
	}
	running, ready := 0, 0
	var restarts int64
	seen := map[string]bool{}
	for _, p := range rows {
		if p.Name == nil || strings.TrimSpace(*p.Name) == "" || seen[*p.Name] || p.Phase == nil || p.Ready == nil || p.Restarts == nil || *p.Restarts < 0 || *p.Restarts > 1<<53 || restarts > (1<<53)-*p.Restarts {
			return bad()
		}
		seen[*p.Name] = true
		switch *p.Phase {
		case "Running":
			running++
			if *p.Ready {
				ready++
			}
		case "Pending", "Succeeded", "Failed", "Unknown":
		default:
			return bad()
		}
		restarts += *p.Restarts
	}
	data, _ := json.Marshal(map[string]any{"pod_count": len(rows), "running": running, "ready_running": ready, "restarts_total": restarts, "desired_replicas": nil})
	return data, "ok", ""
}
