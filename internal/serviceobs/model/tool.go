package model

type ToolCategory string

const (
	ToolCategoryContext  ToolCategory = "context"
	ToolCategoryTopology ToolCategory = "topology"
	ToolCategoryMetrics  ToolCategory = "metrics"
	ToolCategoryLogs     ToolCategory = "logs"
	ToolCategoryTraces   ToolCategory = "traces"
	ToolCategoryDomain   ToolCategory = "domain"
)

func (c ToolCategory) IsValid() bool {
	switch c {
	case ToolCategoryContext, ToolCategoryTopology, ToolCategoryMetrics,
		ToolCategoryLogs, ToolCategoryTraces, ToolCategoryDomain:
		return true
	default:
		return false
	}
}

// ToolDescriptor 镜像 tool-descriptor.schema.json：服务端注册的类型化只读工具元数据。
//
// 它刻意只描述"输入输出形状、adapter、模板引用、权限、限额、审计、脱敏"，不携带
// SQL/PromQL/LogQL/Shell/凭据/URL header/prompt。真正的 query template 留在服务端代码
// 或配置里，Profile 与浏览器只能传语义 key。ReadOnly 是 schema 里的 const true：
// 本 Phase 不存在写工具，未来写工具需要新 ADR 与独立授权流程，不能复用这个 descriptor。
type ToolDescriptor struct {
	SchemaVersion   string       `json:"schema_version"`
	Key             string       `json:"key"`
	SemanticVersion string       `json:"semantic_version"`
	Category        ToolCategory `json:"category"`
	// SourceAdapter 是服务端 Tool Registry 里的受控 key。schema 保持 provider-neutral，
	// allowlist 由 registry 持有，不在契约里写死供应商集合。
	SourceAdapter       string         `json:"source_adapter"`
	Description         string         `json:"description,omitempty"`
	ReadOnly            bool           `json:"read_only"`
	InputSchema         ToolJSONSchema `json:"input_schema"`
	OutputSchema        ToolJSONSchema `json:"output_schema"`
	QueryTemplateRef    string         `json:"query_template_ref"`
	RequiredPermissions []string       `json:"required_permissions"`
	Limits              ToolLimits     `json:"limits"`
	FreshnessTTLSeconds int            `json:"freshness_ttl_seconds"`
	Audit               ToolAudit      `json:"audit"`
	Redaction           ToolRedaction  `json:"redaction"`
}

// ToolJSONSchema 是嵌套的输入/输出 JSON Schema 片段。用 map 而不是具体结构体，是因为
// 契约只约束它是一个 type 为 object/array 的 schema 对象，具体形状由每个工具自己决定。
type ToolJSONSchema map[string]any

type ToolLimits struct {
	TimeoutMS        int `json:"timeout_ms"`
	MaxWindowSeconds int `json:"max_window_seconds"`
	MaxRows          int `json:"max_rows"`
	MaxCardinality   int `json:"max_cardinality"`
}

// ToolAudit 的两个字段在 schema 里都是 const true：不存在"不审计"的只读工具。
type ToolAudit struct {
	LogInvocation     bool `json:"log_invocation"`
	LogResultMetadata bool `json:"log_result_metadata"`
}

type ToolRedaction struct {
	PolicyRef      string `json:"policy_ref"`
	DenyRawSecrets bool   `json:"deny_raw_secrets"`
}

func (t *ToolDescriptor) Normalize() {
	t.RequiredPermissions = nonNilSlice(t.RequiredPermissions)
}

// Clone 深拷贝 descriptor，包括两个 schema map，避免 catalog 的调用方改到共享注册表。
func (t ToolDescriptor) Clone() ToolDescriptor {
	clone := t
	clone.RequiredPermissions = cloneStrings(t.RequiredPermissions)
	clone.InputSchema = cloneJSONValue(t.InputSchema).(ToolJSONSchema)
	clone.OutputSchema = cloneJSONValue(t.OutputSchema).(ToolJSONSchema)
	return clone
}
