package integration

import (
	"fmt"
	"sort"
)

// 本文件是受信 **provider template registry**。
//
// 它刻意**只**持有每个模板的能力归属、参数槽位契约与输出字段名。真正的 PromQL/SQL 构造
// 属于 Phase 3 Task 4/5 的 adapter：把查询文本放在这里会让"服务方提交不了查询"这条边界
// 变成"服务方提交不了查询，但注册表里躺着一份可被参数拼接的模板"，两者的审计面完全不同。
//
// registry 的职责是回答三个问题，且只回答这三个：
//
//  1. 这个 template_ref 存在吗（未知 ref 一律 fail closed）；
//  2. package 传的参数槽位是否恰好满足它的必填/可选契约（多传、少传、错配都拒绝）；
//  3. signal binding 声明的 output_field 是否是它真的会产出的字段。

// Capability 是模板所属的 adapter 能力。它与 tools 包的 source adapter allowlist 同名，
// 但语义不同：这里描述"用哪个 provider 执行"，那里描述"哪个 tool 由哪个 adapter 实现"。
type Capability string

const (
	CapabilityPrometheus  Capability = "prometheus_compatible"
	CapabilityOpenObserve Capability = "openobserve"
)

// ParamSlot 是参数槽位名。它与 TemplateParams 的字段一一对应；用显式常量而不是反射，
// 是为了让"某个模板允许哪些槽位"成为可读、可测的声明。
type ParamSlot string

const (
	SlotMetric            ParamSlot = "metric"
	SlotNumeratorMetric   ParamSlot = "numerator_metric"
	SlotDenominatorMetric ParamSlot = "denominator_metric"
	SlotBucketMetric      ParamSlot = "bucket_metric"
	SlotTotalMetric       ParamSlot = "total_metric"
	SlotFreeMetric        ParamSlot = "free_metric"
	SlotLabels            ParamSlot = "labels"
	SlotCommonLabels      ParamSlot = "common_labels"
	SlotNumeratorLabels   ParamSlot = "numerator_labels"
	SlotDenominatorLabels ParamSlot = "denominator_labels"
	SlotGroupBy           ParamSlot = "group_by"
	SlotQuantile          ParamSlot = "quantile"
	SlotRangeSeconds      ParamSlot = "range_seconds"
	SlotStream            ParamSlot = "stream"
	SlotFocusField        ParamSlot = "focus_field"
	SlotStageField        ParamSlot = "stage_field"
	SlotStatusField       ParamSlot = "status_field"
	SlotSelectFields      ParamSlot = "select_fields"
	SlotRowLimit          ParamSlot = "row_limit"
)

// EvidenceClass 是 evidence binding 的证据类别。它由受信 registry 推导，package 不能自报。
type EvidenceClass string

const (
	EvidenceClassLogs   EvidenceClass = "logs"
	EvidenceClassTraces EvidenceClass = "traces"
	EvidenceClassAccess EvidenceClass = "access"
)

// SideOutput 是模板在同一次有界读取里必然额外投影的产物。
//
// 它把一次读取必然产生的额外事实变成受信契约：一次 trace 读取可以同时给出 journey 阶段、
// focus 结论、trace EvidenceRef 与 observed topology hints，adapter 必须一起投影。side output
// 不参与 primary binding 多重性；多个来源或覆盖范围不同的 binding 可以共存，精确重复执行由
// adapter 合并，而不是由 compiler 猜测两个 binding 是否等价。
type SideOutput string

const (
	SideOutputFocus         SideOutput = "focus"
	SideOutputTraceEvidence SideOutput = "trace_evidence"
	SideOutputTopologyHints SideOutput = "topology_hints"
)

// Template 是一个受信 provider 查询模板的**契约描述**。
type Template struct {
	Ref        string
	Capability Capability
	// AllowedKinds 是该模板可以充当的主要 binding kind。空集是配置错误。
	AllowedKinds []BindingKind
	// SourceKey 是该模板读取时归属的稳定 source key。它进 CompiledIntegration，
	// 让 coordinator 与 adapter 不必再从 capability 猜。
	SourceKey string
	// EvidenceClass 只在模板可充当 evidence 时有意义。
	EvidenceClass EvidenceClass
	// SideOutputs 是固定的额外产物。package 不能增删。
	SideOutputs []SideOutput
	// Required 是必须出现的槽位；Optional 是允许出现的槽位。两者之外的槽位一律拒绝，
	// 因此"给 gauge 模板传 quantile"这种错配不会被静默忽略。
	Required []ParamSlot
	Optional []ParamSlot
	// Outputs 是该模板真的会产出的字段名。signal binding 的 output_field 必须命中其中之一。
	Outputs []string
	// RequiredPermission 是执行该模板所需的最小权限。它在这里只是**声明**，实际判定由
	// Task 7 的 PermissionResolver 在服务端按 session 计算；package 无法自报权限。
	RequiredPermission string
	Summary            string
}

// source key 与 serviceobs 的稳定 source key 逐字一致。这里重复声明而不是 import
// serviceobs：integration 是编写/发布期包，不该依赖运行时 ports；两侧的一致性由
// TestTemplateSourceKeysMatchRuntime 守护。
const (
	SourceKeyPrometheus        = "prometheus_compatible"
	SourceKeyOpenObserveLogs   = "openobserve_logs"
	SourceKeyOpenObserveTraces = "openobserve_traces"
	SourceKeyOpenObserveAccess = "openobserve_access"
)

// 权限 key 与设计交接稿 §14 一致：metrics/logs/traces/faults 分离。
const (
	PermissionMetricsRead = "metrics.read"
	PermissionLogsRead    = "logs.read"
	PermissionTracesRead  = "traces.read"
)

// registry 是编译进 binary 的模板集合。新增模板必须同时补 golden test 与 docs 表格。
var registry = func() map[string]Template {
	templates := []Template{
		{
			Ref:                "counter_rate.v1",
			Capability:         CapabilityPrometheus,
			AllowedKinds:       []BindingKind{KindSignal},
			SourceKey:          SourceKeyPrometheus,
			Required:           []ParamSlot{SlotMetric, SlotRangeSeconds},
			Optional:           []ParamSlot{SlotLabels, SlotGroupBy},
			Outputs:            []string{"series.rate_per_second"},
			RequiredPermission: PermissionMetricsRead,
			Summary:            "counter 的区间速率",
		},
		{
			Ref:          "counter_ratio_percent.v1",
			Capability:   CapabilityPrometheus,
			AllowedKinds: []BindingKind{KindSignal},
			SourceKey:    SourceKeyPrometheus,
			Required:     []ParamSlot{SlotNumeratorMetric, SlotDenominatorMetric, SlotRangeSeconds},
			// 三组 label 分开：common 施加到两侧，numerator/denominator 各自附加。
			// 只用一组 label 会让"同 metric 的错误率"恒等于 100%。
			Optional:           []ParamSlot{SlotCommonLabels, SlotNumeratorLabels, SlotDenominatorLabels, SlotGroupBy},
			Outputs:            []string{"series.ratio_percent"},
			RequiredPermission: PermissionMetricsRead,
			Summary:            "两个 counter 的百分比",
		},
		{
			Ref:                "histogram_quantile_rate.v1",
			Capability:         CapabilityPrometheus,
			AllowedKinds:       []BindingKind{KindSignal},
			SourceKey:          SourceKeyPrometheus,
			Required:           []ParamSlot{SlotBucketMetric, SlotQuantile, SlotRangeSeconds},
			Optional:           []ParamSlot{SlotLabels, SlotGroupBy},
			Outputs:            []string{"series.quantile_value"},
			RequiredPermission: PermissionMetricsRead,
			Summary:            "histogram 分位数",
		},
		{
			Ref:                "gauge_ratio_percent.v1",
			Capability:         CapabilityPrometheus,
			AllowedKinds:       []BindingKind{KindSignal},
			SourceKey:          SourceKeyPrometheus,
			Required:           []ParamSlot{SlotTotalMetric, SlotFreeMetric},
			Optional:           []ParamSlot{SlotLabels, SlotGroupBy},
			Outputs:            []string{"series.ratio_percent"},
			RequiredPermission: PermissionMetricsRead,
			Summary:            "两个 gauge 的百分比",
		},
		{
			Ref:                "gauge_max.v1",
			Capability:         CapabilityPrometheus,
			AllowedKinds:       []BindingKind{KindSignal},
			SourceKey:          SourceKeyPrometheus,
			Required:           []ParamSlot{SlotMetric},
			Optional:           []ParamSlot{SlotLabels, SlotGroupBy},
			Outputs:            []string{"series.max_value"},
			RequiredPermission: PermissionMetricsRead,
			Summary:            "gauge 的最坏值",
		},
		{
			// 逐条 focus 记录搜索：回答"这个 focus 值命中了吗"。
			//
			// 权限与 source 沿用既有语义（traces.read / openobserve_traces）。这是通用模板，
			// 不因为某个服务的逐条字段恰好也出现在日志流里就改成 logs.read——那是把单个
			// 接入方的数据布局写进平台契约。真要改必须有权威文档或输出 schema 依据。
			Ref:                "focus_records.v1",
			Capability:         CapabilityOpenObserve,
			AllowedKinds:       []BindingKind{KindFocus},
			SourceKey:          SourceKeyOpenObserveTraces,
			Required:           []ParamSlot{SlotStream, SlotFocusField, SlotSelectFields, SlotRowLimit},
			Optional:           []ParamSlot{SlotStageField, SlotStatusField},
			Outputs:            []string{"records.count", "records.first_observed_at", "records.last_observed_at"},
			RequiredPermission: PermissionTracesRead,
			Summary:            "按 focus 逐条取记录",
		},
		{
			// 一次 trace 读取同时给出 journey 阶段（主）、focus 结论、trace EvidenceRef 与
			// observed topology hints（三个 side output）。这正是为什么不存在 topology kind：
			// 那会让同一次读取被重复发起三遍。
			Ref:           "trace_by_focus.v1",
			Capability:    CapabilityOpenObserve,
			AllowedKinds:  []BindingKind{KindJourney, KindEvidence},
			SourceKey:     SourceKeyOpenObserveTraces,
			EvidenceClass: EvidenceClassTraces,
			SideOutputs: []SideOutput{
				SideOutputFocus, SideOutputTraceEvidence, SideOutputTopologyHints,
			},
			Required:           []ParamSlot{SlotStream, SlotFocusField, SlotRowLimit},
			Optional:           []ParamSlot{SlotSelectFields, SlotStageField},
			Outputs:            []string{"traces.count", "traces.last_observed_at"},
			RequiredPermission: PermissionTracesRead,
			Summary:            "按 focus 取 trace 与 span",
		},
		{
			Ref:                "logs_by_focus.v1",
			Capability:         CapabilityOpenObserve,
			AllowedKinds:       []BindingKind{KindEvidence},
			SourceKey:          SourceKeyOpenObserveLogs,
			EvidenceClass:      EvidenceClassLogs,
			SideOutputs:        []SideOutput{SideOutputFocus},
			Required:           []ParamSlot{SlotStream, SlotFocusField, SlotRowLimit},
			Optional:           []ParamSlot{SlotSelectFields, SlotStatusField},
			Outputs:            []string{"logs.count", "logs.last_observed_at"},
			RequiredPermission: PermissionLogsRead,
			Summary:            "按 focus 取日志摘要",
		},
		{
			// access 记录可能带 request/response 片段。除非输出 schema 能保证不含任何 raw
			// payload，否则必须按 logs.read 从严要求，而不是借 traces.read 放行。
			Ref:                "access_by_focus.v1",
			Capability:         CapabilityOpenObserve,
			AllowedKinds:       []BindingKind{KindEvidence},
			SourceKey:          SourceKeyOpenObserveAccess,
			EvidenceClass:      EvidenceClassAccess,
			SideOutputs:        []SideOutput{SideOutputFocus},
			Required:           []ParamSlot{SlotStream, SlotFocusField, SlotRowLimit},
			Optional:           []ParamSlot{SlotSelectFields, SlotStatusField},
			Outputs:            []string{"access.count", "access.last_observed_at"},
			RequiredPermission: PermissionLogsRead,
			Summary:            "按 focus 取 access 记录摘要",
		},
	}
	byRef := make(map[string]Template, len(templates))
	for _, template := range templates {
		byRef[template.Ref] = template
	}
	return byRef
}()

// LookupTemplate 返回模板契约。第二个返回值为 false 表示未知 ref：调用方必须 fail closed。
func LookupTemplate(ref string) (Template, bool) {
	template, ok := registry[ref]
	return template, ok
}

// TemplateRefs 返回稳定排序的全部模板 ref，供文档与测试遍历。
func TemplateRefs() []string {
	refs := make([]string, 0, len(registry))
	for ref := range registry {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// AllowsKind 判断该模板能否充当某个主要 binding kind。
func (t Template) AllowsKind(kind BindingKind) bool {
	for _, allowed := range t.AllowedKinds {
		if allowed == kind {
			return true
		}
	}
	return false
}

// ProvidesSideOutput 判断该模板是否必然额外投影某个产物。
func (t Template) ProvidesSideOutput(output SideOutput) bool {
	for _, side := range t.SideOutputs {
		if side == output {
			return true
		}
	}
	return false
}

// allowsOutput 判断 output_field 是否是该模板真的会产出的字段。
func (t Template) allowsOutput(field string) bool {
	for _, output := range t.Outputs {
		if output == field {
			return true
		}
	}
	return false
}

// presentSlots 列出这份参数里真正出现的槽位。
//
// 判定用的是"零值即未出现"，而指针字段（quantile/range_seconds/row_limit）用 nil 判定，
// 因此 `quantile: 0` 这种显式非法值会被 schema 的 exclusiveMinimum 先拦掉，不会被这里
// 误当成"未提供"。
func presentSlots(params TemplateParams) map[ParamSlot]struct{} {
	present := map[ParamSlot]struct{}{}
	mark := func(slot ParamSlot, ok bool) {
		if ok {
			present[slot] = struct{}{}
		}
	}
	mark(SlotMetric, params.Metric != "")
	mark(SlotNumeratorMetric, params.NumeratorMetric != "")
	mark(SlotDenominatorMetric, params.DenominatorMetric != "")
	mark(SlotBucketMetric, params.BucketMetric != "")
	mark(SlotTotalMetric, params.TotalMetric != "")
	mark(SlotFreeMetric, params.FreeMetric != "")
	mark(SlotLabels, len(params.Labels) > 0)
	mark(SlotCommonLabels, len(params.CommonLabels) > 0)
	mark(SlotNumeratorLabels, len(params.NumeratorLabels) > 0)
	mark(SlotDenominatorLabels, len(params.DenominatorLabels) > 0)
	mark(SlotGroupBy, len(params.GroupBy) > 0)
	mark(SlotQuantile, params.Quantile != nil)
	mark(SlotRangeSeconds, params.RangeSeconds != nil)
	mark(SlotStream, params.Stream != "")
	mark(SlotFocusField, params.FocusField != "")
	mark(SlotStageField, params.StageField != "")
	mark(SlotStatusField, params.StatusField != "")
	mark(SlotSelectFields, len(params.SelectFields) > 0)
	mark(SlotRowLimit, params.RowLimit != nil)
	return present
}

// validateParams 校验参数槽位恰好满足模板契约。
func (t Template) validateParams(bindingKey string, params TemplateParams) error {
	allowed := map[ParamSlot]struct{}{}
	for _, slot := range t.Required {
		allowed[slot] = struct{}{}
	}
	for _, slot := range t.Optional {
		allowed[slot] = struct{}{}
	}

	present := presentSlots(params)
	missing := make([]string, 0, len(t.Required))
	for _, slot := range t.Required {
		if _, ok := present[slot]; !ok {
			missing = append(missing, string(slot))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] 模板 %s 缺少必填参数 %v", bindingKey, t.Ref, missing)
	}

	unexpected := make([]string, 0, len(present))
	for slot := range present {
		if _, ok := allowed[slot]; !ok {
			unexpected = append(unexpected, string(slot))
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return failf(ReasonTemplateParamInvalid,
			"provider_bindings[%s] 模板 %s 不接受参数 %v", bindingKey, t.Ref, unexpected)
	}
	return nil
}

// String 只输出 ref 与能力，避免把参数契约打进日志。
func (t Template) String() string {
	return fmt.Sprintf("Template{Ref:%s, Capability:%s}", t.Ref, t.Capability)
}
