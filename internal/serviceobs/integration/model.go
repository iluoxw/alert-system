// Package integration 是 Service Integration Package（SIP）v1 的 Go 镜像、schema 校验与
// 发布前 compiler。
//
// 三条边界在这里就固化下来：
//
//  1. SIP 是**声明式文档**，不是执行插件。它只能引用受信 provider template 并传字面参数；
//     PromQL/SQL 的构造属于 Phase 3 Task 4/5 的 adapter，本包不含任何查询文本。
//  2. package 只写稳定 `entity_key`，真实的 `service_registrations.id` /
//     `resource_nodes.id` 一律由 compiler 在发布前唯一解析。零命中或多命中 fail closed，
//     没有 entity_key fallback，也没有任何 fixture ID 兜底路径。
//  3. 编译产物继续过一遍现有 Profile schema、结构校验与 Tool catalog 校验。SIP 不绕过
//     Profile Registry，也不改变 v1 字段的 ID 语义。
//
// 依赖方向与 ports 一致：本包只依赖 contract/model/profile/tools 这些既有边界，不 import
// store、vendor SDK 或任何业务私有包，因此"第二个服务只需登记 service、写一份 package"
// 这件事不会因为核心增加分支而失效。
package integration

// Package 是一份 SIP v1 文档。字段顺序与 service-integration-package.schema.json 一致。
//
// 它刻意不含 service_ref / profile_version / profile status：那三项由 compiler 从
// target 解析与 package 元数据生成，package 作者写不出真实 ID，也无法自行把 draft 提成
// published Profile。
type Package struct {
	SchemaVersion   string             `json:"schema_version"`
	PackageKey      string             `json:"package_key"`
	PackageVersion  int                `json:"package_version"`
	Status          Status             `json:"status"`
	Description     string             `json:"description,omitempty"`
	Compatibility   Compatibility      `json:"compatibility"`
	Target          Target             `json:"target"`
	ProfileTemplate ProfileTemplate    `json:"profile_template"`
	Telemetry       []TelemetryBinding `json:"telemetry_bindings"`
	TopologyPolicy  TopologyPolicy     `json:"topology_policy"`
}

// Status 是 package 的发布状态。它决定编译产物的 Profile status，因此 draft package
// 不可能变成某个服务的 active published Profile。
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
)

// Compatibility 声明该 package 期望的契约与 compiler 版本。
//
// MinCompilerVersion 高于当前 compiler 时必须拒绝编译：一个用更新语义写成的 package 被
// 老 compiler "尽力"编译出来，等于静默丢掉它依赖的约束。
type Compatibility struct {
	ProfileSchemaVersion string `json:"profile_schema_version"`
	MinCompilerVersion   int    `json:"min_compiler_version"`
}

// Target 是编译目标环境与 canonical 对象选择器。
type Target struct {
	Environment     string           `json:"environment"`
	ServiceSelector EntitySelector   `json:"service_selector"`
	RootSelectors   []EntitySelector `json:"root_selectors"`
}

// EntitySelector 按稳定 entity_key 选一个 canonical 对象。
type EntitySelector struct {
	EntityKey string `json:"entity_key"`
}

// RelationSelector 按两端 entity_key 与 canonical 关系类型选一条依赖边。
// 方向固定 consumer/caller -> provider/callee，与 canonical 依赖边一致。
type RelationSelector struct {
	FromEntityKey string `json:"from_entity_key"`
	ToEntityKey   string `json:"to_entity_key"`
	Kind          string `json:"kind"`
}

// ProfileTemplate 是编译目标 Profile 的声明部分。
type ProfileTemplate struct {
	ProfileKey     string          `json:"profile_key"`
	FocusKinds     []FocusKind     `json:"focus_kinds"`
	Journeys       []Journey       `json:"journeys"`
	Views          []View          `json:"views"`
	SignalBindings []SignalBinding `json:"signal_bindings"`
	ToolRefs       []string        `json:"tool_refs"`
	Limits         TemplateLimits  `json:"limits"`
}

type FocusKind struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	ValuePattern string `json:"value_pattern"`
	Placeholder  string `json:"placeholder,omitempty"`
	Primary      bool   `json:"primary"`
}

type Journey struct {
	Key       string         `json:"key"`
	Label     string         `json:"label"`
	FocusKind string         `json:"focus_kind"`
	Stages    []JourneyStage `json:"stages"`
}

// JourneyStage 只声明顺序与 canonical 引用。stage 的运行状态只能来自受信 adapter 的
// 观测事实，package 声明不了状态。
type JourneyStage struct {
	Key               string             `json:"key"`
	Label             string             `json:"label"`
	EntitySelectors   []EntitySelector   `json:"entity_selectors,omitempty"`
	RelationSelectors []RelationSelector `json:"relation_selectors,omitempty"`
}

type View struct {
	Key        string   `json:"key"`
	Renderer   string   `json:"renderer"`
	Title      string   `json:"title"`
	Order      int      `json:"order"`
	JourneyRef string   `json:"journey_ref,omitempty"`
	SignalRefs []string `json:"signal_refs,omitempty"`
	ToolRefs   []string `json:"tool_refs,omitempty"`
}

type SignalBinding struct {
	Key             string      `json:"key"`
	Label           string      `json:"label"`
	Family          string      `json:"family"`
	Semantic        string      `json:"semantic"`
	Scope           SignalScope `json:"scope"`
	ToolKey         string      `json:"tool_key"`
	OutputField     string      `json:"output_field"`
	Unit            string      `json:"unit"`
	Aggregation     string      `json:"aggregation"`
	MeasurementRole string      `json:"measurement_role,omitempty"`
	ProxyReason     string      `json:"proxy_reason,omitempty"`
	BaselineRef     string      `json:"baseline_ref,omitempty"`
	Thresholds      *Thresholds `json:"thresholds,omitempty"`
}

type SignalScope struct {
	Kind              string             `json:"kind"`
	EntitySelectors   []EntitySelector   `json:"entity_selectors,omitempty"`
	RelationSelectors []RelationSelector `json:"relation_selectors,omitempty"`
}

type Thresholds struct {
	Mode      string  `json:"mode"`
	Direction string  `json:"direction"`
	Degraded  float64 `json:"degraded"`
	Critical  float64 `json:"critical"`
}

type TemplateLimits struct {
	DefaultWindowSeconds int `json:"default_window_seconds"`
	MaxWindowSeconds     int `json:"max_window_seconds"`
	MaxNodes             int `json:"max_nodes"`
	MaxRelations         int `json:"max_relations"`
	MaxRows              int `json:"max_rows"`
	MaxEvidenceRefs      int `json:"max_evidence_refs"`
	FreshnessTTLSeconds  int `json:"freshness_ttl_seconds"`
}

// TelemetryBinding 把一个 signal binding 绑到一个受信 provider template。
type TelemetryBinding struct {
	BindingKey  string         `json:"binding_key"`
	TemplateRef string         `json:"template_ref"`
	Params      TemplateParams `json:"params"`
}

// TemplateParams 是封闭的参数槽位集合。
//
// 每个槽位都有固定语义与严格形状，schema 已经限制到字符集级别；compiler 再按 template
// 声明校验必填与禁止槽位，因此未知 template 或错配参数都无法通过。
type TemplateParams struct {
	Metric            string          `json:"metric,omitempty"`
	NumeratorMetric   string          `json:"numerator_metric,omitempty"`
	DenominatorMetric string          `json:"denominator_metric,omitempty"`
	BucketMetric      string          `json:"bucket_metric,omitempty"`
	TotalMetric       string          `json:"total_metric,omitempty"`
	FreeMetric        string          `json:"free_metric,omitempty"`
	Labels            []LabelEquality `json:"labels,omitempty"`
	GroupBy           []string        `json:"group_by,omitempty"`
	Quantile          *float64        `json:"quantile,omitempty"`
	RangeSeconds      *int            `json:"range_seconds,omitempty"`
	Stream            string          `json:"stream,omitempty"`
	FocusField        string          `json:"focus_field,omitempty"`
	StageField        string          `json:"stage_field,omitempty"`
	StatusField       string          `json:"status_field,omitempty"`
	SelectFields      []string        `json:"select_fields,omitempty"`
	RowLimit          *int            `json:"row_limit,omitempty"`
}

// LabelEquality 是 label 等值对。schema 的 value 字符集里没有 regex/否定/集合元字符与
// 空白，因此自由 matcher 在语法层面就不可表达。
type LabelEquality struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TopologyPolicy 声明 discovery 的 alias namespace 白名单与 observed overlay 边界。
type TopologyPolicy struct {
	AliasNamespaces []string         `json:"alias_namespaces"`
	Discovery       *DiscoveryPolicy `json:"discovery,omitempty"`
	Overlay         OverlayPolicy    `json:"overlay"`
}

type DiscoveryPolicy struct {
	IdentityFields []string `json:"identity_fields,omitempty"`
	// InstanceRole 区分"稳定逻辑组件"与"随副本变化的实例清单成员"：实例数量变化不得被
	// 解释成组件拓扑变更。
	InstanceRole string `json:"instance_role,omitempty"`
}

type OverlayPolicy struct {
	StaleAfterSeconds int  `json:"stale_after_seconds"`
	MaxHints          *int `json:"max_hints,omitempty"`
}
