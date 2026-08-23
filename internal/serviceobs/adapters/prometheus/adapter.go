package prometheus

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/observe"
	"alert-system/internal/serviceobs/provider"
)

const (
	queryInstantPath = "/api/v1/query"
	queryRangePath   = "/api/v1/query_range"
	defaultStep      = 60
)

// Adapter 实现 SemanticSignalReader。
type Adapter struct {
	client              *provider.Client
	queryLimitSupported bool
	now                 func() time.Time
}

// Options 构造 Prometheus adapter。
type Options struct {
	Client              *provider.Client
	QueryLimitSupported bool
	Now                 func() time.Time
}

// NewAdapter 构造 adapter。client 为 nil 时 ReadSignals 返回 not connected。
func NewAdapter(opts Options) *Adapter {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{
		client:              opts.Client,
		queryLimitSupported: opts.QueryLimitSupported,
		now:                 now,
	}
}

// ReadSignals 读取 compiled bindings 对应的语义指标。
func (a *Adapter) ReadSignals(ctx context.Context, req serviceobs.SignalRequest) (serviceobs.SignalFacts, error) {
	readAt := a.now().UTC()
	if a.client == nil {
		return serviceobs.SignalFacts{
			Signals: a.notConnectedSignals(req.Bindings),
			Source: health.SourceObservation{
				SourceKey:  serviceobs.SourceKeyPrometheus,
				State:      model.AvailabilityUnavailable,
				Freshness:  model.FreshnessUnknown,
				ReasonCode: serviceobs.ReasonPrometheusNotConnected,
				Retryable:  false,
			},
			ReadAt: readAt,
		}, nil
	}

	bindings := filterBindings(req)
	if len(bindings) == 0 {
		return serviceobs.SignalFacts{
			Signals: []health.SignalObservation{},
			Source: health.SourceObservation{
				SourceKey: serviceobs.SourceKeyPrometheus,
				State:     model.AvailabilityAvailable,
				Freshness: model.FreshnessFresh,
			},
			ReadAt: readAt,
		}, nil
	}

	type execGroup struct {
		fingerprint string
		plan        QueryPlan
		bindings    []integration.CompiledBinding
	}

	groups := make([]*execGroup, 0, len(bindings))
	byFingerprint := map[string]*execGroup{}
	for _, binding := range bindings {
		if err := validateBinding(binding); err != nil {
			return serviceobs.SignalFacts{}, err
		}
		plan, err := BuildQueryPlan(binding)
		if err != nil {
			return serviceobs.SignalFacts{}, err
		}
		group, ok := byFingerprint[plan.Fingerprint]
		if !ok {
			group = &execGroup{fingerprint: plan.Fingerprint, plan: plan}
			byFingerprint[plan.Fingerprint] = group
			groups = append(groups, group)
		}
		group.bindings = append(group.bindings, binding)
	}

	wantSparkline := req.Purpose == serviceobs.PurposeOverview ||
		req.Purpose == serviceobs.PurposeFocusedWorkspace

	results := make(map[string]groupResult, len(groups))
	for _, group := range groups {
		result, err := a.executePlan(ctx, group.plan, req, wantSparkline)
		if err != nil {
			result.err = err
		}
		results[group.fingerprint] = result
	}

	signals := make([]health.SignalObservation, 0, len(bindings))
	var (
		successCount int
		failureCount int
		emptyCount   int
		partialCount int
		authFailures int
		latestObs    time.Time
	)

	for _, binding := range bindings {
		plan, _ := BuildQueryPlan(binding)
		result := results[plan.Fingerprint]
		signal, partial, err := a.projectBinding(binding, plan, result, req)
		if err != nil {
			failureCount++
			if errors.Is(err, observe.ErrUpstreamAuthFailed) {
				authFailures++
			}
			signals = append(signals, unknownSignal(binding, reasonForBindingError(err), !errors.Is(err, observe.ErrUpstreamAuthFailed)))
			continue
		}
		if signal.Availability == model.SignalAvailabilityAvailable {
			successCount++
			if !signal.ObservedAt.IsZero() && signal.ObservedAt.After(latestObs) {
				latestObs = signal.ObservedAt
			}
		} else if signal.UnknownReason == serviceobs.ReasonNoSeries {
			emptyCount++
		} else {
			failureCount++
		}
		if partial {
			partialCount++
		}
		signals = append(signals, signal)
	}

	if failureCount == len(bindings) && authFailures == failureCount && failureCount > 0 {
		return serviceobs.SignalFacts{}, observe.ErrUpstreamAuthFailed
	}

	source := a.buildSource(successCount, failureCount, emptyCount, partialCount, authFailures, len(bindings), latestObs)
	return serviceobs.SignalFacts{
		Signals: signals,
		Source:  source,
		ReadAt:  readAt,
	}, nil
}

type groupResult struct {
	instant   map[string]ParsedResponse
	rangeData map[string]ParsedResponse
	err       error
}

func (a *Adapter) executePlan(
	ctx context.Context,
	plan QueryPlan,
	req serviceobs.SignalRequest,
	wantSparkline bool,
) (groupResult, error) {
	out := groupResult{
		instant:   map[string]ParsedResponse{},
		rangeData: map[string]ParsedResponse{},
	}
	for _, query := range plan.Queries {
		parsed, err := a.queryInstant(ctx, query.Expr, req)
		if err != nil {
			return out, err
		}
		out.instant[query.Role] = parsed
		if wantSparkline {
			rangeParsed, err := a.queryRange(ctx, query.Expr, req)
			if err != nil {
				return out, err
			}
			out.rangeData[query.Role] = rangeParsed
		}
	}
	return out, nil
}

func (a *Adapter) queryInstant(ctx context.Context, expr string, req serviceobs.SignalRequest) (ParsedResponse, error) {
	form := url.Values{}
	form.Set("query", expr)
	if !req.To.IsZero() {
		form.Set("time", formatPromTime(req.To))
	}
	if a.queryLimitSupported && req.Limits.MaxSeries > 0 {
		form.Set("limit", strconv.Itoa(req.Limits.MaxSeries))
	}
	resp, err := a.client.PostForm(ctx, queryInstantPath, form)
	if err != nil {
		return ParsedResponse{}, MapProviderError(err)
	}
	if err := MapHTTPStatus(resp.StatusCode); err != nil {
		return ParsedResponse{}, err
	}
	if resp.StatusCode != 200 {
		return ParsedResponse{}, fmt.Errorf("%w: status=%d", ErrUpstreamReadFailed, resp.StatusCode)
	}
	return ParseQueryResponse(resp.Body, req.Limits.MaxSeries)
}

func (a *Adapter) queryRange(ctx context.Context, expr string, req serviceobs.SignalRequest) (ParsedResponse, error) {
	start, end := rangeWindow(req)
	form := url.Values{}
	form.Set("query", expr)
	form.Set("start", formatPromTime(start))
	form.Set("end", formatPromTime(end))
	form.Set("step", strconv.Itoa(stepSeconds(req)))
	if a.queryLimitSupported && req.Limits.MaxSeries > 0 {
		form.Set("limit", strconv.Itoa(req.Limits.MaxSeries))
	}
	resp, err := a.client.PostForm(ctx, queryRangePath, form)
	if err != nil {
		return ParsedResponse{}, MapProviderError(err)
	}
	if err := MapHTTPStatus(resp.StatusCode); err != nil {
		return ParsedResponse{}, err
	}
	if resp.StatusCode != 200 {
		return ParsedResponse{}, fmt.Errorf("%w: status=%d", ErrUpstreamReadFailed, resp.StatusCode)
	}
	return ParseQueryResponse(resp.Body, req.Limits.MaxSeries)
}

func (a *Adapter) projectBinding(
	binding integration.CompiledBinding,
	plan QueryPlan,
	result groupResult,
	req serviceobs.SignalRequest,
) (health.SignalObservation, bool, error) {
	if result.err != nil {
		return unknownSignal(binding, reasonForBindingError(result.err), !errors.Is(result.err, observe.ErrUpstreamAuthFailed)), false, result.err
	}
	switch plan.TemplateRef {
	case "gauge_ratio_percent.v1":
		return a.projectGaugeRatio(binding, plan, result, req)
	case "counter_ratio_percent.v1":
		return a.projectCounterRatio(binding, plan, result, req)
	default:
		return a.projectSingle(binding, plan, result, req)
	}
}

func (a *Adapter) projectSingle(
	binding integration.CompiledBinding,
	plan QueryPlan,
	result groupResult,
	req serviceobs.SignalRequest,
) (health.SignalObservation, bool, error) {
	parsed := result.instant["value"]
	if len(parsed.Series) == 0 && parsed.Scalar == nil {
		return unknownSignal(binding, serviceobs.ReasonNoSeries, false), false, nil
	}
	value, observedAt, sparkline, err := aggregateSeries(parsed, result.rangeData["value"], plan)
	if err != nil {
		return unknownSignal(binding, serviceobs.ReasonSourceReadFailed, true), false, err
	}
	if value == nil {
		return unknownSignal(binding, serviceobs.ReasonNoSeries, false), false, nil
	}
	return health.SignalObservation{
		BindingKey:   binding.Key,
		Availability: model.SignalAvailabilityAvailable,
		Value:        value,
		ObservedAt:   observedAt,
		SourceKey:    serviceobs.SourceKeyPrometheus,
		Sparkline:    sparkline,
	}, false, nil
}

func (a *Adapter) projectCounterRatio(
	binding integration.CompiledBinding,
	plan QueryPlan,
	result groupResult,
	req serviceobs.SignalRequest,
) (health.SignalObservation, bool, error) {
	num := result.instant["numerator"]
	den := result.instant["denominator"]
	if len(num.Series) == 0 && len(den.Series) == 0 {
		return unknownSignal(binding, serviceobs.ReasonNoSeries, false), false, nil
	}
	ratios, observedAt, partial := computeCounterRatios(num, den, plan.GroupBy)
	if len(ratios) == 0 {
		return unknownSignal(binding, serviceobs.ReasonNoSeries, false), false, nil
	}
	worst, _ := worstOf(ratios)
	sparkline := counterRatioSparkline(result.rangeData["numerator"], result.rangeData["denominator"], plan.GroupBy)
	return health.SignalObservation{
		BindingKey:       binding.Key,
		Availability:     model.SignalAvailabilityAvailable,
		Value:            &worst,
		ObservedAt:       observedAt,
		SourceKey:        serviceobs.SourceKeyPrometheus,
		Sparkline:        sparkline,
		FreshnessUnknown: partial,
	}, partial, nil
}

func (a *Adapter) projectGaugeRatio(
	binding integration.CompiledBinding,
	plan QueryPlan,
	result groupResult,
	req serviceobs.SignalRequest,
) (health.SignalObservation, bool, error) {
	total := result.instant["total"]
	free := result.instant["free"]
	if len(total.Series) == 0 && len(free.Series) == 0 {
		return unknownSignal(binding, serviceobs.ReasonNoSeries, false), false, nil
	}
	ratios, observedAt, partial, allInvalid := computeGaugeRatios(total, free, plan.GroupBy)
	if allInvalid {
		return health.SignalObservation{
			BindingKey:    binding.Key,
			Availability:  model.SignalAvailabilityUnknown,
			SourceKey:     serviceobs.SourceKeyPrometheus,
			UnknownReason: serviceobs.ReasonSourceReadFailed,
		}, false, nil
	}
	if len(ratios) == 0 {
		return unknownSignal(binding, serviceobs.ReasonNoSeries, false), false, nil
	}
	worst, _ := worstOf(ratios)
	sparkline := gaugeRatioSparkline(result.rangeData["total"], result.rangeData["free"], plan.GroupBy)
	return health.SignalObservation{
		BindingKey:       binding.Key,
		Availability:     model.SignalAvailabilityAvailable,
		Value:            &worst,
		ObservedAt:       observedAt,
		SourceKey:        serviceobs.SourceKeyPrometheus,
		Sparkline:        sparkline,
		FreshnessUnknown: partial,
	}, partial, nil
}

func computeGaugeRatios(total, free ParsedResponse, groupBy []string) ([]float64, time.Time, bool, bool) {
	totalByKey := indexSeries(total.Series, groupBy)
	freeByKey := indexSeries(free.Series, groupBy)
	keys := unionKeys(totalByKey, freeByKey)
	ratios := make([]float64, 0, len(keys))
	var observedAt time.Time
	partial := false
	validCount := 0
	for _, key := range keys {
		tSeries, hasTotal := totalByKey[key]
		fSeries, hasFree := freeByKey[key]
		if !hasTotal || !hasFree {
			partial = true
			continue
		}
		if tSeries.Value <= 0 {
			partial = true
			continue
		}
		ratio := (tSeries.Value - fSeries.Value) / tSeries.Value * 100
		if ratio < 0 {
			ratio = 0
		}
		ratios = append(ratios, ratio)
		validCount++
		observedAt = laterOf(observedAt, tSeries.Timestamp, fSeries.Timestamp)
	}
	return ratios, observedAt, partial, validCount == 0 && len(keys) > 0
}

func computeCounterRatios(num, den ParsedResponse, groupBy []string) ([]float64, time.Time, bool) {
	numByKey := indexSeries(num.Series, groupBy)
	denByKey := indexSeries(den.Series, groupBy)
	keys := unionKeys(numByKey, denByKey)
	ratios := make([]float64, 0, len(keys))
	var observedAt time.Time
	partial := false
	for _, key := range keys {
		nSeries, hasNum := numByKey[key]
		dSeries, hasDen := denByKey[key]
		if !hasNum || !hasDen || dSeries.Value == 0 {
			partial = true
			continue
		}
		ratio := nSeries.Value / dSeries.Value * 100
		ratios = append(ratios, ratio)
		observedAt = laterOf(observedAt, nSeries.Timestamp, dSeries.Timestamp)
	}
	return ratios, observedAt, partial
}

func aggregateSeries(
	instant ParsedResponse,
	rangeData ParsedResponse,
	plan QueryPlan,
) (*float64, time.Time, []float64, error) {
	if instant.Scalar != nil {
		value := instant.Scalar.Value
		sparkline := extractSparkline(rangeData.ScalarSeries())
		return &value, instant.Scalar.Timestamp, sparkline, nil
	}
	if len(instant.Series) == 0 {
		return nil, time.Time{}, nil, nil
	}
	values := make([]float64, 0, len(instant.Series))
	var observedAt time.Time
	for _, series := range instant.Series {
		values = append(values, series.Value)
		if series.Timestamp.After(observedAt) {
			observedAt = series.Timestamp
		}
	}
	worst, _ := worstOf(values)
	sparkline := sparklineFromRange(rangeData, plan)
	return &worst, observedAt, sparkline, nil
}

func sparklineFromRange(rangeData ParsedResponse, plan QueryPlan) []float64 {
	if len(rangeData.Series) == 0 {
		return nil
	}
	// 对多 series 取每个时间步的 worst-of。
	stepCount := 0
	for _, series := range rangeData.Series {
		if len(series.Points) > stepCount {
			stepCount = len(series.Points)
		}
	}
	if stepCount == 0 {
		return nil
	}
	out := make([]float64, 0, stepCount)
	for i := 0; i < stepCount; i++ {
		values := make([]float64, 0, len(rangeData.Series))
		for _, series := range rangeData.Series {
			if i < len(series.Points) {
				values = append(values, series.Points[i].Value)
			}
		}
		if len(values) == 0 {
			continue
		}
		worst, _ := worstOf(values)
		out = append(out, worst)
	}
	return out
}

func gaugeRatioSparkline(total, free ParsedResponse, groupBy []string) []float64 {
	if len(total.Series) == 0 || len(free.Series) == 0 {
		return nil
	}
	totalByKey := indexSeries(total.Series, groupBy)
	freeByKey := indexSeries(free.Series, groupBy)
	stepCount := maxMatrixLen(total.Series, free.Series)
	if stepCount == 0 {
		return nil
	}
	out := make([]float64, 0, stepCount)
	for i := 0; i < stepCount; i++ {
		ratios := make([]float64, 0)
		for key, tSeries := range totalByKey {
			fSeries, ok := freeByKey[key]
			if !ok || i >= len(tSeries.Points) || i >= len(fSeries.Points) {
				continue
			}
			totalVal := tSeries.Points[i].Value
			freeVal := fSeries.Points[i].Value
			if totalVal <= 0 {
				continue
			}
			ratio := (totalVal - freeVal) / totalVal * 100
			if ratio < 0 {
				ratio = 0
			}
			ratios = append(ratios, ratio)
		}
		if len(ratios) == 0 {
			continue
		}
		worst, _ := worstOf(ratios)
		out = append(out, worst)
	}
	return out
}

func counterRatioSparkline(num, den ParsedResponse, groupBy []string) []float64 {
	if len(num.Series) == 0 || len(den.Series) == 0 {
		return nil
	}
	numByKey := indexSeries(num.Series, groupBy)
	denByKey := indexSeries(den.Series, groupBy)
	stepCount := maxMatrixLen(num.Series, den.Series)
	if stepCount == 0 {
		return nil
	}
	out := make([]float64, 0, stepCount)
	for i := 0; i < stepCount; i++ {
		ratios := make([]float64, 0)
		for key, nSeries := range numByKey {
			dSeries, ok := denByKey[key]
			if !ok || i >= len(nSeries.Points) || i >= len(dSeries.Points) {
				continue
			}
			denVal := dSeries.Points[i].Value
			if denVal == 0 {
				continue
			}
			ratios = append(ratios, nSeries.Points[i].Value/denVal*100)
		}
		if len(ratios) == 0 {
			continue
		}
		worst, _ := worstOf(ratios)
		out = append(out, worst)
	}
	return out
}

func (p ParsedResponse) ScalarSeries() []SampleSeries {
	if p.Scalar == nil {
		return nil
	}
	return []SampleSeries{{
		Value:     p.Scalar.Value,
		Timestamp: p.Scalar.Timestamp,
		Points:    []SamplePoint{{Timestamp: p.Scalar.Timestamp, Value: p.Scalar.Value}},
	}}
}

func extractSparkline(series []SampleSeries) []float64 {
	if len(series) == 0 || len(series[0].Points) == 0 {
		return nil
	}
	out := make([]float64, 0, len(series[0].Points))
	for _, point := range series[0].Points {
		out = append(out, point.Value)
	}
	return out
}

func indexSeries(series []SampleSeries, groupBy []string) map[string]SampleSeries {
	out := make(map[string]SampleSeries, len(series))
	for _, item := range series {
		out[groupKey(item.Labels, groupBy)] = item
	}
	return out
}

func groupKey(labels map[string]string, groupBy []string) string {
	if len(groupBy) == 0 {
		return ""
	}
	parts := make([]string, 0, len(groupBy))
	for _, key := range groupBy {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, "\x1e")
}

func unionKeys(maps ...map[string]SampleSeries) []string {
	seen := map[string]struct{}{}
	for _, m := range maps {
		for key := range m {
			seen[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func worstOf(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	worst := values[0]
	for _, value := range values[1:] {
		if value > worst {
			worst = value
		}
	}
	return worst, true
}

func maxMatrixLen(series ...[]SampleSeries) int {
	maxLen := 0
	for _, items := range series {
		for _, item := range items {
			if len(item.Points) > maxLen {
				maxLen = len(item.Points)
			}
		}
	}
	return maxLen
}

func reasonForBindingError(err error) string {
	switch {
	case errors.Is(err, observe.ErrUpstreamAuthFailed):
		return serviceobs.ReasonUpstreamAuthFailed
	case errors.Is(err, observe.ErrUpstreamRateLimited):
		return serviceobs.ReasonUpstreamRateLimited
	case errors.Is(err, observe.ErrResponseTooLarge):
		return serviceobs.ReasonResponseTooLarge
	case errors.Is(err, observe.ErrCardinalityExceeded):
		return serviceobs.ReasonCardinalityExceeded
	default:
		return serviceobs.ReasonSourceReadFailed
	}
}

func (a *Adapter) buildSource(success, failure, empty, partial, authFailures, total int, observedAt time.Time) health.SourceObservation {
	source := health.SourceObservation{
		SourceKey: serviceobs.SourceKeyPrometheus,
		Freshness: model.FreshnessFresh,
	}
	if !observedAt.IsZero() {
		source.ObservedAt = observedAt
	}
	switch {
	case failure == total && authFailures == failure && failure > 0:
		source.State = model.AvailabilityUnavailable
		source.Freshness = model.FreshnessUnknown
		source.ReasonCode = serviceobs.ReasonUpstreamAuthFailed
		source.Retryable = false
	case failure == total:
		source.State = model.AvailabilityUnavailable
		source.Freshness = model.FreshnessUnknown
		source.ReasonCode = serviceobs.ReasonSourceReadFailed
		source.Retryable = true
	case failure > 0 || partial > 0:
		source.State = model.AvailabilityUnknown
		source.Freshness = model.FreshnessUnknown
		source.ReasonCode = serviceobs.ReasonSourceReadFailed
		source.Retryable = true
	case success == 0 && empty == total:
		source.State = model.AvailabilityEmpty
		source.Freshness = model.FreshnessUnknown
		source.ReasonCode = serviceobs.ReasonNoSeries
		source.Retryable = false
	default:
		source.State = model.AvailabilityAvailable
	}
	return source
}

func (a *Adapter) notConnectedSignals(bindings []integration.CompiledBinding) []health.SignalObservation {
	out := make([]health.SignalObservation, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, unknownSignal(binding, serviceobs.ReasonPrometheusNotConnected, false))
	}
	return out
}

func unknownSignal(binding integration.CompiledBinding, reason string, retryable bool) health.SignalObservation {
	_ = retryable
	return health.SignalObservation{
		BindingKey:    binding.Key,
		Availability:  model.SignalAvailabilityUnknown,
		SourceKey:     serviceobs.SourceKeyPrometheus,
		UnknownReason: reason,
	}
}

func filterBindings(req serviceobs.SignalRequest) []integration.CompiledBinding {
	if len(req.Bindings) == 0 {
		return nil
	}
	if len(req.BindingKeys) == 0 {
		return append([]integration.CompiledBinding(nil), req.Bindings...)
	}
	allowed := map[string]struct{}{}
	for _, key := range req.BindingKeys {
		allowed[key] = struct{}{}
	}
	out := make([]integration.CompiledBinding, 0, len(req.Bindings))
	for _, binding := range req.Bindings {
		if _, ok := allowed[binding.Key]; ok {
			out = append(out, binding)
		}
	}
	return out
}

func rangeWindow(req serviceobs.SignalRequest) (time.Time, time.Time) {
	if !req.From.IsZero() && !req.To.IsZero() {
		return req.From.UTC(), req.To.UTC()
	}
	end := req.To.UTC()
	if end.IsZero() {
		end = time.Now().UTC()
	}
	window := req.WindowSeconds
	if window <= 0 {
		window = 3600
	}
	return end.Add(-time.Duration(window) * time.Second), end
}

func stepSeconds(req serviceobs.SignalRequest) int {
	window := req.WindowSeconds
	if window <= 0 {
		window = 3600
	}
	step := window / 60
	if step < defaultStep {
		step = defaultStep
	}
	if step > 300 {
		step = 300
	}
	return step
}

func formatPromTime(ts time.Time) string {
	return strconv.FormatFloat(float64(ts.Unix())+float64(ts.Nanosecond())/1e9, 'f', 3, 64)
}

func laterOf(current time.Time, candidates ...time.Time) time.Time {
	out := current
	for _, candidate := range candidates {
		if candidate.After(out) {
			out = candidate
		}
	}
	return out
}

// Ensure Adapter implements SemanticSignalReader.
var _ serviceobs.SemanticSignalReader = (*Adapter)(nil)
