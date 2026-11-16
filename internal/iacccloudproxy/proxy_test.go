package iacccloudproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const proxyTestToken = "narrow-client-token-with-at-least-32-bytes"
const cloudTestKey = "server-side-key-never-exposed-to-model"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func makeHandler(t *testing.T, body string, calls *atomic.Int32) *Handler {
	t.Helper()
	h, e := New(Options{ProxyToken: proxyTestToken, CloudAPIKey: cloudTestKey, Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Host != "ccloud.gz.cvte.cn" || r.URL.Path != podsPath || r.URL.Query().Get("appId") != AppID || r.URL.Query().Get("clusterId") != "60" {
			t.Error("upstream scope broadened")
		}
		if r.Header.Get("Authorization") != "Bearer "+cloudTestKey {
			t.Error("wrong upstream authorization")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}})
	if e != nil {
		t.Fatal(e)
	}
	return h
}

const podFixture = `{"code":0,"message":"","data":[{"podName":"app-iac-123","phase":"Running","ready":true,"restartCount":0,"nodeName":"node-1","podIP":"10.0.0.1","startTime":"2026-11-16","env":{"TOKEN":"secret"},"labels":{"secret":"must-not-leak"}}]}`

func TestReadOnlyScopeAndCredentialIsolation(t *testing.T) {
	var calls atomic.Int32
	h := makeHandler(t, podFixture, &calls)
	path := Prefix + podsPath + "?appId=" + AppID + "&clusterId=60"
	for _, tt := range []struct {
		method, path, auth, body string
		want                     int
	}{
		{"GET", path, "", "", 401}, {"GET", path, "Bearer bad", "", 401}, {"POST", path, "Bearer " + proxyTestToken, "", 405},
		{"PUT", path, "Bearer " + proxyTestToken, "", 405}, {"DELETE", path, "Bearer " + proxyTestToken, "", 405},
		{"GET", path + "&appVersionId=x", "Bearer " + proxyTestToken, "", 403}, {"GET", strings.Replace(path, AppID, "other", 1), "Bearer " + proxyTestToken, "", 403},
		{"GET", path + "&clusterId=60", "Bearer " + proxyTestToken, "", 403}, {"GET", strings.Replace(path, "clusterId=60", "clusterId=50", 1), "Bearer " + proxyTestToken, "", 403},
		{"GET", Prefix + "/cloud/api/v1/agent/auth/me", "Bearer " + proxyTestToken, "", 404}, {"GET", path, "Bearer " + proxyTestToken, "x", 400},
		{"GET", Prefix + resourcesPath + "?extra=x", "Bearer " + proxyTestToken, "", 400}, {"GET", Prefix + actionsPath, "Bearer " + proxyTestToken, "", 200},
		{"GET", path, "Bearer " + proxyTestToken, "", 200},
	} {
		r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
		if tt.auth != "" {
			r.Header.Set("Authorization", tt.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Fatalf("%s %s got%d want%d: %s", tt.method, tt.path, w.Code, tt.want, w.Body.String())
		}
		for _, secret := range []string{proxyTestToken, cloudTestKey, "must-not-leak", "podIP", "startTime"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("sensitive/unnecessary data leaked: %s", secret)
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("denied/metadata request reached upstream: %d", calls.Load())
	}
}
func TestInvalidAndOversizedObservationStayUnavailable(t *testing.T) {
	for _, body := range []string{`{"code":401,"message":"Bearer secret","data":[]}`, `{"code":0,"data":null}`, `{"code":0,"data":[{"podName":"x","phase":"Running","ready":true}]}`, strings.Repeat("x", maxResponse+1)} {
		var calls atomic.Int32
		h := makeHandler(t, body, &calls)
		r := httptest.NewRequest("GET", podsPath+"?appId="+AppID+"&clusterId=60", nil)
		r.Header.Set("Authorization", "Bearer "+proxyTestToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 502 || strings.Contains(w.Body.String(), "Bearer secret") {
			t.Fatal("invalid observation leaked or became success")
		}
	}
	var calls atomic.Int32
	h := makeHandler(t, `{"code":0,"data":[]}`, &calls)
	r := httptest.NewRequest("GET", podsPath+"?appId="+AppID+"&clusterId=60", nil)
	r.Header.Set("Authorization", "Bearer "+proxyTestToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal("valid empty pod list corrupted")
	}
}
func TestDeadlineAndConcurrencyBound(t *testing.T) {
	entered := make(chan struct{}, 1)
	h, e := New(Options{ProxyToken: proxyTestToken, CloudAPIKey: cloudTestKey, Timeout: time.Second, MaxConcurrent: 1, Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}})
	if e != nil {
		t.Fatal(e)
	}
	req := func() *http.Request {
		r := httptest.NewRequest("GET", podsPath+"?appId="+AppID+"&clusterId=60", nil)
		r.Header.Set("Authorization", "Bearer "+proxyTestToken)
		return r
	}
	done := make(chan int, 1)
	go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, req()); done <- w.Code }()
	<-entered
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req())
	if w.Code != 503 {
		t.Fatal("unbounded requests")
	}
	select {
	case code := <-done:
		if code != 502 {
			t.Fatal(code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadline unbounded")
	}
}

// Supply the official installed/bundled JS entrypoint. This cold-cache test has no live CCloud calls.
func TestOfficialCLIColdCacheThroughAdapter(t *testing.T) {
	cli := os.Getenv("IAC_CCLOUD_CLI_PATH")
	if cli == "" {
		t.Skip("official CLI path required for local integration fixture")
	}
	var calls atomic.Int32
	h := makeHandler(t, podFixture, &calls)
	var mu sync.Mutex
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("request method=%s path=%s raw=%s query=%s content_length=%d encoding=%v", r.Method, r.URL.Path, r.URL.RawPath, r.URL.RawQuery, r.ContentLength, r.TransferEncoding)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+proxyTestToken {
			t.Error("official CLI did not use narrow token")
		}
		h.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", cli, "app", "pods", "--app-id", AppID, "--cluster-id", "60")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "CCLOUD_API_BASE_URL=" + server.URL + Prefix, "CCLOUD_API_KEY=" + proxyTestToken, "CCLOUD_API_MODE=real", "CCLOUD_DISABLE_UPDATE_CHECK=1"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("CLI fixture failed: %v %s", err, stderr.String())
	}
	var result struct{ Data []Pod }
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || len(result.Data) != 1 || !result.Data[0].Ready {
		t.Fatalf("CLI shape incompatible: %s", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 || paths[0] != Prefix+resourcesPath || paths[1] != Prefix+actionsPath || paths[2] != Prefix+podsPath || calls.Load() != 1 {
		t.Fatalf("unexpected cold-cache chain: %v", paths)
	}
}

func TestRedirectCannotForwardPrivilegedCredential(t *testing.T) {
	var calls atomic.Int32
	h, e := New(Options{ProxyToken: proxyTestToken, CloudAPIKey: cloudTestKey, Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://other.invalid/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}})
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("GET", podsPath+"?appId="+AppID+"&clusterId=60", nil)
	r.Header.Set("Authorization", "Bearer "+proxyTestToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if calls.Load() != 1 || w.Code != 502 {
		t.Fatal("redirect followed or treated as observation")
	}
}
