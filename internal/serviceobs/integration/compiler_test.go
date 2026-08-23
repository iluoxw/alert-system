package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
	"alert-system/internal/serviceobs/tools"
)

// fakeResolver 按 entity_key 返回确定的真实 ID。
//
// 它刻意**不**含任何按名字猜测的兜底：未登记的 key 返回 not_found。compiler 因此没有
// "找不到就用 fixture ID"的路径可走。
type fakeResolver struct {
	environment string
	services    map[string]ServiceSelectorResolution
	entities    map[string]EntitySelectorResolution
	relations   map[string]RelationSelectorResolution
	// infraErr 非 nil 时模拟基础设施错误（DB 不可用），必须以 Go error 上抛。
	infraErr error
	calls    int
}

func (f *fakeResolver) ResolveServiceSelector(_ context.Context, environment, entityKey string) (ServiceSelectorResolution, error) {
	f.calls++
	if f.infraErr != nil {
		return ServiceSelectorResolution{}, f.infraErr
	}
	if environment != f.environment {
		return ServiceSelectorResolution{State: ResolutionNotFound}, nil
	}
	if resolution, ok := f.services[entityKey]; ok {
		return resolution, nil
	}
	return ServiceSelectorResolution{State: ResolutionNotFound}, nil
}

func (f *fakeResolver) ResolveEntitySelector(_ context.Context, environment, entityKey string) (EntitySelectorResolution, error) {
	f.calls++
	if f.infraErr != nil {
		return EntitySelectorResolution{}, f.infraErr
	}
	if environment != f.environment {
		return EntitySelectorResolution{State: ResolutionNotFound}, nil
	}
	if resolution, ok := f.entities[entityKey]; ok {
		return resolution, nil
	}
	return EntitySelectorResolution{State: ResolutionNotFound}, nil
}

func (f *fakeResolver) ResolveRelationSelector(_ context.Context, environment string, selector RelationSelector) (RelationSelectorResolution, error) {
	f.calls++
	if f.infraErr != nil {
		return RelationSelectorResolution{}, f.infraErr
	}
	if environment != f.environment {
		return RelationSelectorResolution{State: ResolutionNotFound}, nil
	}
	if resolution, ok := f.relations[relationKey(selector)]; ok {
		return resolution, nil
	}
	return RelationSelectorResolution{State: ResolutionNotFound}, nil
}

// e2bResolver 覆盖 E2B package 里出现的全部 selector。
func e2bResolver() *fakeResolver {
	return &fakeResolver{
		environment: "production",
		services: map[string]ServiceSelectorResolution{
			"platform:e2b": {
				State:       ResolutionFound,
				ServiceID:   "11111111-1111-4111-8111-111111111111",
				EntityID:    "22222222-2222-4222-8222-222222222222",
				Environment: "production",
			},
		},
		entities: map[string]EntitySelectorResolution{
			"platform:e2b:api":                {State: ResolutionFound, EntityID: "33333333-3333-4333-8333-333333333333", Environment: "production"},
			"platform:e2b:orchestrator":       {State: ResolutionFound, EntityID: "44444444-4444-4444-8444-444444444444", Environment: "production"},
			"infrastructure:e2b:sandbox-pool": {State: ResolutionFound, EntityID: "55555555-5555-4555-8555-555555555555", Environment: "production"},
			"infrastructure:e2b:host-fleet":   {State: ResolutionFound, EntityID: "66666666-6666-4666-8666-666666666666", Environment: "production"},
		},
		relations: map[string]RelationSelectorResolution{
			relationKey(RelationSelector{
				FromEntityKey: "platform:e2b:api", ToEntityKey: "platform:e2b:orchestrator", Kind: "depends_on",
			}): {State: ResolutionFound, RelationID: "77777777-7777-4777-8777-777777777777"},
		},
	}
}

func postgresResolver() *fakeResolver {
	return &fakeResolver{
		environment: "production",
		services: map[string]ServiceSelectorResolution{
			"infrastructure:postgresql-core": {
				State:       ResolutionFound,
				ServiceID:   "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				EntityID:    "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				Environment: "production",
			},
		},
		entities: map[string]EntitySelectorResolution{
			"infrastructure:postgresql-core:primary": {State: ResolutionFound, EntityID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Environment: "production"},
		},
		relations: map[string]RelationSelectorResolution{},
	}
}

func newCompiler(t *testing.T, resolver SelectorResolver) *Compiler {
	t.Helper()
	validator, err := NewValidator()
	if err != nil {
		t.Fatalf("构造 SIP validator 失败: %v", err)
	}
	contractValidator, err := contract.Default()
	if err != nil {
		t.Fatalf("构造 contract validator 失败: %v", err)
	}
	catalog, err := tools.Canonical()
	if err != nil {
		t.Fatalf("加载 tool catalog 失败: %v", err)
	}
	documents, err := profile.NewDocumentValidator(contractValidator, catalog)
	if err != nil {
		t.Fatalf("构造 Profile document validator 失败: %v", err)
	}
	return &Compiler{
		Validator: validator,
		Profiles:  documents,
		Resolver:  resolver,
		Now:       func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
}

func readPackage(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docsDir, "examples", name))
	if err != nil {
		t.Fatalf("读取 package fixture %s 失败: %v", name, err)
	}
	return raw
}

// mutate 把 package JSON 解成通用结构、交给回调改写、再序列化回去。
//
// 用它而不是手写整份非法 fixture：这样每个 negative case 只表达"相对合法文档改了哪一处"，
// 不会因为顺手写错别的字段而让测试因为错误的原因通过。
func mutate(t *testing.T, raw []byte, apply func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 package 失败: %v", err)
	}
	apply(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化 package 失败: %v", err)
	}
	return out
}

func profileTemplate(doc map[string]any) map[string]any {
	return doc["profile_template"].(map[string]any)
}

// ---- 正向：两份 example 都能编出合法 v1 Profile ----

func TestCompileE2BDraftPackage(t *testing.T) {
	resolver := e2bResolver()
	compiler := newCompiler(t, resolver)
	compiled, err := compiler.Compile(context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err != nil {
		t.Fatalf("编译 E2B package 失败: %v", err)
	}

	// service_ref 必须是解析出来的真实 ID，绝不能是 entity_key 或 fixture ID。
	if compiled.Profile.ServiceRef.ServiceID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("service_id=%q，必须是 resolver 返回的真实 ID", compiled.Profile.ServiceRef.ServiceID)
	}
	for _, root := range compiled.Profile.ServiceRef.RootEntityIDs {
		if strings.Contains(root, ":") {
			t.Fatalf("root_entity_ids 里出现了 entity_key 形状的值 %q", root)
		}
	}
	// draft package 只能编出 draft Profile：否则它会被 Registry 直接激活。
	if compiled.Profile.Status != model.ProfileStatusDraft {
		t.Fatalf("Profile status=%q，draft package 必须编出 draft", compiled.Profile.Status)
	}
	if compiled.Profile.ProfileVersion != 1 {
		t.Fatalf("profile_version=%d，应跟随 package_version", compiled.Profile.ProfileVersion)
	}
	// journey stage 的 refs 必须换成真实 relation/entity ID。
	var placeStage model.JourneyStageDef
	for _, stage := range compiled.Profile.Journeys[0].Stages {
		if stage.Key == "place" {
			placeStage = stage
		}
	}
	if len(placeStage.RelationRefs) != 1 || placeStage.RelationRefs[0] != "77777777-7777-4777-8777-777777777777" {
		t.Fatalf("place stage relation_refs=%v，必须是真实 relation ID", placeStage.RelationRefs)
	}
	// 摘要必须落在清单里，且与实际序列化字节一致。
	if compiled.Manifest.ProfileDigest != digest(compiled.ProfileJSON) {
		t.Fatal("ProfileDigest 与 ProfileJSON 不一致")
	}
	if !strings.HasPrefix(compiled.Manifest.ContentDigest, "sha256:") {
		t.Fatalf("ContentDigest=%q 形状不对", compiled.Manifest.ContentDigest)
	}
	if compiled.Manifest.PreviousProfileDigest != "" {
		t.Fatal("首个版本不应有 previous digest")
	}
	if len(compiled.Manifest.TemplateRefs) == 0 {
		t.Fatal("清单必须记录用到的 template refs")
	}
}

// 第二份 package 结构与 E2B 明显不同（没有 journey、没有 topology renderer），但必须通过
// 同一份契约与同一个 compiler。这是"契约不是为 E2B 定制"的可执行证据。
func TestCompileStructurallyDifferentSecondPackage(t *testing.T) {
	compiler := newCompiler(t, postgresResolver())
	compiled, err := compiler.Compile(context.Background(), CompileRequest{Raw: readPackage(t, "postgresql.package.json")})
	if err != nil {
		t.Fatalf("编译第二份 package 失败: %v", err)
	}
	if len(compiled.Profile.Journeys) != 0 {
		t.Fatalf("journeys=%d，第二份 package 刻意没有 journey", len(compiled.Profile.Journeys))
	}
	// 序列化形状必须是 []，不能是 null——契约要求 required 数组不为 null。
	if !strings.Contains(string(compiled.ProfileJSON), "\"journeys\": []") {
		t.Fatal("空 journeys 必须序列化成 []")
	}
	for _, view := range compiled.Profile.Views {
		if view.Renderer == model.RendererRequestJourney || view.Renderer == model.RendererDependencyTopology {
			t.Fatalf("第二份 package 不应出现 renderer=%s", view.Renderer)
		}
	}
}

// 回滚引用：上一版本摘要原样进清单，compiler 不改 Profile ID 语义。
func TestCompileCarriesPreviousDigestForRollback(t *testing.T) {
	compiler := newCompiler(t, e2bResolver())
	previous := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	compiled, err := compiler.Compile(context.Background(), CompileRequest{
		Raw:                   readPackage(t, "e2b.package.json"),
		PreviousProfileDigest: previous,
	})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if compiled.Manifest.PreviousProfileDigest != previous {
		t.Fatalf("PreviousProfileDigest=%q，应原样保留", compiled.Manifest.PreviousProfileDigest)
	}
}

// 同一份输入必须编出逐字节相同的产物：摘要才有审计意义。
func TestCompileIsDeterministic(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	first, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: raw})
	if err != nil {
		t.Fatalf("第一次编译失败: %v", err)
	}
	second, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: raw})
	if err != nil {
		t.Fatalf("第二次编译失败: %v", err)
	}
	if string(first.ProfileJSON) != string(second.ProfileJSON) {
		t.Fatal("同一输入两次编译的产物不一致")
	}
	if first.Manifest.ProfileDigest != second.Manifest.ProfileDigest {
		t.Fatal("同一输入两次编译的摘要不一致")
	}
}

// ---- 反向：selector 零命中 / 多命中 ----

func TestSelectorZeroAndMultipleHitsFailClosed(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")

	t.Run("service 零命中", func(t *testing.T) {
		resolver := e2bResolver()
		resolver.services = map[string]ServiceSelectorResolution{}
		_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: raw})
		if code := ReasonCodeOf(err); code != ReasonSelectorNotFound {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorNotFound)
		}
	})

	t.Run("service 多命中", func(t *testing.T) {
		resolver := e2bResolver()
		resolver.services["platform:e2b"] = ServiceSelectorResolution{State: ResolutionAmbiguous}
		_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: raw})
		if code := ReasonCodeOf(err); code != ReasonSelectorAmbiguous {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorAmbiguous)
		}
	})

	t.Run("entity 零命中", func(t *testing.T) {
		resolver := e2bResolver()
		delete(resolver.entities, "infrastructure:e2b:host-fleet")
		_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: raw})
		if code := ReasonCodeOf(err); code != ReasonSelectorNotFound {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorNotFound)
		}
	})

	t.Run("entity 多命中", func(t *testing.T) {
		resolver := e2bResolver()
		resolver.entities["platform:e2b:api"] = EntitySelectorResolution{State: ResolutionAmbiguous}
		_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: raw})
		if code := ReasonCodeOf(err); code != ReasonSelectorAmbiguous {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorAmbiguous)
		}
	})

	t.Run("relation 零命中", func(t *testing.T) {
		resolver := e2bResolver()
		resolver.relations = map[string]RelationSelectorResolution{}
		_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: raw})
		if code := ReasonCodeOf(err); code != ReasonSelectorNotFound {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorNotFound)
		}
	})

	t.Run("relation 多命中", func(t *testing.T) {
		resolver := e2bResolver()
		for key := range resolver.relations {
			resolver.relations[key] = RelationSelectorResolution{State: ResolutionAmbiguous}
		}
		_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: raw})
		if code := ReasonCodeOf(err); code != ReasonSelectorAmbiguous {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorAmbiguous)
		}
	})
}

// 基础设施错误绝不能被折叠成"package 非法"：那会让一次 DB 抖动变成"这个环境里没有这个服务"，
// 从而编出一个引用错误 ID 的 Profile。
func TestResolverInfrastructureErrorIsNotAPackageFailure(t *testing.T) {
	resolver := e2bResolver()
	sentinel := errors.New("database unavailable")
	resolver.infraErr = sentinel
	_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if code := ReasonCodeOf(err); code != "" {
		t.Fatalf("基础设施错误不应带 package reason code，得到 %q", code)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("应包装原始错误，得到 %v", err)
	}
}

// target.environment 与 registration 真实环境不一致时必须失败：否则会把另一个环境编进来。
func TestEnvironmentMismatchFailsClosed(t *testing.T) {
	resolver := e2bResolver()
	resolver.services["platform:e2b"] = ServiceSelectorResolution{
		State: ResolutionFound, ServiceID: "svc", EntityID: "ent", Environment: "staging",
	}
	_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if code := ReasonCodeOf(err); code != ReasonSelectorNotFound {
		t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonSelectorNotFound)
	}
}

// resolver 声称 found 但没给真实 ID 是实现 bug，必须当基础设施错误上抛而不是编出空 ID。
func TestFoundWithoutRealIDIsRejected(t *testing.T) {
	resolver := e2bResolver()
	resolver.services["platform:e2b"] = ServiceSelectorResolution{State: ResolutionFound, Environment: "production"}
	_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err == nil || !strings.Contains(err.Error(), "缺少真实 ID") {
		t.Fatalf("err=%v，应显式拒绝空 ID", err)
	}
}

// 没有任何 fixture ID 兜底路径：即使 package_key 叫 e2b、环境里存在一个 fixture 命名的服务，
// selector 不命中就必须失败，不能回落到 svc_e2b_fixture。
func TestNoFixtureServiceIDFallback(t *testing.T) {
	resolver := e2bResolver()
	resolver.services = map[string]ServiceSelectorResolution{
		// 环境里确实有一个 fixture 命名的服务，但它的 entity_key 不是 package 选的那个。
		"platform:e2b-fixture": {
			State: ResolutionFound, ServiceID: "svc_e2b_fixture",
			EntityID: "entity_e2b_api", Environment: "production",
		},
	}
	_, err := newCompiler(t, resolver).Compile(context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if code := ReasonCodeOf(err); code != ReasonSelectorNotFound {
		t.Fatalf("reason=%q err=%v，必须 fail closed 而不是回落 fixture ID", code, err)
	}
}

// ---- 反向：版本、模板、参数、引用 ----

func TestVersionGates(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	cases := []struct {
		name  string
		apply func(map[string]any)
		want  string
	}{
		{
			name:  "schema_version 不支持",
			apply: func(doc map[string]any) { doc["schema_version"] = "2.0" },
			// schema 的 const 会先拦下来，因此这里期望 schema_invalid。
			want: ReasonSchemaInvalid,
		},
		{
			name: "min_compiler_version 高于当前",
			apply: func(doc map[string]any) {
				doc["compatibility"].(map[string]any)["min_compiler_version"] = float64(CompilerVersion + 1)
			},
			want: ReasonCompilerVersion,
		},
		{
			name: "profile_schema_version 不是 1.0",
			apply: func(doc map[string]any) {
				doc["compatibility"].(map[string]any)["profile_schema_version"] = "1.1"
			},
			want: ReasonSchemaInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := mutate(t, raw, tc.apply)
			_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
			if code := ReasonCodeOf(err); code != tc.want {
				t.Fatalf("reason=%q err=%v，want %s", code, err, tc.want)
			}
		})
	}
}

func TestTemplateAndParamFailures(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	cases := []struct {
		name  string
		apply func(map[string]any)
		want  string
	}{
		{
			name: "未知 template ref",
			apply: func(doc map[string]any) {
				bindings := doc["telemetry_bindings"].([]any)
				bindings[0].(map[string]any)["template_ref"] = "not_registered.v1"
			},
			want: ReasonUnknownTemplate,
		},
		{
			name: "缺少必填参数",
			apply: func(doc map[string]any) {
				bindings := doc["telemetry_bindings"].([]any)
				params := bindings[0].(map[string]any)["params"].(map[string]any)
				delete(params, "quantile")
			},
			want: ReasonTemplateParamInvalid,
		},
		{
			name: "多传模板不接受的参数",
			apply: func(doc map[string]any) {
				bindings := doc["telemetry_bindings"].([]any)
				// counter_rate.v1 不接受 quantile。
				for _, item := range bindings {
					binding := item.(map[string]any)
					if binding["template_ref"] == "counter_rate.v1" {
						binding["params"].(map[string]any)["quantile"] = 0.9
						return
					}
				}
				t.Fatal("fixture 里应存在 counter_rate.v1 binding")
			},
			want: ReasonTemplateParamInvalid,
		},
		{
			name: "output_field 不是模板声明的输出",
			apply: func(doc map[string]any) {
				bindings := profileTemplate(doc)["signal_bindings"].([]any)
				bindings[0].(map[string]any)["output_field"] = "series.made_up_field"
			},
			want: ReasonTemplateOutputUnknown,
		},
		{
			name: "telemetry binding 没有对应 signal binding",
			apply: func(doc map[string]any) {
				bindings := doc["telemetry_bindings"].([]any)
				bindings[0].(map[string]any)["binding_key"] = "not_a_signal"
			},
			want: ReasonBindingOrphan,
		},
		{
			name: "signal binding 没有 telemetry binding",
			apply: func(doc map[string]any) {
				bindings := doc["telemetry_bindings"].([]any)
				doc["telemetry_bindings"] = bindings[1:]
			},
			want: ReasonBindingNotBound,
		},
		{
			name: "telemetry binding 重复",
			apply: func(doc map[string]any) {
				bindings := doc["telemetry_bindings"].([]any)
				doc["telemetry_bindings"] = append(bindings, bindings[0])
			},
			want: ReasonDuplicateKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := mutate(t, raw, tc.apply)
			_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
			if code := ReasonCodeOf(err); code != tc.want {
				t.Fatalf("reason=%q err=%v，want %s", code, err, tc.want)
			}
		})
	}
}

func TestStructuralFailures(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	cases := []struct {
		name  string
		apply func(map[string]any)
		want  string
	}{
		{
			name: "signal binding 引用未声明的 tool",
			apply: func(doc map[string]any) {
				bindings := profileTemplate(doc)["signal_bindings"].([]any)
				bindings[0].(map[string]any)["tool_key"] = "metrics.not_declared.v1"
			},
			want: ReasonDanglingRef,
		},
		{
			name: "view 引用未声明的 journey",
			apply: func(doc map[string]any) {
				views := profileTemplate(doc)["views"].([]any)
				views[1].(map[string]any)["journey_ref"] = "no_such_journey"
			},
			want: ReasonDanglingRef,
		},
		{
			name: "view order 冲突",
			apply: func(doc map[string]any) {
				views := profileTemplate(doc)["views"].([]any)
				views[1].(map[string]any)["order"] = views[0].(map[string]any)["order"]
			},
			want: ReasonDuplicateKey,
		},
		{
			name: "primary focus 不唯一",
			apply: func(doc map[string]any) {
				focuses := profileTemplate(doc)["focus_kinds"].([]any)
				focuses[1].(map[string]any)["primary"] = true
			},
			want: ReasonDanglingRef,
		},
		{
			name: "value_pattern 无法被 RE2 编译",
			apply: func(doc map[string]any) {
				focuses := profileTemplate(doc)["focus_kinds"].([]any)
				focuses[0].(map[string]any)["value_pattern"] = "^(unclosed"
			},
			want: ReasonInvalidFocusPattern,
		},
		{
			name: "tool_refs 引用未发布的 tool",
			apply: func(doc map[string]any) {
				template := profileTemplate(doc)
				refs := template["tool_refs"].([]any)
				template["tool_refs"] = append(refs, "metrics.unpublished_tool.v1")
			},
			want: ReasonToolNotPublished,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := mutate(t, raw, tc.apply)
			_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
			if code := ReasonCodeOf(err); code != tc.want {
				t.Fatalf("reason=%q err=%v，want %s", code, err, tc.want)
			}
		})
	}
}

// 编译产物必须真的过一遍现有 Profile 校验，而不是 compiler 自证合法。
// 这里去掉一个 golden binding：SIP 层面没有"必须四个 golden"的规则，但 Profile 校验有。
func TestCompiledProfileMustPassExistingProfileValidation(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	mutated := mutate(t, raw, func(doc map[string]any) {
		template := profileTemplate(doc)
		bindings := template["signal_bindings"].([]any)
		kept := make([]any, 0, len(bindings))
		var removedKey string
		for _, item := range bindings {
			binding := item.(map[string]any)
			if binding["key"] == "golden_create_traffic" {
				removedKey = binding["key"].(string)
				continue
			}
			kept = append(kept, binding)
		}
		template["signal_bindings"] = kept
		// 同步移除对应的 telemetry binding 与 view 引用，让失败原因只剩 golden 数量。
		telemetry := doc["telemetry_bindings"].([]any)
		keptTelemetry := make([]any, 0, len(telemetry))
		for _, item := range telemetry {
			if item.(map[string]any)["binding_key"] == removedKey {
				continue
			}
			keptTelemetry = append(keptTelemetry, item)
		}
		doc["telemetry_bindings"] = keptTelemetry
		views := template["views"].([]any)
		overview := views[0].(map[string]any)
		refs := overview["signal_refs"].([]any)
		keptRefs := make([]any, 0, len(refs))
		for _, ref := range refs {
			if ref == removedKey {
				continue
			}
			keptRefs = append(keptRefs, ref)
		}
		overview["signal_refs"] = keptRefs
	})
	_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
	if code := ReasonCodeOf(err); code != ReasonCompiledProfileInvalid {
		t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonCompiledProfileInvalid)
	}
}

// published package 编出 published Profile：门槛在"改 status"这一个显式动作上，
// 而不是在 compiler 里偷偷提升。
func TestPublishedPackageProducesPublishedProfile(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	mutated := mutate(t, raw, func(doc map[string]any) { doc["status"] = "published" })
	compiled, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if compiled.Profile.Status != model.ProfileStatusPublished {
		t.Fatalf("status=%q，published package 应编出 published Profile", compiled.Profile.Status)
	}
}

// 装配缺依赖是编程错误，必须显式失败而不是"尽力编译"。
func TestMissingDependenciesAreProgrammingErrors(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	full := newCompiler(t, e2bResolver())
	cases := map[string]*Compiler{
		"缺 SIP validator":     {Profiles: full.Profiles, Resolver: full.Resolver},
		"缺 Profile validator": {Validator: full.Validator, Resolver: full.Resolver},
		"缺 resolver":          {Validator: full.Validator, Profiles: full.Profiles},
	}
	for name, compiler := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := compiler.Compile(context.Background(), CompileRequest{Raw: raw}); err == nil {
				t.Fatal("应返回装配错误")
			}
		})
	}
}
