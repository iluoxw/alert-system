// model 是 contracts v1 的 Go 镜像，因此最有价值的测试就是"拿 canonical fixture
// round-trip 一圈，形状不能变"：任何字段名拼错、漏字段、错用 omitempty 都会在这里暴露。
// 第二组测试盯住 required 数组不得序列化成 null——那会让页面在解构时直接崩。
package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const examplesDir = "../../../docs/contracts/service-observability/v1/examples"

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(examplesDir, name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return raw
}

// roundTrip 把 fixture 解码进 target、再编码回来，断言两侧规范化 JSON 完全一致。
// 解码用 DisallowUnknownFields：fixture 里出现 model 没有的字段等于镜像不完整。
func roundTrip(t *testing.T, name string, target any) {
	t.Helper()
	raw := loadFixture(t, name)

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("%s 解码进 %T 失败（model 与契约漂移）: %v", name, target, err)
	}

	encoded, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("%s 重新编码失败: %v", name, err)
	}

	var original, produced any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatalf("解析 fixture %s 失败: %v", name, err)
	}
	if err := json.Unmarshal(encoded, &produced); err != nil {
		t.Fatalf("解析重新编码结果失败: %v", err)
	}
	if !reflect.DeepEqual(original, produced) {
		t.Fatalf("%s round-trip 后形状变了。\n原始: %s\n输出: %s", name, raw, encoded)
	}
}

func TestProfileMirrorsContract(t *testing.T) {
	var profile Profile
	roundTrip(t, "e2b.profile.json", &profile)

	if profile.SchemaVersion != SchemaVersion || profile.Status != ProfileStatusPublished {
		t.Fatalf("profile 头部字段错误: %+v", profile.SchemaVersion)
	}
	golden := profile.GoldenBindings()
	if len(golden) != 4 {
		t.Fatalf("golden binding=%d, want 4", len(golden))
	}
	for i, semantic := range GoldenSemantics() {
		if golden[i].Semantic != semantic {
			t.Fatalf("golden[%d] semantic=%s want %s", i, golden[i].Semantic, semantic)
		}
		if golden[i].Family != SignalFamilyGolden {
			t.Fatalf("golden[%d] family=%s", i, golden[i].Family)
		}
	}
	focus, ok := profile.PrimaryFocusKind()
	if !ok || focus.Key != "sandbox_id" {
		t.Fatalf("primary focus=%+v ok=%v", focus, ok)
	}
	// Traffic 没有阈值：这不是缺字段，而是"没有方向阈值就恒 neutral"的契约表达。
	traffic, ok := profile.BindingByKey("golden_create_traffic")
	if !ok {
		t.Fatalf("找不到 golden_create_traffic")
	}
	if traffic.Thresholds != nil {
		t.Fatalf("Traffic 不应带方向阈值: %+v", traffic.Thresholds)
	}
	if traffic.BaselineRef == "" {
		t.Fatalf("Traffic 应保留 baseline_ref 以显示基线变化")
	}
	saturation, _ := profile.BindingByKey("golden_capacity_saturation")
	if saturation.MeasurementRole != MeasurementRoleProxy || saturation.ProxyReason == "" {
		t.Fatalf("容量压力 proxy 必须自带 measurement_role 与 proxy_reason: %+v", saturation)
	}
}

func TestDiagnosticContextMirrorsContract(t *testing.T) {
	var context DiagnosticContext
	roundTrip(t, "e2b.context.json", &context)

	if context.Focus == nil || context.Focus.Kind != "sandbox_id" {
		t.Fatalf("focus 未正确镜像: %+v", context.Focus)
	}
	if len(context.Signals) == 0 || len(context.EvidenceRefs) == 0 {
		t.Fatalf("context 应带 signals 与 evidence_refs")
	}
	// value 是 nullable：unavailable 信号必须是 nil 指针，不能被解成 0。
	var unavailable *Signal
	for i := range context.Signals {
		if context.Signals[i].BindingKey == "use_host_placement_saturation" {
			unavailable = &context.Signals[i]
		}
	}
	if unavailable == nil {
		t.Fatalf("fixture 应带 use_host_placement_saturation")
	}
	if unavailable.Value != nil {
		t.Fatalf("unavailable signal 的 value 必须是 nil, got %v", *unavailable.Value)
	}
	if unavailable.Availability != SignalAvailabilityUnavailable ||
		unavailable.Health != SignalHealthUnknown || unavailable.UnknownReason == "" {
		t.Fatalf("unavailable signal 未保持 Unknown 语义: %+v", unavailable)
	}
	if context.DomainState.Impacts == nil || len(context.DomainState.Impacts) != 0 {
		t.Fatalf("空 impacts 必须是空 slice 而不是 nil: %#v", context.DomainState.Impacts)
	}
}

func TestPageViewsMirrorContract(t *testing.T) {
	var directory DirectoryView
	roundTrip(t, "e2b.directory-view.json", &directory)
	if directory.ViewKind != ViewKindServiceDirectory {
		t.Fatalf("directory view_kind=%s", directory.ViewKind)
	}
	// 目录里带 profile=null 的关联行是治理入口：镜像必须能表达 nil profile。
	sawNilProfile := false
	for _, item := range directory.Items {
		if item.Profile == nil {
			sawNilProfile = true
		}
	}
	if !sawNilProfile {
		t.Fatalf("directory fixture 应至少有一行 profile=null，用于验证未配置入口")
	}

	var overview OverviewView
	roundTrip(t, "e2b.overview-view.json", &overview)
	if overview.ViewKind != ViewKindServiceOverview || overview.Profile == nil {
		t.Fatalf("overview 头部字段错误: %+v", overview.ViewKind)
	}
	if len(overview.GoldenSignals) != 4 {
		t.Fatalf("overview golden_signals=%d, want 4", len(overview.GoldenSignals))
	}

	var workspace WorkspaceView
	roundTrip(t, "e2b.workspace-view.json", &workspace)
	if workspace.ViewKind != ViewKindObservabilityWorkspace || workspace.Focus == nil {
		t.Fatalf("workspace 头部字段错误: %+v", workspace.ViewKind)
	}
	if len(workspace.GoldenSignals) != 4 {
		t.Fatalf("workspace golden_signals=%d, want 4", len(workspace.GoldenSignals))
	}
	if workspace.Capabilities.ViewDomain != true {
		t.Fatalf("workspace fixture 的 view_domain 应为 true")
	}
}

func TestToolDescriptorsMirrorContract(t *testing.T) {
	raw := loadFixture(t, "tool-catalog.json")
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var descriptors []ToolDescriptor
	if err := decoder.Decode(&descriptors); err != nil {
		t.Fatalf("tool catalog 解码失败（model 与契约漂移）: %v", err)
	}
	encoded, err := json.Marshal(descriptors)
	if err != nil {
		t.Fatalf("重新编码失败: %v", err)
	}
	var original, produced any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	if err := json.Unmarshal(encoded, &produced); err != nil {
		t.Fatalf("解析输出失败: %v", err)
	}
	if !reflect.DeepEqual(original, produced) {
		t.Fatalf("tool catalog round-trip 后形状变了")
	}
	for _, descriptor := range descriptors {
		if !descriptor.ReadOnly {
			t.Fatalf("%s 必须是 read_only", descriptor.Key)
		}
		if !descriptor.Audit.LogInvocation || !descriptor.Audit.LogResultMetadata {
			t.Fatalf("%s 必须开启调用与结果元数据审计", descriptor.Key)
		}
		if !descriptor.Redaction.DenyRawSecrets {
			t.Fatalf("%s 必须拒绝原始密钥", descriptor.Key)
		}
	}
}

// Clone 必须是深拷贝：Registry 交出去的 Profile 被 handler 改写会污染共享配置。
func TestProfileCloneIsDeep(t *testing.T) {
	var profile Profile
	roundTrip(t, "e2b.profile.json", &profile)
	clone := profile.Clone()

	clone.ServiceRef.RootEntityIDs[0] = "tampered"
	clone.FocusKinds[0].Key = "tampered"
	clone.Journeys[0].Stages[0].Label = "tampered"
	if len(clone.Journeys[0].Stages[0].EntityRefs) > 0 {
		clone.Journeys[0].Stages[0].EntityRefs[0] = "tampered"
	}
	clone.Views[0].SignalRefs[0] = "tampered"
	clone.SignalBindings[0].Key = "tampered"
	if clone.SignalBindings[0].Thresholds != nil {
		clone.SignalBindings[0].Thresholds.Critical = -1
	}
	clone.ToolRefs[0] = "tampered"

	if profile.ServiceRef.RootEntityIDs[0] == "tampered" ||
		profile.FocusKinds[0].Key == "tampered" ||
		profile.Journeys[0].Stages[0].Label == "tampered" ||
		profile.Journeys[0].Stages[0].EntityRefs[0] == "tampered" ||
		profile.Views[0].SignalRefs[0] == "tampered" ||
		profile.SignalBindings[0].Key == "tampered" ||
		profile.SignalBindings[0].Thresholds.Critical == -1 ||
		profile.ToolRefs[0] == "tampered" {
		t.Fatalf("Clone 不是深拷贝，原 Profile 被改写")
	}
}

func TestToolDescriptorCloneIsDeep(t *testing.T) {
	raw := loadFixture(t, "tool-catalog.json")
	var descriptors []ToolDescriptor
	if err := json.Unmarshal(raw, &descriptors); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	original := descriptors[0]
	clone := original.Clone()
	clone.RequiredPermissions[0] = "tampered"
	clone.InputSchema["type"] = "tampered"
	nested, ok := clone.InputSchema["properties"].(map[string]any)
	if ok {
		for key := range nested {
			nested[key] = "tampered"
			break
		}
	}
	if original.RequiredPermissions[0] == "tampered" || original.InputSchema["type"] == "tampered" {
		t.Fatalf("ToolDescriptor.Clone 不是深拷贝")
	}
	originalNested, _ := original.InputSchema["properties"].(map[string]any)
	for _, value := range originalNested {
		if value == "tampered" {
			t.Fatalf("ToolDescriptor.Clone 未深拷贝嵌套 schema")
		}
	}
}

// required 数组必须序列化成 []。这条是硬约束：null 会让前端解构直接抛异常。
func TestNormalizeNeverEmitsNullForRequiredArrays(t *testing.T) {
	cases := []struct {
		name      string
		normalize func() any
		required  []string
	}{
		{
			name: "profile",
			normalize: func() any {
				p := &Profile{SchemaVersion: SchemaVersion}
				p.Normalize()
				return p
			},
			// root_entity_ids 嵌在 required 的 service_ref 里，同样是 required 数组，
			// 因此序列化形状必须是 [] 而不是 null。
			required: []string{"root_entity_ids", "focus_kinds", "journeys", "views",
				"signal_bindings", "tool_refs"},
		},
		{
			name: "diagnostic_context",
			normalize: func() any {
				c := &DiagnosticContext{SchemaVersion: SchemaVersion}
				c.Normalize()
				return c
			},
			required: []string{"team_ids", "roles", "root_entity_ids", "nodes", "relations",
				"signals", "evidence_refs", "alerts", "faults", "impacts", "incidents",
				"allowed_tool_keys", "redacted_fields"},
		},
		{
			name: "directory_view",
			normalize: func() any {
				v := &DirectoryView{SchemaVersion: SchemaVersion, ViewKind: ViewKindServiceDirectory}
				v.Normalize()
				return v
			},
			required: []string{"items"},
		},
		{
			name: "overview_view",
			normalize: func() any {
				v := &OverviewView{SchemaVersion: SchemaVersion, ViewKind: ViewKindServiceOverview}
				v.Normalize()
				return v
			},
			required: []string{"reason_codes", "sources", "golden_signals", "faults",
				"dependencies", "recent_events", "focus_kinds", "links", "sections"},
		},
		{
			name: "workspace_view",
			normalize: func() any {
				v := &WorkspaceView{SchemaVersion: SchemaVersion, ViewKind: ViewKindObservabilityWorkspace}
				v.Normalize()
				return v
			},
			required: []string{"reason_codes", "sources", "golden_signals", "journey", "nodes",
				"relations", "statements", "evidence_ids", "missing_requirements",
				"evidence_refs", "alerts", "faults", "impacts", "incidents", "timeline", "sections"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.normalize())
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}
			for _, field := range tc.required {
				if strings.Contains(string(encoded), `"`+field+`":null`) {
					t.Fatalf("%s 序列化成 null，必须是 []: %s", field, encoded)
				}
				if !strings.Contains(string(encoded), `"`+field+`":[]`) {
					t.Fatalf("%s 应出现为空数组: %s", field, encoded)
				}
			}
		})
	}
}

// root_entity_ids 是 required 数组，所以序列化形状必须是 []；但它同时带 minItems=1，
// 所以空数组依然是非法 Profile。这两件事必须同时成立：Normalize 只负责修正形状，绝不是
// 把"没有 root entity"变成合法配置——那种 Profile 仍然会被 v1 schema 拒绝并 quarantine。
func TestProfileNormalizeFixesShapeWithoutRelaxingMinItems(t *testing.T) {
	profile := &Profile{SchemaVersion: SchemaVersion}
	profile.Normalize()

	if profile.ServiceRef.RootEntityIDs == nil {
		t.Fatalf("Normalize 后 root_entity_ids 不能是 nil slice")
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), `"root_entity_ids":null`) {
		t.Fatalf("root_entity_ids 序列化成 null，会让页面在解构时崩溃: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"root_entity_ids":[]`) {
		t.Fatalf("root_entity_ids 应序列化成空数组: %s", encoded)
	}

	// 直接读 canonical schema，确认 minItems 语义没有被这次修正顺手放宽。
	raw, err := os.ReadFile(filepath.Join(examplesDir, "..", "service-observability-profile.schema.json"))
	if err != nil {
		t.Fatalf("读取 canonical profile schema 失败: %v", err)
	}
	// service_ref 是顶层 properties 里的内联定义，不在 $defs 里。
	var schema struct {
		Properties struct {
			ServiceRef struct {
				Required   []string `json:"required"`
				Properties struct {
					RootEntityIDs struct {
						MinItems *int `json:"minItems"`
					} `json:"root_entity_ids"`
				} `json:"properties"`
			} `json:"service_ref"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("解析 canonical profile schema 失败: %v", err)
	}
	serviceRef := schema.Properties.ServiceRef
	if !containsString(serviceRef.Required, "root_entity_ids") {
		t.Fatalf("canonical schema 里 root_entity_ids 应为 required: %v", serviceRef.Required)
	}
	minItems := serviceRef.Properties.RootEntityIDs.MinItems
	if minItems == nil {
		t.Fatalf("canonical schema 的 root_entity_ids 应仍带 minItems=1，现在完全没有该关键字")
	}
	if *minItems != 1 {
		t.Fatalf("canonical schema 的 root_entity_ids.minItems 应仍为 1, got %d", *minItems)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// nullable 字段必须真的输出 null，而不是被 omitempty 吞掉：契约里 focus/profile/
// next_cursor/value 的 null 是有语义的（无聚焦、未配置 Profile、无下一页、缺数）。
func TestNullableFieldsSerializeAsNull(t *testing.T) {
	workspace := &WorkspaceView{SchemaVersion: SchemaVersion, ViewKind: ViewKindObservabilityWorkspace}
	workspace.Normalize()
	encoded, err := json.Marshal(workspace)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{"focus", "profile"} {
		if !strings.Contains(string(encoded), `"`+field+`":null`) {
			t.Fatalf("%s 应输出 null: %s", field, encoded)
		}
	}

	directory := &DirectoryView{SchemaVersion: SchemaVersion, ViewKind: ViewKindServiceDirectory}
	directory.Normalize()
	encoded, err = json.Marshal(directory)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"next_cursor":null`) {
		t.Fatalf("next_cursor 应输出 null: %s", encoded)
	}

	signal := SignalValue{BindingKey: "x", Availability: AvailabilityUnavailable, Health: SignalHealthUnknown}
	encoded, err = json.Marshal(signal)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"value":null`) {
		t.Fatalf("缺数 signal 的 value 应输出 null: %s", encoded)
	}
}

// AllowedSemantics 必须与 profile schema 的 family/semantic 分支一致，Task 3 的引用
// 完整性校验直接依赖它。
func TestAllowedSemanticsMatchesContract(t *testing.T) {
	cases := map[SignalFamily][]SignalSemantic{
		SignalFamilyGolden: {SemanticLatency, SemanticTraffic, SemanticErrors, SemanticSaturation},
		SignalFamilyRED:    {SemanticRate, SemanticErrors, SemanticDuration},
		SignalFamilyUSE:    {SemanticUtilization, SemanticSaturation, SemanticErrors},
	}
	for family, want := range cases {
		got := AllowedSemantics(family)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("family=%s allowed=%v want %v", family, got, want)
		}
	}
	if AllowedSemantics("nope") != nil {
		t.Fatalf("未知 family 应返回 nil")
	}
}

// Timestamp 是字符串镜像：契约格式进出必须一致，且能解析成 time.Time 供计算使用。
func TestTimestampRoundTrip(t *testing.T) {
	const raw Timestamp = "2026-08-22T02:09:22Z"
	parsed, err := raw.Time()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if NewTimestamp(parsed) != raw {
		t.Fatalf("round-trip=%q want %q", NewTimestamp(parsed), raw)
	}
	if _, err := Timestamp("not-a-time").Time(); err == nil {
		t.Fatalf("非法时间必须报错")
	}
}
