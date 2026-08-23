// Service Registration 分类（entity_layer / entity_kind）在 HTTP 边界上的口径覆盖。
//
// 这里只测请求解析与分类裁决这一层纯逻辑：serviceRequest 的 JSON 契约、"未传"与
// "显式空值"的区分、以及 422 稳定 reason code。create 的缺省补齐与 update 的
// "保留当前分类 / 分类不可变"落在 handler 与 store 事务里，分别由
// TestServiceRequest_CreateAppliesLegacyDefault 与
// internal/store/service_catalog_integration_test.go 覆盖——后者需要真实
// PostgreSQL，因为"同一事务读取当前分类"本身就是被测行为，用 fake 会把它测空。
package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"alert-system/internal/domain"
)

func decodeServiceRequest(t *testing.T, body string) serviceRequest {
	t.Helper()
	var request serviceRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("解析 serviceRequest 失败: %v", err)
	}
	return request
}

const validServiceBody = `{
	"entity_key": "service/cch/prod",
	"name": "CCH",
	"environment": "prod",
	"owner_team_id": "3f6a5b52-6c4a-4a1e-9b4d-2a1c8f0d7e11",
	"tier": "tier-1"
}`

// 旧 V1 request 完全不带两个新字段：解析结果必须是"未传"（空分类），而不是被解析成
// 显式空字符串再报错——否则一次升级就会打断所有既有调用方。
func TestServiceRequest_LegacyRequestReportsUnsetClassification(t *testing.T) {
	request := decodeServiceRequest(t, validServiceBody)
	if request.EntityLayer != nil || request.EntityKind != nil {
		t.Fatalf("旧 request 应为未传, got layer=%v kind=%v", request.EntityLayer, request.EntityKind)
	}
	layer, kind, err := request.classification()
	if err != nil {
		t.Fatalf("未传分类不应报错: %v", err)
	}
	if layer != "" || kind != "" {
		t.Fatalf("未传分类应返回空 pair, got %q/%q", layer, kind)
	}
}

// create 路径把"未传"补成 application/business_system；这是唯一允许出现缺省的地方，
// update 必须保留当前分类而不是回落到 application。
func TestServiceRequest_CreateAppliesLegacyDefault(t *testing.T) {
	request := decodeServiceRequest(t, validServiceBody)
	service, err := request.domain()
	if err != nil {
		t.Fatalf("合法旧 request 应解析成功: %v", err)
	}
	if service.Entity.Layer != "" || service.Entity.Kind != "" {
		t.Fatalf("domain() 不应自行补缺省, got %q/%q", service.Entity.Layer, service.Entity.Kind)
	}
	applyCreateClassificationDefault(&service)
	if service.Entity.Layer != domain.EntityLayerApplication || service.Entity.Kind != domain.ResourceNodeKindBusinessSystem {
		t.Fatalf("create 缺省=%q/%q, want application/business_system", service.Entity.Layer, service.Entity.Kind)
	}

	explicit := decodeServiceRequest(t, `{
		"entity_key": "platform/e2b/prod", "name": "E2B", "environment": "prod",
		"owner_team_id": "3f6a5b52-6c4a-4a1e-9b4d-2a1c8f0d7e11", "tier": "tier-1",
		"entity_layer": "platform", "entity_kind": "infrastructure_capability"
	}`)
	platform, err := explicit.domain()
	if err != nil {
		t.Fatalf("platform request 应解析成功: %v", err)
	}
	applyCreateClassificationDefault(&platform)
	if platform.Entity.Layer != domain.EntityLayerPlatform ||
		platform.Entity.Kind != domain.ResourceNodeKindInfrastructureCapability {
		t.Fatalf("显式 platform 分类被改写为 %q/%q", platform.Entity.Layer, platform.Entity.Kind)
	}
}

// 两个字段是一个整体：只传一个无法判断调用方意图，静默补另一半会把 E2B 这类
// platform 能力登记成 application。
func TestServiceRequest_PartialClassificationIsRejected(t *testing.T) {
	for _, body := range []string{
		`{"entity_layer": "platform"}`,
		`{"entity_kind": "infrastructure_capability"}`,
		`{"entity_layer": "application"}`,
		`{"entity_kind": "business_system"}`,
	} {
		request := decodeServiceRequest(t, body)
		_, _, err := request.classification()
		if err == nil {
			t.Fatalf("%s 只传单个字段应报错", body)
		}
		if code := reasonCodeOf(err); code != "service_classification_incomplete" {
			t.Fatalf("%s reason_code=%q, want service_classification_incomplete", body, code)
		}
	}
}

// 显式空字符串是"我要清空分类"，不是"未传"；分类没有合法空值，必须 422。
func TestServiceRequest_ExplicitEmptyClassificationIsRejected(t *testing.T) {
	for _, body := range []string{
		`{"entity_layer": "", "entity_kind": ""}`,
		`{"entity_layer": "", "entity_kind": "business_system"}`,
		`{"entity_layer": "application", "entity_kind": ""}`,
		`{"entity_layer": "  ", "entity_kind": "  "}`,
	} {
		request := decodeServiceRequest(t, body)
		_, _, err := request.classification()
		if err == nil {
			t.Fatalf("%s 显式空值应报错", body)
		}
		if code := reasonCodeOf(err); code != "service_classification_invalid" {
			t.Fatalf("%s reason_code=%q, want service_classification_invalid", body, code)
		}
	}
}

// infrastructure/resource 与跨 pair 混搭都不是合法服务分类。
func TestServiceRequest_InvalidClassificationPairIsRejected(t *testing.T) {
	for _, body := range []string{
		`{"entity_layer": "infrastructure", "entity_kind": "resource"}`,
		`{"entity_layer": "platform", "entity_kind": "business_system"}`,
		`{"entity_layer": "application", "entity_kind": "infrastructure_capability"}`,
		`{"entity_layer": "workload", "entity_kind": "business_system"}`,
	} {
		request := decodeServiceRequest(t, body)
		_, _, err := request.classification()
		if err == nil {
			t.Fatalf("%s 非法 pair 应报错", body)
		}
		if code := reasonCodeOf(err); code != "service_classification_invalid" {
			t.Fatalf("%s reason_code=%q, want service_classification_invalid", body, code)
		}
		if !errors.Is(err, domain.ErrInvalidServiceClassification) {
			t.Fatalf("%s 应包裹 domain.ErrInvalidServiceClassification, got %v", body, err)
		}
	}
}

// 合法 pair 原样进入 domain 对象，不做任何归一化猜测。
func TestServiceRequest_ValidClassificationRoundTrips(t *testing.T) {
	cases := []struct {
		layer domain.EntityLayer
		kind  domain.ResourceNodeKind
	}{
		{domain.EntityLayerApplication, domain.ResourceNodeKindBusinessSystem},
		{domain.EntityLayerPlatform, domain.ResourceNodeKindInfrastructureCapability},
	}
	for _, tc := range cases {
		layerValue, kindValue := string(tc.layer), string(tc.kind)
		request := decodeServiceRequest(t, validServiceBody)
		request.EntityLayer, request.EntityKind = &layerValue, &kindValue
		service, err := request.domain()
		if err != nil {
			t.Fatalf("%q/%q 应解析成功: %v", tc.layer, tc.kind, err)
		}
		if service.Entity.Layer != tc.layer || service.Entity.Kind != tc.kind {
			t.Fatalf("分类漂移: got %q/%q want %q/%q",
				service.Entity.Layer, service.Entity.Kind, tc.layer, tc.kind)
		}
		if !service.Entity.Managed {
			t.Fatalf("服务实体必须是受管实体")
		}
	}
}

// ---- C5：Dashboard/Runbook 链接安全策略 ----
//
// 需要显式记录的口径：拒绝 mailto、自定义 scheme、裸主机与 scheme-relative 输入是对 V1
// 写入口径的**有意安全收紧**。这些值此前可以写进登记并被渲染成运营页面的可点击链接，而
// 值班人会在事故中直接点开它们。空值继续合法（未配置 Dashboard/Runbook 是正常状态）。
func TestServiceRequest_RejectsUnsafeDashboardAndRunbookLinks(t *testing.T) {
	// 用例来自与 safelink/前端共用的策略表，避免三处各写一份清单后互相漂移。
	raw, err := os.ReadFile(filepath.Join("..", "safelink", "testdata", "link_policy_cases.json"))
	if err != nil {
		t.Fatalf("读取共享链接策略用例表失败: %v", err)
	}
	var table struct {
		Cases []struct {
			Name string `json:"name"`
			Href string `json:"href"`
			Safe bool   `json:"safe"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("解析共享链接策略用例表失败: %v", err)
	}

	rejected, accepted := 0, 0
	for _, tc := range table.Cases {
		// 空值/纯空白在写入侧是合法的"未配置"，不属于本用例。
		if strings.TrimSpace(tc.Href) == "" {
			continue
		}
		for _, field := range []string{"dashboard_url", "runbook_url"} {
			request := decodeServiceRequest(t, validServiceBody)
			if field == "dashboard_url" {
				request.DashboardURL = tc.Href
			} else {
				request.RunbookURL = tc.Href
			}
			service, err := request.domain()
			if tc.Safe {
				if err != nil {
					t.Fatalf("%s: %s=%q 应被接受: %v", tc.Name, field, tc.Href, err)
				}
				stored := service.DashboardURL
				if field == "runbook_url" {
					stored = service.RunbookURL
				}
				if stored != strings.TrimSpace(tc.Href) {
					t.Fatalf("%s: %s 被改写为 %q", tc.Name, field, stored)
				}
				accepted++
				continue
			}
			if err == nil {
				t.Fatalf("%s: %s=%q 应被拒绝", tc.Name, field, tc.Href)
			}
			if code := reasonCodeOf(err); code != reasonServiceLinkInvalid {
				t.Fatalf("%s: %s=%q reason_code=%q, want %q",
					tc.Name, field, tc.Href, code, reasonServiceLinkInvalid)
			}
			rejected++
		}
	}
	// 防止用例表被清空后测试变成空跑。
	if rejected < 40 || accepted < 20 {
		t.Fatalf("共享用例表覆盖不足: rejected=%d accepted=%d", rejected, accepted)
	}
}

// hostless 与 malformed 的绝对 URL 必须在写入侧就被拒绝：只检查"scheme 之后还有字符"
// 会让这些值落库，之后被渲染成一个点开后行为未定义的链接。
func TestServiceRequest_RejectsHostlessAndMalformedAbsoluteLinks(t *testing.T) {
	for _, href := range []string{
		"https://user@/path", "https://:443/path", "https://user@?x",
		"http:///runbooks", "http:///evil.example.com", "https://?x=1", "https://#x",
		"https://[::1", "https://exa mple.com", "https://evil.example:bad/path",
	} {
		request := decodeServiceRequest(t, validServiceBody)
		request.DashboardURL = href
		if _, err := request.domain(); err == nil {
			t.Fatalf("%q 没有可用主机名或无法解析，应被拒绝", href)
		} else if code := reasonCodeOf(err); code != reasonServiceLinkInvalid {
			t.Fatalf("%q reason_code=%q, want %q", href, code, reasonServiceLinkInvalid)
		}
	}
	// 反向控制：带 userinfo / 端口 / 括号 IPv6 的真实主机必须继续写得进去。
	for _, href := range []string{
		"https://user@grafana.example.com/d", "https://user:pass@grafana.example.com/d",
		"https://grafana.example.com:3000/d", "https://[::1]:8080/d",
		"http://10.60.199.160:9090/graph", "HtTpS://grafana.example.com/d",
	} {
		request := decodeServiceRequest(t, validServiceBody)
		request.DashboardURL = href
		if _, err := request.domain(); err != nil {
			t.Fatalf("%q 有真实主机名，应被接受: %v", href, err)
		}
	}
}

// 合法链接原样保留（只 trim 外层空白）；空值仍然合法。
func TestServiceRequest_AcceptsInternalAndHTTPLinks(t *testing.T) {
	cases := []struct {
		href string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"/settings/services/svc-1", "/settings/services/svc-1"},
		{"  /runbooks/e2b  ", "/runbooks/e2b"},
		{"/alerts?service_id=svc-1", "/alerts?service_id=svc-1"},
		{"https://grafana.example.com/d/abc", "https://grafana.example.com/d/abc"},
		{"http://grafana.example.com:3000/d/abc", "http://grafana.example.com:3000/d/abc"},
		{"HtTpS://grafana.example.com/d", "HtTpS://grafana.example.com/d"},
	}
	for _, tc := range cases {
		request := decodeServiceRequest(t, validServiceBody)
		request.DashboardURL = tc.href
		request.RunbookURL = tc.href
		service, err := request.domain()
		if err != nil {
			t.Fatalf("%q 应被接受: %v", tc.href, err)
		}
		if service.DashboardURL != tc.want || service.RunbookURL != tc.want {
			t.Fatalf("%q 解析为 dashboard=%q runbook=%q, want %q",
				tc.href, service.DashboardURL, service.RunbookURL, tc.want)
		}
	}
}
