package integration

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/service-integration-package.schema.json
var schemaFS embed.FS

const (
	schemaFileName = "schemas/service-integration-package.schema.json"
	// SchemaVersion 是本包能编译的唯一 SIP 契约版本。
	SchemaVersion = "1.0"
	// CompilerVersion 是当前 compiler 的能力版本。package 声明的 min_compiler_version
	// 高于它时必须拒绝编译，而不是"尽力"编出一个丢掉约束的产物。
	CompilerVersion = 1
	// MaxDocumentBytes 是单份 package 文档的大小上界。它先于 JSON 解析生效，避免一份
	// 巨大文档在校验阶段吃掉内存。
	MaxDocumentBytes = 512 * 1024
)

// 稳定 reason code。它们进编译报告与发布 gate，只能增加不能改写。
const (
	ReasonDocumentTooLarge       = "sip_document_too_large"
	ReasonInvalidJSON            = "sip_invalid_json"
	ReasonUnknownField           = "sip_unknown_field"
	ReasonSchemaInvalid          = "sip_schema_invalid"
	ReasonForbiddenKey           = "sip_forbidden_key"
	ReasonSchemaVersion          = "sip_schema_version_unsupported"
	ReasonCompilerVersion        = "sip_compiler_version_unsupported"
	ReasonDuplicateKey           = "sip_duplicate_key"
	ReasonDanglingRef            = "sip_dangling_ref"
	ReasonUnknownTemplate        = "sip_unknown_template"
	ReasonTemplateParamInvalid   = "sip_template_param_invalid"
	ReasonTemplateOutputUnknown  = "sip_template_output_unknown"
	ReasonTemplateCapability     = "sip_template_capability_mismatch"
	ReasonBindingNotBound        = "sip_signal_binding_not_bound"
	ReasonBindingOrphan          = "sip_telemetry_binding_orphan"
	ReasonToolNotPublished       = "sip_tool_not_published"
	ReasonSelectorNotFound       = "sip_selector_not_found"
	ReasonSelectorAmbiguous      = "sip_selector_ambiguous"
	ReasonCompiledProfileInvalid = "sip_compiled_profile_invalid"
	ReasonInvalidFocusPattern    = "sip_invalid_focus_pattern"
)

// forbiddenKeys 是 package 里绝对不允许出现的键名。
//
// 这是纵深防御：schema 每一层都是 additionalProperties=false，所以正常情况下这些键已经
// 被 schema 拒绝。保留递归扫描是为了将来某个 schema 分支被放开（或新增未收紧的对象）时
// 仍然 fail closed。
//
// 只做精确键名匹配，不对合法说明文字做 substring 猜测：proxy_reason 与 description 里
// 出现"查询"两个字是完全正常的。注意 `pattern` 在黑名单里而 `value_pattern` 不在——
// 后者是 focus 输入校验用的正则，不参与任何查询构造。
var forbiddenKeys = map[string]struct{}{
	"query": {}, "queries": {}, "promql": {}, "sql": {}, "logql": {}, "expr": {},
	"expression": {}, "matcher": {}, "matchers": {}, "selector": {}, "regex": {},
	"regexp": {}, "pattern": {}, "filter": {}, "where": {}, "order_by": {},
	"shell": {}, "script": {}, "command": {}, "exec": {}, "executable": {},
	"code": {}, "javascript": {}, "wasm": {}, "html": {}, "template": {},
	"prompt": {}, "system_prompt": {}, "model": {},
	"secret": {}, "secrets": {}, "token": {}, "password": {}, "credential": {},
	"credentials": {}, "authorization": {}, "header": {}, "headers": {},
	"endpoint": {}, "endpoints": {}, "url": {}, "uri": {}, "host": {}, "hosts": {},
}

// Failure 是一次 package 校验/编译失败。它只带稳定 reason code 与定位信息，绝不带文档正文。
type Failure struct {
	ReasonCode string
	Detail     string
}

func (e *Failure) Error() string { return fmt.Sprintf("%s: %s", e.ReasonCode, e.Detail) }

func failf(reasonCode, format string, args ...any) *Failure {
	return &Failure{ReasonCode: reasonCode, Detail: fmt.Sprintf(format, args...)}
}

// ReasonCodeOf 提取 typed reason code，便于调用方与测试按 code 断言。
func ReasonCodeOf(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.ReasonCode
	}
	return ""
}

// EmbeddedSchema 返回嵌入的 canonical schema 原始字节，供 sync 测试与 docs 逐字节比对。
func EmbeddedSchema() ([]byte, error) { return schemaFS.ReadFile(schemaFileName) }

// offlineLoader 让任何未注册的 $ref 直接失败：校验行为只能依赖编入 binary 的 resource，
// 不能在运行时联网或读盘。
type offlineLoader struct{}

func (offlineLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("integration: 拒绝在运行时加载外部 schema %q；只允许 embedded resource", url)
}

// Validator 持有编译好的 SIP schema。零值不可用，必须经 NewValidator 构造。
type Validator struct {
	schema *jsonschema.Schema
}

// NewValidator 编译 embedded SIP schema。
func NewValidator() (*Validator, error) {
	raw, err := EmbeddedSchema()
	if err != nil {
		return nil, fmt.Errorf("integration: 读取 embedded SIP schema 失败: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("integration: 解析 embedded SIP schema 失败: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(offlineLoader{})
	const resource = "https://schemas.alert-system.invalid/service-integration-package/v1/service-integration-package.schema.json"
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("integration: 注册 SIP schema resource 失败: %w", err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("integration: 编译 SIP schema 失败: %w", err)
	}
	return &Validator{schema: compiled}, nil
}

// Decode 执行"大小上界 + unknown-field + schema + 递归禁止键"四重拒绝。
//
// 顺序是刻意的：先用 Go model 的 DisallowUnknownFields 拦掉超出契约的字段（错误信息里只
// 保留字段名），再过 schema 拿到精确的违规位置，最后扫禁止键。任何一步失败都返回 typed
// Failure，绝不返回一个"部分可用"的 package。
func (v *Validator) Decode(raw []byte) (Package, error) {
	if len(raw) > MaxDocumentBytes {
		return Package{}, failf(ReasonDocumentTooLarge, "文档超过 %d 字节上限", MaxDocumentBytes)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return Package{}, failf(ReasonInvalidJSON, "文档不是合法 JSON")
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var pkg Package
	if err := decoder.Decode(&pkg); err != nil {
		return Package{}, failf(ReasonUnknownField, "文档字段超出 SIP v1 契约: %s", unknownFieldDetail(err))
	}

	if err := v.schema.Validate(generic); err != nil {
		var validationErr *jsonschema.ValidationError
		if errors.As(err, &validationErr) {
			return Package{}, failf(ReasonSchemaInvalid, "违规位置 %s", strings.Join(instancePaths(validationErr), ","))
		}
		return Package{}, failf(ReasonSchemaInvalid, "schema 校验失败")
	}

	if path, found := scanForbiddenKeys(generic, ""); found {
		return Package{}, failf(ReasonForbiddenKey, "禁止的键 %s", path)
	}

	if pkg.SchemaVersion != SchemaVersion {
		return Package{}, failf(ReasonSchemaVersion, "schema_version=%q，只支持 %q", pkg.SchemaVersion, SchemaVersion)
	}
	if pkg.Compatibility.MinCompilerVersion > CompilerVersion {
		return Package{}, failf(ReasonCompilerVersion,
			"min_compiler_version=%d 高于当前 compiler 版本 %d",
			pkg.Compatibility.MinCompilerVersion, CompilerVersion)
	}
	return pkg, nil
}

// unknownFieldDetail 只保留 encoding/json 报出的字段名，不回显整段文档。
func unknownFieldDetail(err error) string {
	const marker = "unknown field "
	message := err.Error()
	if index := strings.Index(message, marker); index >= 0 {
		return message[index+len(marker):]
	}
	return "解码失败"
}

// instancePaths 收集稳定的 instance location，不输出文档内容。
func instancePaths(err *jsonschema.ValidationError) []string {
	seen := map[string]struct{}{}
	var walk func(*jsonschema.ValidationError)
	walk = func(node *jsonschema.ValidationError) {
		if node == nil {
			return
		}
		location := "/" + strings.Join(node.InstanceLocation, "/")
		if len(node.InstanceLocation) == 0 {
			location = "/"
		}
		seen[location] = struct{}{}
		for _, cause := range node.Causes {
			walk(cause)
		}
	}
	walk(err)
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// scanForbiddenKeys 递归遍历已解码的 JSON，返回第一个命中的 JSON Pointer 路径。
func scanForbiddenKeys(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			if _, forbidden := forbiddenKeys[strings.ToLower(key)]; forbidden {
				return childPath, true
			}
			if found, ok := scanForbiddenKeys(typed[key], childPath); ok {
				return found, true
			}
		}
	case []any:
		for i, item := range typed {
			if found, ok := scanForbiddenKeys(item, fmt.Sprintf("%s/%d", path, i)); ok {
				return found, true
			}
		}
	}
	return "", false
}
