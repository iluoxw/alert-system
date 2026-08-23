package model

// Profile 镜像 service-observability-profile.schema.json。它是声明式配置：只能引用
// 服务端已实现的 renderer 与已发布的 tool key，绝不携带 SQL/PromQL/凭据/prompt/
// 可执行代码——schema 的 additionalProperties=false 加上 profile 包的递归 key 黑名单
// 共同保证这一点。
type Profile struct {
	SchemaVersion  string            `json:"schema_version"`
	ProfileKey     string            `json:"profile_key"`
	ProfileVersion int               `json:"profile_version"`
	Status         ProfileStatus     `json:"status"`
	ServiceRef     ProfileServiceRef `json:"service_ref"`
	FocusKinds     []FocusKind       `json:"focus_kinds"`
	Journeys       []Journey         `json:"journeys"`
	Views          []ProfileView     `json:"views"`
	SignalBindings []SignalBinding   `json:"signal_bindings"`
	ToolRefs       []string          `json:"tool_refs"`
	Limits         ProfileLimits     `json:"limits"`
}

// ProfileServiceRef 的两个字段按字段本义绑定当前环境内真实的 ID：ServiceID 是
// service_registrations.id，RootEntityIDs 是 resource_nodes.id。实现不得把它们
// 悄悄当作 entity_key 解释，也不做 entity_key fallback；跨环境 portable 引用需要
// 另行演进 schema。
type ProfileServiceRef struct {
	ServiceID     string   `json:"service_id"`
	RootEntityIDs []string `json:"root_entity_ids"`
}

// FocusKind 声明工作区可接受的聚焦维度。ValuePattern 必须能被 Go RE2 编译，否则
// 整个 Profile 被 quarantine——不能让一条无法编译的正则在请求时才炸。
type FocusKind struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	ValuePattern string `json:"value_pattern"`
	Placeholder  string `json:"placeholder"`
	Primary      bool   `json:"primary"`
}

type Journey struct {
	Key       string            `json:"key"`
	Label     string            `json:"label"`
	FocusKind string            `json:"focus_kind"`
	Stages    []JourneyStageDef `json:"stages"`
}

// JourneyStageDef 的 entity/relation refs 是 canonical 引用，允许落在当前 bounded
// 拓扑投影之外；投影时只取交集，不为了补全而临时造图。
type JourneyStageDef struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	EntityRefs   []string `json:"entity_refs,omitempty"`
	RelationRefs []string `json:"relation_refs,omitempty"`
}

type ProfileView struct {
	Key        string   `json:"key"`
	Renderer   Renderer `json:"renderer"`
	Title      string   `json:"title"`
	Order      int      `json:"order"`
	JourneyRef string   `json:"journey_ref,omitempty"`
	SignalRefs []string `json:"signal_refs,omitempty"`
	ToolRefs   []string `json:"tool_refs,omitempty"`
}

// SignalBinding 把一个语义 slot 绑定到一个 tool 的输出字段。Profile v1 没有 optional
// signal 字段，因此每个 binding 都是 required：它缺失会让 required coverage 不完整。
type SignalBinding struct {
	Key             string             `json:"key"`
	Label           string             `json:"label"`
	Family          SignalFamily       `json:"family"`
	Semantic        SignalSemantic     `json:"semantic"`
	Scope           SignalBindingScope `json:"scope"`
	ToolKey         string             `json:"tool_key"`
	OutputField     string             `json:"output_field"`
	Unit            string             `json:"unit"`
	Aggregation     Aggregation        `json:"aggregation"`
	MeasurementRole MeasurementRole    `json:"measurement_role,omitempty"`
	ProxyReason     string             `json:"proxy_reason,omitempty"`
	BaselineRef     string             `json:"baseline_ref,omitempty"`
	// Thresholds 缺失即"没有方向阈值"：Traffic/Rate 因此恒为 neutral，不进入
	// worst-of 聚合，也不能被解释成 Healthy。
	Thresholds *SignalThresholds `json:"thresholds,omitempty"`
}

type SignalBindingScope struct {
	Kind ScopeKind `json:"kind"`
	Refs []string  `json:"refs,omitempty"`
}

type SignalThresholds struct {
	Mode      ThresholdMode      `json:"mode"`
	Direction ThresholdDirection `json:"direction"`
	Degraded  float64            `json:"degraded"`
	Critical  float64            `json:"critical"`
}

type ProfileLimits struct {
	DefaultWindowSeconds int `json:"default_window_seconds"`
	MaxWindowSeconds     int `json:"max_window_seconds"`
	MaxNodes             int `json:"max_nodes"`
	MaxRelations         int `json:"max_relations"`
	MaxRows              int `json:"max_rows"`
	MaxEvidenceRefs      int `json:"max_evidence_refs"`
	FreshnessTTLSeconds  int `json:"freshness_ttl_seconds"`
}

// Normalize 把 required 数组的 nil 补成空 slice，避免序列化出 null。
//
// 注意 service_ref.root_entity_ids 也在其中：它是 schema required 的数组，所以序列化
// 形状必须是 []。这只修正形状，不改变 schema 的 minItems=1 语义——空的 root_entity_ids
// 依然是非法 Profile，会被 v1 schema 校验拒绝并 quarantine。
func (p *Profile) Normalize() {
	p.ServiceRef.RootEntityIDs = nonNilSlice(p.ServiceRef.RootEntityIDs)
	p.FocusKinds = nonNilSlice(p.FocusKinds)
	p.Journeys = nonNilSlice(p.Journeys)
	for i := range p.Journeys {
		p.Journeys[i].Stages = nonNilSlice(p.Journeys[i].Stages)
	}
	p.Views = nonNilSlice(p.Views)
	p.SignalBindings = nonNilSlice(p.SignalBindings)
	p.ToolRefs = nonNilSlice(p.ToolRefs)
}

// Clone 返回深拷贝。Registry 对外只交出副本，handler 无法改到共享配置。
func (p Profile) Clone() Profile {
	clone := p
	clone.ServiceRef.RootEntityIDs = cloneStrings(p.ServiceRef.RootEntityIDs)
	clone.FocusKinds = cloneSlice(p.FocusKinds)
	clone.Journeys = make([]Journey, len(p.Journeys))
	for i, journey := range p.Journeys {
		copied := journey
		copied.Stages = make([]JourneyStageDef, len(journey.Stages))
		for j, stage := range journey.Stages {
			copiedStage := stage
			copiedStage.EntityRefs = cloneStrings(stage.EntityRefs)
			copiedStage.RelationRefs = cloneStrings(stage.RelationRefs)
			copied.Stages[j] = copiedStage
		}
		clone.Journeys[i] = copied
	}
	clone.Views = make([]ProfileView, len(p.Views))
	for i, view := range p.Views {
		copied := view
		copied.SignalRefs = cloneStrings(view.SignalRefs)
		copied.ToolRefs = cloneStrings(view.ToolRefs)
		clone.Views[i] = copied
	}
	clone.SignalBindings = make([]SignalBinding, len(p.SignalBindings))
	for i, binding := range p.SignalBindings {
		copied := binding
		copied.Scope.Refs = cloneStrings(binding.Scope.Refs)
		if binding.Thresholds != nil {
			thresholds := *binding.Thresholds
			copied.Thresholds = &thresholds
		}
		clone.SignalBindings[i] = copied
	}
	clone.ToolRefs = cloneStrings(p.ToolRefs)
	return clone
}

// GoldenBindings 按 latency/traffic/errors/saturation 的固定顺序返回 golden binding。
// 只有 Registry 校验通过（恰好四个、四种 semantic 各一）的 Profile 才会命中全部四项。
func (p Profile) GoldenBindings() []SignalBinding {
	bySemantic := make(map[SignalSemantic]SignalBinding, 4)
	for _, binding := range p.SignalBindings {
		if binding.Family != SignalFamilyGolden {
			continue
		}
		if _, exists := bySemantic[binding.Semantic]; !exists {
			bySemantic[binding.Semantic] = binding
		}
	}
	ordered := make([]SignalBinding, 0, 4)
	for _, semantic := range GoldenSemantics() {
		if binding, ok := bySemantic[semantic]; ok {
			ordered = append(ordered, binding)
		}
	}
	return ordered
}

// BindingByKey 按 key 查 binding；projector 用它把 signal 值配回声明的语义。
func (p Profile) BindingByKey(key string) (SignalBinding, bool) {
	for _, binding := range p.SignalBindings {
		if binding.Key == key {
			return binding, true
		}
	}
	return SignalBinding{}, false
}

// PrimaryFocusKind 返回唯一的 primary focus。Registry 保证恰好一个，这里对未校验的
// 文档保持 false 而不是猜第一个。
func (p Profile) PrimaryFocusKind() (FocusKind, bool) {
	var found FocusKind
	count := 0
	for _, focus := range p.FocusKinds {
		if focus.Primary {
			found = focus
			count++
		}
	}
	if count != 1 {
		return FocusKind{}, false
	}
	return found, true
}
