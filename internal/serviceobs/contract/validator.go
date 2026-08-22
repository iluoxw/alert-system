// Package contract 提供 Service Observability contracts v1 的离线 schema 校验。
//
// 三条硬约束：
//
//  1. 五份 canonical schema 以 go:embed 编入 binary，编译期只注册这些本地 resource。
//     compiler 挂一个恒失败的 URLLoader，因此运行时既不会联网取 $ref，也不会读取
//     文件系统上的任意路径——部署环境不可用的网络不能变成校验行为的隐式依赖。
//  2. 校验器在启动时 compile 一次，之后可并发 Validate。jsonschema.Schema 编译后是
//     只读的，Validator 内部不持有可变状态。
//  3. 错误只输出稳定的 instance path + keyword path，不把完整文档写进 error 或日志。
//     Profile、Context 与 Evidence 里可能带业务标签与摘要，泄露到日志等于绕过脱敏。
package contract

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/*.json
var schemaFS embed.FS

// SchemaKind 枚举可校验的五种 v1 文档。
type SchemaKind string

const (
	KindProfile           SchemaKind = "profile"
	KindDiagnosticContext SchemaKind = "diagnostic_context"
	KindPageView          SchemaKind = "page_view"
	KindEvidenceRef       SchemaKind = "evidence_ref"
	KindToolDescriptor    SchemaKind = "tool_descriptor"
)

// schemaBaseURL 与 canonical schema 的 $id 前缀一致。相对 $ref（例如 Page View 引用
// evidence-ref.schema.json）按这个 base 解析到已注册的本地 resource。
const schemaBaseURL = "https://schemas.alert-system.invalid/service-observability/v1/"

var schemaFiles = map[SchemaKind]string{
	KindProfile:           "service-observability-profile.schema.json",
	KindDiagnosticContext: "diagnostic-context.schema.json",
	KindPageView:          "service-observability-page-view.schema.json",
	KindEvidenceRef:       "evidence-ref.schema.json",
	KindToolDescriptor:    "tool-descriptor.schema.json",
}

// SchemaKinds 返回稳定顺序的全部 kind，供测试与启动自检遍历。
func SchemaKinds() []SchemaKind {
	kinds := make([]SchemaKind, 0, len(schemaFiles))
	for kind := range schemaFiles {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// EmbeddedSchema 返回嵌入的 canonical schema 原始字节。schema_sync_test 用它与
// docs/contracts/service-observability/v1/ 逐字节比对，防止两份定义漂移。
func EmbeddedSchema(kind SchemaKind) ([]byte, error) {
	name, ok := schemaFiles[kind]
	if !ok {
		return nil, fmt.Errorf("contract: 未知 schema kind %q", kind)
	}
	return schemaFS.ReadFile("schemas/" + name)
}

// EmbeddedSchemaFileName 返回该 kind 对应的 canonical 文件名。
func EmbeddedSchemaFileName(kind SchemaKind) (string, bool) {
	name, ok := schemaFiles[kind]
	return name, ok
}

// offlineLoader 让任何未注册的 $ref 直接失败。默认 loader 会尝试按 file:// 读盘，
// 这里必须彻底关掉：校验行为只能依赖编入 binary 的 resource。
type offlineLoader struct{}

func (offlineLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("contract: 拒绝在运行时加载外部 schema %q；只允许 embedded resource", url)
}

// Validator 持有编译好的五份 schema。零值不可用，必须经 NewValidator 构造。
type Validator struct {
	compiled map[SchemaKind]*jsonschema.Schema
}

var (
	defaultOnce      sync.Once
	defaultValidator *Validator
	defaultErr       error
)

// Default 返回进程内共享的 Validator，只编译一次。适合 app 装配与只读校验路径。
func Default() (*Validator, error) {
	defaultOnce.Do(func() { defaultValidator, defaultErr = NewValidator() })
	return defaultValidator, defaultErr
}

// NewValidator 编译全部 embedded schema。任何一份编译失败都直接返回错误：带着
// 半个可用校验器启动，等于让非法 Profile 在请求时才炸。
func NewValidator() (*Validator, error) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(offlineLoader{})

	entries, err := fs.Glob(schemaFS, "schemas/*.json")
	if err != nil {
		return nil, fmt.Errorf("contract: 枚举 embedded schema 失败: %w", err)
	}
	sort.Strings(entries)
	for _, entry := range entries {
		raw, err := schemaFS.ReadFile(entry)
		if err != nil {
			return nil, fmt.Errorf("contract: 读取 embedded schema %s 失败: %w", entry, err)
		}
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("contract: 解析 embedded schema %s 失败: %w", entry, err)
		}
		name := strings.TrimPrefix(entry, "schemas/")
		if err := compiler.AddResource(schemaBaseURL+name, doc); err != nil {
			return nil, fmt.Errorf("contract: 注册 embedded schema %s 失败: %w", entry, err)
		}
	}

	compiled := make(map[SchemaKind]*jsonschema.Schema, len(schemaFiles))
	for kind, name := range schemaFiles {
		schema, err := compiler.Compile(schemaBaseURL + name)
		if err != nil {
			return nil, fmt.Errorf("contract: 编译 %s 失败: %w", name, err)
		}
		compiled[kind] = schema
	}
	return &Validator{compiled: compiled}, nil
}

// ValidationError 是稳定的校验失败结果：只带文档类型与 issue 列表，不带文档内容。
type ValidationError struct {
	Kind   SchemaKind
	Issues []Issue
}

// Issue 定位一处违规。InstancePath 是 JSON Pointer（文档内位置），KeywordPath 是
// 违反的 schema 关键字路径。两者都不含实例值。
type Issue struct {
	InstancePath string
	KeywordPath  string
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		path := issue.InstancePath
		if path == "" {
			path = "/"
		}
		parts = append(parts, path+" 违反 "+issue.KeywordPath)
	}
	return fmt.Sprintf("contract: %s 未通过 v1 schema 校验: %s", e.Kind, strings.Join(parts, "; "))
}

// Paths 返回全部违规位置，供 quarantine reason 与测试断言使用。
func (e *ValidationError) Paths() []string {
	paths := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		paths = append(paths, issue.InstancePath)
	}
	return paths
}

// ErrUnknownSchemaKind 在调用方传入未注册 kind 时返回。
var ErrUnknownSchemaKind = errors.New("contract: 未知 schema kind")

// Validate 校验一个已解码的 JSON 值（map[string]any / []any / 标量）。传入 Go 结构体
// 不会被正确校验，必须先经 json.Marshal + UnmarshalJSON 解码。
func (v *Validator) Validate(kind SchemaKind, document any) error {
	schema, ok := v.compiled[kind]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSchemaKind, kind)
	}
	err := schema.Validate(document)
	if err == nil {
		return nil
	}
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		return fmt.Errorf("contract: %s 校验失败: %w", kind, err)
	}
	return &ValidationError{Kind: kind, Issues: collectIssues(validationErr)}
}

// ValidateJSON 解码原始 JSON 后校验。解码使用 jsonschema.UnmarshalJSON，它把数字保留成
// json.Number，避免大整数被 float64 精度改写后误判 schema。
func (v *Validator) ValidateJSON(kind SchemaKind, raw []byte) error {
	document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("contract: %s 不是合法 JSON: %w", kind, err)
	}
	return v.Validate(kind, document)
}

// ValidateValue 把任意 Go 值（含 model 结构体）序列化后校验。它是 model 与 schema 之间
// 的桥：projector 输出可以直接在测试里过一遍真实契约。
func (v *Validator) ValidateValue(kind SchemaKind, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("contract: %s 序列化失败: %w", kind, err)
	}
	return v.ValidateJSON(kind, raw)
}

// collectIssues 把嵌套的 ValidationError 展平成叶子 issue，并按路径稳定排序去重。
// 只取叶子是因为中间节点（allOf/oneOf 容器）的路径没有诊断价值。
func collectIssues(root *jsonschema.ValidationError) []Issue {
	seen := make(map[Issue]struct{})
	var walk func(node *jsonschema.ValidationError)
	walk = func(node *jsonschema.ValidationError) {
		if len(node.Causes) > 0 {
			for _, cause := range node.Causes {
				walk(cause)
			}
			return
		}
		issue := Issue{
			InstancePath: jsonPointer(node.InstanceLocation),
			KeywordPath:  jsonPointer(node.ErrorKind.KeywordPath()),
		}
		seen[issue] = struct{}{}
	}
	walk(root)

	issues := make([]Issue, 0, len(seen))
	for issue := range seen {
		issues = append(issues, issue)
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].InstancePath != issues[j].InstancePath {
			return issues[i].InstancePath < issues[j].InstancePath
		}
		return issues[i].KeywordPath < issues[j].KeywordPath
	})
	return issues
}

func jsonPointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, token := range tokens {
		builder.WriteByte('/')
		builder.WriteString(strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1"))
	}
	return builder.String()
}
