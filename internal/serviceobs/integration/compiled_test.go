package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ---- 双产物原子性与配对 ----

// 一次编译必须原子给出两份产物，且 CompiledIntegration 真的承载 adapter 需要的 typed binding。
//
// 这正是 correction 要修的 P0：修复前 compiler 只输出 Profile，template_ref 与 params 在编译
// 之后蒸发，Task 4/5 的 adapter 拿不到任何可执行绑定。
func TestCompileProducesBothArtifacts(t *testing.T) {
	compiled, err := newCompiler(t, e2bResolver()).Compile(
		context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if len(compiled.ProfileJSON) == 0 || len(compiled.IntegrationJSON) == 0 {
		t.Fatal("两份产物必须同时存在")
	}
	if compiled.Manifest.ProfileDigest == "" || compiled.Manifest.IntegrationDigest == "" {
		t.Fatal("manifest 必须同时记录两个摘要")
	}
	if compiled.Manifest.ProfileDigest == compiled.Manifest.IntegrationDigest {
		t.Fatal("两个摘要不该相同")
	}
	if compiled.Integration.ProfileDigest != compiled.Manifest.ProfileDigest {
		t.Fatal("CompiledIntegration 必须记录配对的 ProfileDigest")
	}

	// server-only 产物必须真的带上 adapter 执行所需的一切。
	if len(compiled.Integration.Bindings) != 10 {
		t.Fatalf("binding 数=%d，E2B fixture 有 8 signal + 1 journey + 1 evidence",
			len(compiled.Integration.Bindings))
	}
	byKind := map[BindingKind]int{}
	for _, binding := range compiled.Integration.Bindings {
		byKind[binding.Kind]++
		if binding.TemplateRef == "" || binding.ToolKey == "" {
			t.Fatalf("binding %s 缺少 template/tool", binding.Key)
		}
		if binding.SourceKey == "" || binding.Capability == "" || binding.RequiredPermission == "" {
			t.Fatalf("binding %s 缺少 registry 推导事实: %+v", binding.Key, binding)
		}
	}
	if byKind[KindSignal] != 8 || byKind[KindJourney] != 1 || byKind[KindEvidence] != 1 {
		t.Fatalf("kind 分布不对: %v", byKind)
	}

	// typed params 必须完整保留：这是修复前丢失的东西。
	var latency CompiledBinding
	for _, binding := range compiled.Integration.Bindings {
		if binding.TargetRef == "golden_create_latency" {
			latency = binding
		}
	}
	if latency.Params.BucketMetric == "" || latency.Params.Quantile == nil || latency.Params.RangeSeconds == nil {
		t.Fatalf("signal binding 的 typed params 丢失: %+v", latency.Params)
	}
	if len(latency.ResolvedTargetIDs) != 0 {
		// scope=service 没有 canonical ref，应是空数组而不是 null。
		t.Fatalf("service scope 不该有 resolved target: %v", latency.ResolvedTargetIDs)
	}

	// relation scope 的 signal 必须落成真实 relation ID。
	var redRate CompiledBinding
	for _, binding := range compiled.Integration.Bindings {
		if binding.TargetRef == "red_api_orchestrator_rate" {
			redRate = binding
		}
	}
	if len(redRate.ResolvedTargetIDs) != 1 ||
		redRate.ResolvedTargetIDs[0] != "77777777-7777-4777-8777-777777777777" {
		t.Fatalf("relation scope 应落成真实 relation ID，得到 %v", redRate.ResolvedTargetIDs)
	}

	// journey binding 的 side outputs 必须来自 registry，且包含 topology hints。
	var journey CompiledBinding
	for _, binding := range compiled.Integration.Bindings {
		if binding.Kind == KindJourney {
			journey = binding
		}
	}
	if len(journey.ResolvedTargetIDs) == 0 {
		t.Fatal("journey binding 应带上 stage 的真实 canonical ID")
	}
	hasTopology := false
	for _, side := range journey.SideOutputs {
		if side == SideOutputTopologyHints {
			hasTopology = true
		}
	}
	if !hasTopology {
		t.Fatalf("journey 的 trace 读取必须声明 topology hints side output: %v", journey.SideOutputs)
	}

	// 自检：编译器产出的激活单元必须能过加载器校验。
	if err := ValidateBundle(compiled.Bundle()); err != nil {
		t.Fatalf("编译产物应通过激活单元校验: %v", err)
	}
}

// 两份产物都必须逐字节确定：摘要才有审计意义。
func TestBothArtifactsAreDeterministic(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	first, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: raw})
	if err != nil {
		t.Fatalf("第一次编译失败: %v", err)
	}
	second, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: raw})
	if err != nil {
		t.Fatalf("第二次编译失败: %v", err)
	}
	if string(first.IntegrationJSON) != string(second.IntegrationJSON) {
		t.Fatal("CompiledIntegration 字节不确定")
	}
	if first.Manifest.IntegrationDigest != second.Manifest.IntegrationDigest {
		t.Fatal("IntegrationDigest 不确定")
	}
	if string(first.ProfileJSON) != string(second.ProfileJSON) {
		t.Fatal("Profile 字节不确定")
	}
}

// 缺任一半、被篡改或摘要错配都必须 fail closed。
func TestBundleValidationFailsClosed(t *testing.T) {
	compiled, err := newCompiler(t, e2bResolver()).Compile(
		context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	good := compiled.Bundle()

	cases := []struct {
		name  string
		build func() Bundle
		want  error
	}{
		{
			name: "缺 Profile",
			build: func() Bundle {
				broken := good
				broken.Profile = nil
				return broken
			},
			want: ErrBundleProfileMissing,
		},
		{
			name: "缺 CompiledIntegration",
			build: func() Bundle {
				broken := good
				broken.Integration = nil
				return broken
			},
			want: ErrBundleIntegrationMissing,
		},
		{
			name: "Profile 被篡改",
			build: func() Bundle {
				broken := good
				broken.Profile = append(append([]byte{}, good.Profile...), ' ')
				return broken
			},
			want: ErrBundleProfileDigestMismatch,
		},
		{
			name: "CompiledIntegration 被篡改",
			build: func() Bundle {
				broken := good
				broken.Integration = append(append([]byte{}, good.Integration...), ' ')
				return broken
			},
			want: ErrBundleIntegrationDigestMismatch,
		},
		{
			name: "manifest 记的 Profile 摘要不对",
			build: func() Bundle {
				broken := good
				broken.Manifest.ProfileDigest = "sha256:" + strings.Repeat("0", 64)
				return broken
			},
			want: ErrBundleProfileDigestMismatch,
		},
		{
			name: "manifest 记的 Integration 摘要不对",
			build: func() Bundle {
				broken := good
				broken.Manifest.IntegrationDigest = "sha256:" + strings.Repeat("0", 64)
				return broken
			},
			want: ErrBundleIntegrationDigestMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBundle(tc.build()); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
		})
	}

	// 正向控制。
	if err := ValidateBundle(good); err != nil {
		t.Fatalf("完整激活单元应通过: %v", err)
	}
}

// 混用一新一旧：两份产物各自摘要都对得上 manifest，但 CompiledIntegration 指向另一个版本的
// Profile。这是 quarantine 必须抓住的形状——单看任何一半都发现不了。
func TestMixedVersionPairIsRejected(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")
	first, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: raw})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	// 造一个版本不同的 Profile 产物：改 package_version 会同时改 profile_version 与摘要。
	bumped := mutate(t, raw, func(doc map[string]any) { doc["package_version"] = float64(2) })
	second, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: bumped})
	if err != nil {
		t.Fatalf("编译 v2 失败: %v", err)
	}

	// 把 v2 的 Profile 与 v1 的 Integration 拼在一起，并把 manifest 的摘要都改成"看起来对"。
	mixed := Bundle{
		Manifest:    second.Manifest,
		Profile:     second.ProfileJSON,
		Integration: first.IntegrationJSON,
	}
	mixed.Manifest.IntegrationDigest = first.Manifest.IntegrationDigest
	if err := ValidateBundle(mixed); !errors.Is(err, ErrBundlePairBroken) {
		t.Fatalf("err=%v，want ErrBundlePairBroken", err)
	}
}

// CompiledIntegration 里绝不能出现禁止内容，即使编译路径将来被改动。
func TestBundleRejectsForbiddenContentInIntegration(t *testing.T) {
	compiled, err := newCompiler(t, e2bResolver()).Compile(
		context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(compiled.IntegrationJSON, &generic); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 模拟"将来有人往运行时产物里加了一个装 endpoint 的字段"。
	generic["endpoint"] = "https://metrics.internal"
	tampered, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	bundle := compiled.Bundle()
	bundle.Integration = tampered
	bundle.Manifest.IntegrationDigest = digest(tampered)
	if err := ValidateBundle(bundle); !errors.Is(err, ErrBundleForbiddenContent) {
		t.Fatalf("err=%v，want ErrBundleForbiddenContent", err)
	}
}

// 正常编译出来的 CompiledIntegration 本身不含任何禁止键。
func TestCompiledIntegrationHasNoForbiddenKeys(t *testing.T) {
	for _, name := range []string{"e2b.package.json", "postgresql.package.json"} {
		t.Run(name, func(t *testing.T) {
			resolver := e2bResolver()
			if name == "postgresql.package.json" {
				resolver = postgresResolver()
			}
			compiled, err := newCompiler(t, resolver).Compile(
				context.Background(), CompileRequest{Raw: readPackage(t, name)})
			if err != nil {
				t.Fatalf("编译失败: %v", err)
			}
			var generic any
			if err := json.Unmarshal(compiled.IntegrationJSON, &generic); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if path, found := scanForbiddenKeys(generic, ""); found {
				t.Fatalf("CompiledIntegration 含禁止键 %s", path)
			}
			// 也不该出现任何查询文本片段。
			text := string(compiled.IntegrationJSON)
			for _, forbidden := range []string{"rate(", "sum(", "SELECT ", "histogram_quantile(", "Bearer "} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("CompiledIntegration 含查询/凭据片段 %q", forbidden)
				}
			}
		})
	}
}

// 回滚引用是**一对**：只回退一半就等于混用版本。
func TestRollbackReferencesArePaired(t *testing.T) {
	compiled, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{
		Raw:                       readPackage(t, "e2b.package.json"),
		PreviousProfileDigest:     "sha256:" + strings.Repeat("a", 64),
		PreviousIntegrationDigest: "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if compiled.Manifest.PreviousProfileDigest == "" || compiled.Manifest.PreviousIntegrationDigest == "" {
		t.Fatal("两个回滚引用都必须保留")
	}
	if compiled.Manifest.PreviousProfileDigest == compiled.Manifest.PreviousIntegrationDigest {
		t.Fatal("两个回滚引用不该相同")
	}
}

// ---- ratio 退化与 label 分组 ----

func TestRatioSelectorRules(t *testing.T) {
	raw := readPackage(t, "e2b.package.json")

	ratioBinding := func(t *testing.T, doc map[string]any) map[string]any {
		return bindingAt(t, doc, byTemplate("counter_ratio_percent.v1"))
	}

	t.Run("同 metric 同选择器被拒绝", func(t *testing.T) {
		mutated := mutate(t, raw, func(doc map[string]any) {
			params := ratioBinding(t, doc)["params"].(map[string]any)
			// 去掉分子的差异化 label：分子与分母因此完全相同，比率恒 100%。
			delete(params, "numerator_labels")
		})
		_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
		if code := ReasonCodeOf(err); code != ReasonRatioDegenerate {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonRatioDegenerate)
		}
	})

	t.Run("common 与 numerator 重名被拒绝", func(t *testing.T) {
		mutated := mutate(t, raw, func(doc map[string]any) {
			params := ratioBinding(t, doc)["params"].(map[string]any)
			common := params["common_labels"].([]any)
			name := common[0].(map[string]any)["name"]
			params["numerator_labels"] = []any{
				map[string]any{"name": name, "value": "something-else"},
			}
		})
		_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
		if code := ReasonCodeOf(err); code != ReasonTemplateParamInvalid {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonTemplateParamInvalid)
		}
	})

	t.Run("denominator 侧差异化也合法", func(t *testing.T) {
		mutated := mutate(t, raw, func(doc map[string]any) {
			params := ratioBinding(t, doc)["params"].(map[string]any)
			delete(params, "numerator_labels")
			params["denominator_labels"] = []any{
				map[string]any{"name": "status_code", "value": "STATUS_CODE_UNSET"},
			}
		})
		if _, err := newCompiler(t, e2bResolver()).Compile(
			context.Background(), CompileRequest{Raw: mutated}); err != nil {
			t.Fatalf("单侧差异化选择器应合法: %v", err)
		}
	})

	t.Run("不同 metric 即使选择器相同也合法", func(t *testing.T) {
		mutated := mutate(t, raw, func(doc map[string]any) {
			params := ratioBinding(t, doc)["params"].(map[string]any)
			delete(params, "numerator_labels")
			params["numerator_metric"] = "traces_spanmetrics_errors_total"
		})
		if _, err := newCompiler(t, e2bResolver()).Compile(
			context.Background(), CompileRequest{Raw: mutated}); err != nil {
			t.Fatalf("不同 metric 应合法: %v", err)
		}
	})

	t.Run("非比率模板不接受比率分组", func(t *testing.T) {
		mutated := mutate(t, raw, func(doc map[string]any) {
			binding := bindingAt(t, doc, byTemplate("counter_rate.v1"))
			binding["params"].(map[string]any)["numerator_labels"] = []any{
				map[string]any{"name": "status_code", "value": "STATUS_CODE_ERROR"},
			}
		})
		_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
		if code := ReasonCodeOf(err); code != ReasonTemplateParamInvalid {
			t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonTemplateParamInvalid)
		}
	})
}

// registry 的新字段必须自洽：每个模板都要有 allowed kinds 与 source key，
// evidence 能力的模板必须声明 class。
func TestTemplateKindAndSourceInvariants(t *testing.T) {
	sourceKeys := map[string]struct{}{
		SourceKeyPrometheus:        {},
		SourceKeyOpenObserveLogs:   {},
		SourceKeyOpenObserveTraces: {},
		SourceKeyOpenObserveAccess: {},
	}
	for _, ref := range TemplateRefs() {
		template, _ := LookupTemplate(ref)
		if len(template.AllowedKinds) == 0 {
			t.Fatalf("%s 没有声明 allowed kinds", ref)
		}
		if _, ok := sourceKeys[template.SourceKey]; !ok {
			t.Fatalf("%s 的 source key=%q 不在稳定集合内", ref, template.SourceKey)
		}
		if template.AllowsKind(KindEvidence) && template.EvidenceClass == "" {
			t.Fatalf("%s 可充当 evidence 但没声明 class", ref)
		}
		if !template.AllowsKind(KindEvidence) && template.EvidenceClass != "" {
			t.Fatalf("%s 不能充当 evidence 却声明了 class", ref)
		}
		// prometheus 模板只服务 signal；openobserve 模板不该服务 signal。
		switch template.Capability {
		case CapabilityPrometheus:
			if !template.AllowsKind(KindSignal) || len(template.AllowedKinds) != 1 {
				t.Fatalf("%s 是 prometheus 模板，应只服务 signal: %v", ref, template.AllowedKinds)
			}
		case CapabilityOpenObserve:
			if template.AllowsKind(KindSignal) {
				t.Fatalf("%s 是 openobserve 模板，不应服务 signal", ref)
			}
		}
	}
}
