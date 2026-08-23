package prometheus

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"alert-system/internal/serviceobs/integration"
)

const (
	maxRangeSeconds = 86400
	maxGroupBy      = 20
)

var (
	metricPattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePat  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	labelValuePat = regexp.MustCompile(`^[A-Za-z0-9_.:/@+-]+$`)
)

// QueryKind 区分 instant 与 range 查询。
type QueryKind int

const (
	QueryInstant QueryKind = iota
	QueryRange
)

// PlannedQuery 是一条待执行的 PromQL。
type PlannedQuery struct {
	Expr string
	Kind QueryKind
	// Role 在 gauge_ratio 等多查询模板里区分 total/free/numerator/denominator。
	Role string
}

// QueryPlan 是一个 binding 的执行计划，可含多条查询。
type QueryPlan struct {
	TemplateRef string
	Fingerprint string
	Queries     []PlannedQuery
	// AggregateOp 描述如何从多条 series 聚合为 binding 值。
	AggregateOp AggregateOp
	GroupBy     []string
}

// AggregateOp 是 binding 级聚合方式。
type AggregateOp string

const (
	AggregateWorstOf AggregateOp = "worst_of"
	AggregateSingle  AggregateOp = "single"
)

// BuildQueryPlan 从 compiled binding 构造 PromQL 执行计划。
func BuildQueryPlan(binding integration.CompiledBinding) (QueryPlan, error) {
	if err := validateBinding(binding); err != nil {
		return QueryPlan{}, err
	}
	switch binding.TemplateRef {
	case "counter_rate.v1":
		return buildCounterRate(binding)
	case "counter_ratio_percent.v1":
		return buildCounterRatio(binding)
	case "histogram_quantile_rate.v1":
		return buildHistogramQuantile(binding)
	case "gauge_max.v1":
		return buildGaugeMax(binding)
	case "gauge_ratio_percent.v1":
		return buildGaugeRatio(binding)
	default:
		return QueryPlan{}, fmt.Errorf("%w: 未知模板 %s", ErrInvalidParams, binding.TemplateRef)
	}
}

// ExecutionFingerprint 返回 template+params 的稳定指纹，用于 request 内去重。
func ExecutionFingerprint(binding integration.CompiledBinding) (string, error) {
	plan, err := BuildQueryPlan(binding)
	if err != nil {
		return "", err
	}
	return plan.Fingerprint, nil
}

func validateBinding(binding integration.CompiledBinding) error {
	if binding.SourceKey != integration.SourceKeyPrometheus {
		return fmt.Errorf("%w: source_key=%s", ErrBindingMismatch, binding.SourceKey)
	}
	if binding.Kind != integration.KindSignal {
		return fmt.Errorf("%w: kind=%s", ErrBindingMismatch, binding.Kind)
	}
	if binding.Capability != integration.CapabilityPrometheus {
		return fmt.Errorf("%w: capability=%s", ErrBindingMismatch, binding.Capability)
	}
	return nil
}

func buildCounterRate(binding integration.CompiledBinding) (QueryPlan, error) {
	params := binding.Params
	if err := validateMetric(params.Metric); err != nil {
		return QueryPlan{}, err
	}
	rangeSec, err := requireRange(params.RangeSeconds)
	if err != nil {
		return QueryPlan{}, err
	}
	labels, err := validateLabels(params.Labels)
	if err != nil {
		return QueryPlan{}, err
	}
	groupBy, err := validateGroupBy(params.GroupBy)
	if err != nil {
		return QueryPlan{}, err
	}
	inner := fmt.Sprintf("rate(%s%s[%ds])", params.Metric, labelSelector(labels), rangeSec)
	expr := wrapGroupBy(groupBy, "sum", inner)
	return planFrom(binding, []PlannedQuery{{Expr: expr, Kind: QueryInstant, Role: "value"}}, AggregateWorstOf, groupBy), nil
}

func buildCounterRatio(binding integration.CompiledBinding) (QueryPlan, error) {
	params := binding.Params
	if err := validateMetric(params.NumeratorMetric); err != nil {
		return QueryPlan{}, err
	}
	if err := validateMetric(params.DenominatorMetric); err != nil {
		return QueryPlan{}, err
	}
	rangeSec, err := requireRange(params.RangeSeconds)
	if err != nil {
		return QueryPlan{}, err
	}
	groupBy, err := validateGroupBy(params.GroupBy)
	if err != nil {
		return QueryPlan{}, err
	}
	numLabels, err := validateLabels(effectiveSelector(params.CommonLabels, params.NumeratorLabels))
	if err != nil {
		return QueryPlan{}, err
	}
	denLabels, err := validateLabels(effectiveSelector(params.CommonLabels, params.DenominatorLabels))
	if err != nil {
		return QueryPlan{}, err
	}
	if params.NumeratorMetric == params.DenominatorMetric &&
		selectorSignature(numLabels) == selectorSignature(denLabels) {
		return QueryPlan{}, fmt.Errorf("%w: 比率分子分母退化", ErrInvalidParams)
	}
	numInner := fmt.Sprintf("rate(%s%s[%ds])", params.NumeratorMetric, labelSelector(numLabels), rangeSec)
	denInner := fmt.Sprintf("rate(%s%s[%ds])", params.DenominatorMetric, labelSelector(denLabels), rangeSec)
	numExpr := wrapGroupBy(groupBy, "sum", numInner)
	denExpr := wrapGroupBy(groupBy, "sum", denInner)
	return planFrom(binding, []PlannedQuery{
		{Expr: numExpr, Kind: QueryInstant, Role: "numerator"},
		{Expr: denExpr, Kind: QueryInstant, Role: "denominator"},
	}, AggregateWorstOf, groupBy), nil
}

func buildHistogramQuantile(binding integration.CompiledBinding) (QueryPlan, error) {
	params := binding.Params
	if err := validateMetric(params.BucketMetric); err != nil {
		return QueryPlan{}, err
	}
	if params.Quantile == nil {
		return QueryPlan{}, fmt.Errorf("%w: 缺少 quantile", ErrInvalidParams)
	}
	if *params.Quantile <= 0 || *params.Quantile >= 1 {
		return QueryPlan{}, fmt.Errorf("%w: quantile 超出 (0,1)", ErrInvalidParams)
	}
	rangeSec, err := requireRange(params.RangeSeconds)
	if err != nil {
		return QueryPlan{}, err
	}
	labels, err := validateLabels(params.Labels)
	if err != nil {
		return QueryPlan{}, err
	}
	groupBy, err := validateGroupBy(params.GroupBy)
	if err != nil {
		return QueryPlan{}, err
	}
	sumBy := append([]string{"le"}, groupBy...)
	sort.Strings(sumBy)
	inner := fmt.Sprintf("rate(%s%s[%ds])", params.BucketMetric, labelSelector(labels), rangeSec)
	aggregated := fmt.Sprintf("sum by (%s) (%s)", strings.Join(sumBy, ", "), inner)
	expr := fmt.Sprintf("histogram_quantile(%s, %s)", formatQuantile(*params.Quantile), aggregated)
	return planFrom(binding, []PlannedQuery{{Expr: expr, Kind: QueryInstant, Role: "value"}}, AggregateWorstOf, groupBy), nil
}

func buildGaugeMax(binding integration.CompiledBinding) (QueryPlan, error) {
	params := binding.Params
	if err := validateMetric(params.Metric); err != nil {
		return QueryPlan{}, err
	}
	labels, err := validateLabels(params.Labels)
	if err != nil {
		return QueryPlan{}, err
	}
	groupBy, err := validateGroupBy(params.GroupBy)
	if err != nil {
		return QueryPlan{}, err
	}
	inner := fmt.Sprintf("%s%s", params.Metric, labelSelector(labels))
	expr := wrapGroupBy(groupBy, "max", inner)
	return planFrom(binding, []PlannedQuery{{Expr: expr, Kind: QueryInstant, Role: "value"}}, AggregateWorstOf, groupBy), nil
}

func buildGaugeRatio(binding integration.CompiledBinding) (QueryPlan, error) {
	params := binding.Params
	if err := validateMetric(params.TotalMetric); err != nil {
		return QueryPlan{}, err
	}
	if err := validateMetric(params.FreeMetric); err != nil {
		return QueryPlan{}, err
	}
	labels, err := validateLabels(params.Labels)
	if err != nil {
		return QueryPlan{}, err
	}
	groupBy, err := validateGroupBy(params.GroupBy)
	if err != nil {
		return QueryPlan{}, err
	}
	totalInner := fmt.Sprintf("%s%s", params.TotalMetric, labelSelector(labels))
	freeInner := fmt.Sprintf("%s%s", params.FreeMetric, labelSelector(labels))
	totalExpr := wrapGroupBy(groupBy, "max", totalInner)
	freeExpr := wrapGroupBy(groupBy, "max", freeInner)
	return planFrom(binding, []PlannedQuery{
		{Expr: totalExpr, Kind: QueryInstant, Role: "total"},
		{Expr: freeExpr, Kind: QueryInstant, Role: "free"},
	}, AggregateWorstOf, groupBy), nil
}

func planFrom(
	binding integration.CompiledBinding,
	queries []PlannedQuery,
	op AggregateOp,
	groupBy []string,
) QueryPlan {
	return QueryPlan{
		TemplateRef: binding.TemplateRef,
		Fingerprint: fingerprint(binding),
		Queries:     queries,
		AggregateOp: op,
		GroupBy:     append([]string(nil), groupBy...),
	}
}

func fingerprint(binding integration.CompiledBinding) string {
	parts := []string{
		binding.TemplateRef,
		binding.Params.Metric,
		binding.Params.NumeratorMetric,
		binding.Params.DenominatorMetric,
		binding.Params.BucketMetric,
		binding.Params.TotalMetric,
		binding.Params.FreeMetric,
		labelSignature(binding.Params.Labels),
		labelSignature(binding.Params.CommonLabels),
		labelSignature(binding.Params.NumeratorLabels),
		labelSignature(binding.Params.DenominatorLabels),
		strings.Join(binding.Params.GroupBy, ","),
	}
	if binding.Params.Quantile != nil {
		parts = append(parts, formatQuantile(*binding.Params.Quantile))
	}
	if binding.Params.RangeSeconds != nil {
		parts = append(parts, strconv.Itoa(*binding.Params.RangeSeconds))
	}
	return strings.Join(parts, "\x1e")
}

func labelSignature(labels []integration.LabelEquality) string {
	if len(labels) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(labels))
	for _, label := range labels {
		pairs = append(pairs, label.Name+"="+label.Value)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func effectiveSelector(common, side []integration.LabelEquality) []integration.LabelEquality {
	out := make([]integration.LabelEquality, 0, len(common)+len(side))
	out = append(out, common...)
	out = append(out, side...)
	return out
}

func selectorSignature(labels []integration.LabelEquality) string {
	return labelSignature(labels)
}

func requireRange(rangeSeconds *int) (int, error) {
	if rangeSeconds == nil || *rangeSeconds <= 0 {
		return 0, fmt.Errorf("%w: 缺少 range_seconds", ErrInvalidParams)
	}
	if *rangeSeconds > maxRangeSeconds {
		return 0, fmt.Errorf("%w: range_seconds 超长", ErrInvalidParams)
	}
	return *rangeSeconds, nil
}

func validateMetric(metric string) error {
	if metric == "" {
		return fmt.Errorf("%w: 缺少 metric", ErrInvalidParams)
	}
	if len(metric) > 200 || !metricPattern.MatchString(metric) {
		return fmt.Errorf("%w: metric 非法", ErrInvalidParams)
	}
	return nil
}

func validateLabels(labels []integration.LabelEquality) ([]integration.LabelEquality, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(labels))
	out := make([]integration.LabelEquality, len(labels))
	copy(out, labels)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, label := range out {
		if err := validateLabelName(label.Name); err != nil {
			return nil, err
		}
		if err := validateLabelValue(label.Value); err != nil {
			return nil, err
		}
		if _, exists := seen[label.Name]; exists {
			return nil, fmt.Errorf("%w: 重复 label %s", ErrInvalidParams, label.Name)
		}
		seen[label.Name] = struct{}{}
	}
	return out, nil
}

func validateGroupBy(groupBy []string) ([]string, error) {
	if len(groupBy) == 0 {
		return nil, nil
	}
	if len(groupBy) > maxGroupBy {
		return nil, fmt.Errorf("%w: group_by 过多", ErrInvalidParams)
	}
	seen := make(map[string]struct{}, len(groupBy))
	out := append([]string(nil), groupBy...)
	sort.Strings(out)
	for _, name := range out {
		if err := validateLabelName(name); err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("%w: 重复 group_by %s", ErrInvalidParams, name)
		}
		seen[name] = struct{}{}
	}
	return out, nil
}

func validateLabelName(name string) error {
	if name == "" || len(name) > 100 || !labelNamePat.MatchString(name) {
		return fmt.Errorf("%w: label 名非法", ErrInvalidParams)
	}
	if isDynamicFocusLabel(name) {
		return fmt.Errorf("%w: 禁止动态 focus label", ErrInvalidParams)
	}
	return nil
}

func validateLabelValue(value string) error {
	if value == "" || len(value) > 200 || !labelValuePat.MatchString(value) {
		return fmt.Errorf("%w: label 值非法", ErrInvalidParams)
	}
	if strings.ContainsAny(value, `"\{}=~!,`) {
		return fmt.Errorf("%w: label 值含注入字符", ErrInvalidParams)
	}
	return nil
}

func isDynamicFocusLabel(name string) bool {
	switch strings.ToLower(name) {
	case "focus", "focus_value", "sandbox_id", "sandboxid", "__name__":
		return true
	default:
		return false
	}
}

func labelSelector(labels []integration.LabelEquality) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%s=%q", label.Name, label.Value))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func wrapGroupBy(groupBy []string, fn, inner string) string {
	if len(groupBy) == 0 {
		return inner
	}
	return fmt.Sprintf("%s by (%s) (%s)", fn, strings.Join(groupBy, ", "), inner)
}

func formatQuantile(q float64) string {
	return strconv.FormatFloat(q, 'f', -1, 64)
}
