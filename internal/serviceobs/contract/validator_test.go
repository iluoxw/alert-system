// 离线 validator 的正例与负例。正例直接用 docs/contracts 下的 canonical fixture，
// 负例集中在两类真实风险上：Profile/ToolDescriptor 被塞进 query/secret/prompt/executable
// 字段，以及"缺数被伪装成绿值"（availability 非 available 却带数值或非 unknown 健康）。
package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const examplesDir = canonicalDir + "/examples"

func newValidator(t *testing.T) *Validator {
	t.Helper()
	validator, err := NewValidator()
	if err != nil {
		t.Fatalf("编译 embedded schema 失败: %v", err)
	}
	return validator
}

func loadExample(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(examplesDir, name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return raw
}

func loadExampleDoc(t *testing.T, name string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(loadExample(t, name), &doc); err != nil {
		t.Fatalf("解析 fixture %s 失败: %v", name, err)
	}
	return doc
}

// 五种文档的 canonical fixture 全部必须通过。这是 Go 侧校验与 python verifier 的共同基线。
func TestValidatorAcceptsCanonicalFixtures(t *testing.T) {
	validator := newValidator(t)
	cases := []struct {
		kind SchemaKind
		file string
	}{
		{KindProfile, "e2b.profile.json"},
		{KindDiagnosticContext, "e2b.context.json"},
		{KindPageView, "e2b.directory-view.json"},
		{KindPageView, "e2b.overview-view.json"},
		{KindPageView, "e2b.workspace-view.json"},
	}
	for _, tc := range cases {
		if err := validator.ValidateJSON(tc.kind, loadExample(t, tc.file)); err != nil {
			t.Fatalf("%s 应通过 %s 校验: %v", tc.file, tc.kind, err)
		}
	}

	var descriptors []any
	if err := json.Unmarshal(loadExample(t, "tool-catalog.json"), &descriptors); err != nil {
		t.Fatalf("解析 tool-catalog.json 失败: %v", err)
	}
	if len(descriptors) == 0 {
		t.Fatalf("tool catalog fixture 不应为空")
	}
	for i, descriptor := range descriptors {
		if err := validator.Validate(KindToolDescriptor, descriptor); err != nil {
			t.Fatalf("tool-catalog.json[%d] 应通过校验: %v", i, err)
		}
	}

	// EvidenceRef 只作为 Context 的子文档出现在 fixture 里；单独抽出来也必须自洽。
	context := loadExampleDoc(t, "e2b.context.json")
	refs, ok := context["evidence_refs"].([]any)
	if !ok || len(refs) == 0 {
		t.Fatalf("context fixture 应带 evidence_refs")
	}
	for i, ref := range refs {
		if err := validator.Validate(KindEvidenceRef, ref); err != nil {
			t.Fatalf("evidence_refs[%d] 应通过校验: %v", i, err)
		}
	}
}

// Profile 的 additionalProperties=false 是"禁止携带查询/凭据/prompt/可执行代码"的机器
// 可校验形式。顶层与 signal binding 两个层级都要拒绝。
func TestValidatorRejectsForbiddenProfileFields(t *testing.T) {
	validator := newValidator(t)
	forbidden := []string{"query", "promql", "sql", "logql", "shell", "secret", "token",
		"headers", "prompt", "html", "executable"}

	for _, key := range forbidden {
		profile := loadExampleDoc(t, "e2b.profile.json")
		profile[key] = "SELECT 1"
		if err := validator.Validate(KindProfile, profile); err == nil {
			t.Fatalf("Profile 顶层 %q 字段必须被拒绝", key)
		}

		nested := loadExampleDoc(t, "e2b.profile.json")
		bindings, _ := nested["signal_bindings"].([]any)
		if len(bindings) == 0 {
			t.Fatalf("profile fixture 应带 signal_bindings")
		}
		binding, _ := bindings[0].(map[string]any)
		binding[key] = "SELECT 1"
		if err := validator.Validate(KindProfile, nested); err == nil {
			t.Fatalf("signal_bindings[0] 的 %q 字段必须被拒绝", key)
		}
	}
}

func TestValidatorRejectsForbiddenToolDescriptorFields(t *testing.T) {
	validator := newValidator(t)
	var descriptors []map[string]any
	if err := json.Unmarshal(loadExample(t, "tool-catalog.json"), &descriptors); err != nil {
		t.Fatalf("解析 tool-catalog.json 失败: %v", err)
	}
	for _, key := range []string{"query", "promql", "sql", "secret", "prompt", "executable", "headers"} {
		descriptor := map[string]any{}
		for k, v := range descriptors[0] {
			descriptor[k] = v
		}
		descriptor[key] = "x"
		if err := validator.Validate(KindToolDescriptor, descriptor); err == nil {
			t.Fatalf("ToolDescriptor 的 %q 字段必须被拒绝", key)
		}
	}

	// read_only 是 const true：不存在"顺手改成写工具"的 descriptor。
	writable := map[string]any{}
	for k, v := range descriptors[0] {
		writable[k] = v
	}
	writable["read_only"] = false
	if err := validator.Validate(KindToolDescriptor, writable); err == nil {
		t.Fatalf("read_only=false 必须被拒绝")
	}
}

// 缺数不假绿：availability 非 available 时不允许带数值，也不允许非 unknown 的健康，
// 并且必须给出 unknown_reason。
func TestValidatorRejectsFakeGreenSignals(t *testing.T) {
	validator := newValidator(t)

	withValue := loadExampleDoc(t, "e2b.context.json")
	signals, _ := withValue["signals"].([]any)
	unavailable := findSignal(t, signals, "use_host_placement_saturation")
	unavailable["value"] = 12.0
	if err := validator.Validate(KindDiagnosticContext, withValue); err == nil {
		t.Fatalf("unavailable signal 带数值必须被拒绝")
	}

	withHealth := loadExampleDoc(t, "e2b.context.json")
	signals, _ = withHealth["signals"].([]any)
	unavailable = findSignal(t, signals, "use_host_placement_saturation")
	unavailable["health"] = "healthy"
	if err := validator.Validate(KindDiagnosticContext, withHealth); err == nil {
		t.Fatalf("unavailable signal 标 healthy 必须被拒绝")
	}

	withoutReason := loadExampleDoc(t, "e2b.context.json")
	signals, _ = withoutReason["signals"].([]any)
	unavailable = findSignal(t, signals, "use_host_placement_saturation")
	delete(unavailable, "unknown_reason")
	if err := validator.Validate(KindDiagnosticContext, withoutReason); err == nil {
		t.Fatalf("unavailable signal 缺 unknown_reason 必须被拒绝")
	}
}

// proxy 必须自证：没有 proxy_reason 的 proxy 信号会让页面把容量压力 proxy 说成真实饱和度。
func TestValidatorRejectsProxyWithoutReason(t *testing.T) {
	validator := newValidator(t)
	workspace := loadExampleDoc(t, "e2b.workspace-view.json")
	signals, _ := workspace["golden_signals"].([]any)
	found := false
	for _, item := range signals {
		signal, _ := item.(map[string]any)
		if signal["measurement_role"] == "proxy" {
			delete(signal, "proxy_reason")
			found = true
		}
	}
	if !found {
		t.Fatalf("workspace fixture 应至少带一个 proxy 信号")
	}
	if err := validator.Validate(KindPageView, workspace); err == nil {
		t.Fatalf("proxy 信号缺 proxy_reason 必须被拒绝")
	}
}

// expired 只属于 EvidenceRef。source/signal 的 availability 词表里没有它，误用必须被拒绝。
func TestValidatorRejectsExpiredOutsideEvidence(t *testing.T) {
	validator := newValidator(t)

	evidence := loadExampleDoc(t, "e2b.context.json")
	refs, _ := evidence["evidence_refs"].([]any)
	first, _ := refs[0].(map[string]any)
	first["availability"] = "expired"
	if err := validator.Validate(KindDiagnosticContext, evidence); err != nil {
		t.Fatalf("EvidenceRef availability=expired 必须被接受: %v", err)
	}

	contextSignal := loadExampleDoc(t, "e2b.context.json")
	signals, _ := contextSignal["signals"].([]any)
	target := findSignal(t, signals, "use_host_placement_saturation")
	target["availability"] = "expired"
	if err := validator.Validate(KindDiagnosticContext, contextSignal); err == nil {
		t.Fatalf("context signal availability=expired 必须被拒绝")
	}

	view := loadExampleDoc(t, "e2b.overview-view.json")
	dataStatus, _ := view["data_status"].(map[string]any)
	sources, _ := dataStatus["sources"].([]any)
	if len(sources) == 0 {
		t.Fatalf("overview fixture 应带 data_status.sources")
	}
	source, _ := sources[0].(map[string]any)
	source["state"] = "expired"
	if err := validator.Validate(KindPageView, view); err == nil {
		t.Fatalf("source state=expired 必须被拒绝")
	}
}

// 错误必须报出稳定 instance path，而不是把违规实例值喷进 error 或日志——Profile、
// Context 与 Evidence 都可能带业务标签和摘要，泄露出去等于绕过脱敏。
func TestValidationErrorPathsAreStableAndDocumentFree(t *testing.T) {
	validator := newValidator(t)
	profile := loadExampleDoc(t, "e2b.profile.json")
	bindings, _ := profile["signal_bindings"].([]any)
	binding, _ := bindings[0].(map[string]any)

	// label 超长会触发 maxLength，报错必须定位到 /signal_bindings/0/label 且不含值。
	oversized := strings.Repeat("x", 500)
	binding["label"] = oversized
	err := validator.Validate(KindProfile, profile)
	if err == nil {
		t.Fatalf("超长 label 必须被拒绝")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("应返回 *ValidationError, got %T", err)
	}
	if validationErr.Kind != KindProfile {
		t.Fatalf("Kind=%s, want %s", validationErr.Kind, KindProfile)
	}
	paths := validationErr.Paths()
	if len(paths) == 0 {
		t.Fatalf("应至少报出一处路径")
	}
	found := false
	for _, path := range paths {
		if path == "/signal_bindings/0/label" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应定位到 /signal_bindings/0/label, got %v", paths)
	}
	if strings.Contains(err.Error(), oversized) || strings.Contains(err.Error(), strings.Repeat("x", 40)) {
		t.Fatalf("错误消息不得包含实例值: %s", err.Error())
	}
}

// 未知 kind 走 typed 错误，调用方能区分"文档不合契约"与"用错了 API"。
func TestValidatorRejectsUnknownKind(t *testing.T) {
	validator := newValidator(t)
	err := validator.Validate("not_a_kind", map[string]any{})
	if !errors.Is(err, ErrUnknownSchemaKind) {
		t.Fatalf("应返回 ErrUnknownSchemaKind, got %v", err)
	}
}

// 运行时任何外部 $ref 加载都必须失败。这条保证部署环境没有网络时校验行为不变。
func TestOfflineLoaderRefusesRemoteRefs(t *testing.T) {
	if _, err := (offlineLoader{}).Load("https://json-schema.org/draft/2020-12/schema"); err == nil {
		t.Fatalf("offlineLoader 必须拒绝外部加载")
	}
	if _, err := (offlineLoader{}).Load("file:///etc/passwd"); err == nil {
		t.Fatalf("offlineLoader 必须拒绝文件系统加载")
	}
}

// compile once + 并发 Validate：这是 API handler 的实际使用形态。
func TestValidatorIsSafeForConcurrentUse(t *testing.T) {
	validator := newValidator(t)
	raw := loadExample(t, "e2b.workspace-view.json")
	profile := loadExample(t, "e2b.profile.json")

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := validator.ValidateJSON(KindPageView, raw); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if err := validator.ValidateJSON(KindProfile, profile); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发校验失败: %v", err)
	}
}

// Default() 只编译一次并可复用。
func TestDefaultValidatorIsShared(t *testing.T) {
	first, err := Default()
	if err != nil {
		t.Fatalf("Default 失败: %v", err)
	}
	second, err := Default()
	if err != nil {
		t.Fatalf("Default 二次调用失败: %v", err)
	}
	if first != second {
		t.Fatalf("Default 应返回同一实例")
	}
}

func findSignal(t *testing.T, signals []any, bindingKey string) map[string]any {
	t.Helper()
	for _, item := range signals {
		signal, _ := item.(map[string]any)
		if signal["binding_key"] == bindingKey {
			return signal
		}
	}
	t.Fatalf("fixture 里找不到 binding_key=%s", bindingKey)
	return nil
}
