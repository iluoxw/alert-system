package iacbroker

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"alert-system/internal/iacccloudproxy"
)

type localCCloudTransport func(*http.Request) (*http.Response, error)

func (f localCCloudTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCCloudOfficialCLIColdCacheAndNonzeroAPIResponse(t *testing.T) {
	archive := os.Getenv("IAC_CCLOUD_TEST_ARCHIVE")
	if archive == "" {
		t.Skip("set IAC_CCLOUD_TEST_ARCHIVE to the audited public npm archive")
	}
	node, e := exec.LookPath("node")
	if e != nil {
		t.Fatal(e)
	}
	node, e = filepath.Abs(node)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(archive)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != "df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87" {
		t.Fatal("test archive is not the audited public package")
	}
	f, e := os.Open(archive)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	dir := t.TempDir()
	for {
		h, e := tarReader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(h.Name, "ccloud-cli/") || strings.Contains(h.Name, "..") {
			t.Fatal("unsafe archive")
		}
		path := filepath.Join(dir, h.Name)
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		out, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, e = io.Copy(out, tarReader)
		_ = out.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	packageDir := filepath.Join(dir, "ccloud-cli")
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(key, "")
	}
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "upstream_nonzero_code"}[failed], func(t *testing.T) {
			var metadata, pods atomic.Int32
			token := strings.Repeat("n", 40)
			handler, e := iacccloudproxy.New(iacccloudproxy.Options{ProxyToken: token, CloudAPIKey: "fixture-upstream-key", Client: &http.Client{Transport: localCCloudTransport(func(r *http.Request) (*http.Response, error) {
				pods.Add(1)
				body := `{"code":0,"message":"","data":[{"podName":"pod-one","phase":"Running","ready":true,"restartCount":0}]}`
				if failed {
					body = `{"code":401,"message":"suppressed","data":[]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}})
			if e != nil {
				t.Fatal(e)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/meta/") {
					metadata.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			// Only the host fixture shim supplies a fresh private cache HOME. The broker's child env never inherits HOME.
			// Production uses the image's controlled passwd home under /tmp, validated by the image acceptance test.
			home := t.TempDir()
			wrapper := filepath.Join(t.TempDir(), "node")
			if e := os.WriteFile(wrapper, []byte("#!/bin/sh\nexport HOME='"+home+"'\nexec '"+node+"' \"$@\"\n"), 0700); e != nil {
				t.Fatal(e)
			}
			b := fakeCLI(t, "exit 99")
			b.timeout = 10 * time.Second
			b.ccloud = newCCloud(CCloudOptions{NodeBinary: wrapper, PackageDir: packageDir, ProxyToken: token}, CCloudManifestSHA256)
			b.ccloud.baseURL = server.URL + iacccloudproxy.Prefix
			item, e := b.Query(t.Context(), "ccloud_l4")
			if e != nil {
				t.Fatal(e)
			}
			want := "ok"
			if failed {
				want = "unknown"
			}
			if item.Status != want {
				t.Fatal("official CLI result mismatch", item.Status, item.ReasonCode)
			}
			if metadata.Load() != 2 || pods.Load() != 1 {
				t.Fatal("cold-cache chain was not resources/actions/pods", metadata.Load(), pods.Load())
			}
			if item.Provenance.PackageManifestSHA256 != CCloudManifestSHA256 || item.Provenance.CLIHash == "" || item.Provenance.Query == "" {
				t.Fatal("missing independent provenance")
			}
		})
	}
}
