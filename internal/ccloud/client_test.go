package ccloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestFetchFullLinkTopology_RealFixture 用 fulllink_topology.json（24 节点/77 边，
// 契约事实见 testdata/README.md）验证真实响应能被正确反序列化。
func TestFetchFullLinkTopology_RealFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("projectId") != "proj-1" {
			t.Errorf("projectId = %q, want proj-1", r.URL.Query().Get("projectId"))
		}
		serveFixture(t, w, "testdata/fulllink_topology.json")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil)
	got, err := c.FetchFullLinkTopology(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("FetchFullLinkTopology() error = %v", err)
	}
	if len(got.Nodes) != 24 {
		t.Errorf("Nodes = %d, want 24", len(got.Nodes))
	}
	if len(got.Edges) != 77 {
		t.Errorf("Edges = %d, want 77", len(got.Edges))
	}

	// health 的 null 与 0.0 必须区分：chang_ting_waf 是 null，nginx-prd-it（APISIX）是 0.0。
	var sawNullHealth, sawZeroHealth bool
	for _, n := range got.Nodes {
		switch n.Key {
		case "chang_ting_waf":
			if n.Health != nil {
				t.Errorf("chang_ting_waf.Health = %v, want nil", *n.Health)
			}
			sawNullHealth = true
		case "60-kube-system-nginx-prd-it":
			if n.Health == nil || *n.Health != 0.0 {
				t.Errorf("nginx-prd-it.Health = %v, want 0.0（非缺失）", n.Health)
			}
			sawZeroHealth = true
		}
	}
	if !sawNullHealth || !sawZeroHealth {
		t.Fatalf("未覆盖 health null/0.0 两种样本：sawNullHealth=%v sawZeroHealth=%v", sawNullHealth, sawZeroHealth)
	}
}

// TestFetchServiceTopology_RealFixture 用 service_topology_cplm-boot-prod.json
// （22 节点/12 边）验证 RELY_NODE/EXTERNAL_HTTP_SERVER 与 nodeType 为空、
// serviceType 前导空格等契约坑都能原样透传（不在 client 层做任何清洗）。
func TestFetchServiceTopology_RealFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("appId") != "4f2b08e91ce2f89281397c4579f8f5f435e69985" {
			t.Errorf("appId = %q", r.URL.Query().Get("appId"))
		}
		serveFixture(t, w, "testdata/service_topology_cplm-boot-prod.json")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil)
	got, err := c.FetchServiceTopology(context.Background(), "4f2b08e91ce2f89281397c4579f8f5f435e69985")
	if err != nil {
		t.Fatalf("FetchServiceTopology() error = %v", err)
	}
	if len(got.Nodes) != 22 {
		t.Errorf("Nodes = %d, want 22", len(got.Nodes))
	}
	if len(got.Edges) != 12 {
		t.Errorf("Edges = %d, want 12", len(got.Edges))
	}

	var foundEmptyNodeType, foundLeadingSpaceServiceType, foundNilMetadata bool
	for _, n := range got.Nodes {
		if n.Key == "a82b1dd76e5f4f7abed1271449f4ef97" {
			if n.NodeType != "" {
				t.Errorf("EXTERNAL_HTTP_SERVER 节点 nodeType = %q, want 空", n.NodeType)
			}
			foundEmptyNodeType = true
		}
		if n.ServiceType != " POSTGRESQL" {
			continue
		}
		foundLeadingSpaceServiceType = true
		if n.TrimmedServiceType() != "POSTGRESQL" {
			t.Errorf("TrimmedServiceType() = %q, want POSTGRESQL", n.TrimmedServiceType())
		}
		if n.Metadata != nil {
			t.Errorf("RELY_NODE metadata 应为 nil, got %+v", n.Metadata)
		}
		foundNilMetadata = true
	}
	if !foundEmptyNodeType || !foundLeadingSpaceServiceType || !foundNilMetadata {
		t.Fatalf("未覆盖全部契约坑样本: emptyNodeType=%v leadingSpace=%v nilMetadata=%v",
			foundEmptyNodeType, foundLeadingSpaceServiceType, foundNilMetadata)
	}

	// message:null 且外层无 trace 字段，仍应成功解析（不依赖 message 做成功判据）。
}

func TestFetchAppHealth_EmptyInputSkipsRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil)
	got, err := c.FetchAppHealth(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchAppHealth() error = %v", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
	if called {
		t.Errorf("appIDs 为空时不应发起请求")
	}
}

func TestFetchAppHealth_NullAndZeroHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":[
			{"appId":"app-null","health":null,"slaStandard":0.999},
			{"appId":"app-zero","health":0.0,"slaStandard":1.0}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil)
	got, err := c.FetchAppHealth(context.Background(), []string{"app-null", "app-zero"})
	if err != nil {
		t.Fatalf("FetchAppHealth() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Health != nil {
		t.Errorf("app-null.Health = %v, want nil", *got[0].Health)
	}
	if got[1].Health == nil || *got[1].Health != 0.0 {
		t.Errorf("app-zero.Health = %v, want 0.0", got[1].Health)
	}
}

func TestFetch_TimeoutIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		serveFixture(t, w, "testdata/fulllink_topology.json")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", &http.Client{Timeout: 5 * time.Millisecond})
	_, err := c.FetchFullLinkTopology(context.Background(), "proj-1")
	if err == nil {
		t.Fatalf("FetchFullLinkTopology() error = nil, want 超时错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want 可用 errors.Is(err, context.DeadlineExceeded) 判断", err)
	}
}

func TestFetch_BusinessErrorIsDistinguishableFromTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"message":"internal error","data":{"nodes":[],"edges":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil)
	_, err := c.FetchFullLinkTopology(context.Background(), "proj-1")
	if err == nil {
		t.Fatalf("FetchFullLinkTopology() error = nil, want 业务错误")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("业务错误不应被误判为超时: %v", err)
	}
	if !strings.Contains(err.Error(), "code=1") {
		t.Errorf("error = %v, want 包含 code=1", err)
	}
}

func TestFetch_HTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil)
	_, err := c.FetchServiceTopology(context.Background(), "app-1")
	if err == nil {
		t.Fatalf("FetchServiceTopology() error = nil, want HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want 包含状态码", err)
	}
}

func TestFetch_BearerTokenFromEnv(t *testing.T) {
	t.Setenv("CCLOUD_TEST_TOKEN", "s3cr3t")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"nodes":[],"edges":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "CCLOUD_TEST_TOKEN", nil)
	if _, err := c.FetchFullLinkTopology(context.Background(), "proj-1"); err != nil {
		t.Fatalf("FetchFullLinkTopology() error = %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want Bearer s3cr3t", gotAuth)
	}
}

func TestFetch_MissingTokenEnvErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("凭据缺失时不应发起请求")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "CCLOUD_TEST_TOKEN_UNSET", nil)
	_, err := c.FetchFullLinkTopology(context.Background(), "proj-1")
	if err == nil {
		t.Fatalf("FetchFullLinkTopology() error = nil, want 凭据环境变量未设置")
	}
}

func TestFetch_CustomPathsOverridePlaceholders(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"nodes":[],"edges":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", nil).WithPaths(ClientPaths{FullLink: "/real/fulllink"})
	if _, err := c.FetchFullLinkTopology(context.Background(), "proj-1"); err != nil {
		t.Fatalf("FetchFullLinkTopology() error = %v", err)
	}
	if gotPath != "/real/fulllink" {
		t.Fatalf("path = %q, want /real/fulllink", gotPath)
	}
}

func serveFixture(t *testing.T, w http.ResponseWriter, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write fixture response: %v", err)
	}
}
