// Health/Data 真值表的表驱动覆盖。
//
// 这个 evaluator 是三层页面唯一的 Health/Data 计算入口，所以测试的重点不是"能不能算出
// 一个状态"，而是"永远不会算出一个假绿"：过期、缺失、Forbidden、Unavailable、无法判断
// 新鲜度，全部必须退化成 Unknown/Partial/Stale，而不是复用旧值或默认 Healthy。
package health

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"alert-system/internal/serviceobs/model"
)

var evaluatedAt = time.Date(2026, 8, 22, 2, 9, 30, 0, time.UTC)

func floatPtr(value float64) *float64 { return &value }

// binding 构造一个带绝对阈值的 binding；thresholds 为 nil 表示"没有方向阈值"。
func binding(key string, family model.SignalFamily, semantic model.SignalSemantic, thresholds *model.SignalThresholds) model.SignalBinding {
	return model.SignalBinding{
		Key: key, Label: key, Family: family, Semantic: semantic,
		Scope:   model.SignalBindingScope{Kind: model.ScopeKindService},
		ToolKey: "metrics.query_semantic_signal.v1", OutputField: "series.value",
		Unit: "percent", Aggregation: model.AggregationAvg, Thresholds: thresholds,
	}
}

func above(degraded, critical float64) *model.SignalThresholds {
	return &model.SignalThresholds{
		Mode: model.ThresholdModeAbsolute, Direction: model.ThresholdDirectionAbove,
		Degraded: degraded, Critical: critical,
	}
}

func below(degraded, critical float64) *model.SignalThresholds {
	return &model.SignalThresholds{
		Mode: model.ThresholdModeAbsolute, Direction: model.ThresholdDirectionBelow,
		Degraded: degraded, Critical: critical,
	}
}

func profileWith(bindings ...model.SignalBinding) *model.Profile {
	return &model.Profile{
		SchemaVersion: model.SchemaVersion, ProfileKey: "e2b", ProfileVersion: 1,
		Status: model.ProfileStatusPublished, SignalBindings: bindings,
		Limits: model.ProfileLimits{FreshnessTTLSeconds: 60, DefaultWindowSeconds: 300},
	}
}

// fresh 构造一次成功且新鲜的观测。
func fresh(key string, value float64) SignalObservation {
	return SignalObservation{
		BindingKey: key, Availability: model.SignalAvailabilityAvailable,
		Value: floatPtr(value), ObservedAt: evaluatedAt.Add(-10 * time.Second),
		SourceKey: "prometheus_compatible",
	}
}

func staleObservation(key string, value float64) SignalObservation {
	observation := fresh(key, value)
	// TTL 是 60s，这里刻意超出。
	observation.ObservedAt = evaluatedAt.Add(-5 * time.Minute)
	return observation
}

func unavailable(key string) SignalObservation {
	return SignalObservation{
		BindingKey: key, Availability: model.SignalAvailabilityUnavailable,
		ObservedAt: evaluatedAt.Add(-10 * time.Second), SourceKey: "prometheus_compatible",
		UnknownReason: "telemetry_not_connected",
	}
}

func forbidden(key string) SignalObservation {
	return SignalObservation{
		BindingKey: key, Availability: model.SignalAvailabilityForbidden,
		ObservedAt: evaluatedAt.Add(-10 * time.Second), SourceKey: "openobserve",
		UnknownReason: "missing_permission",
	}
}

func availableSource(key string) SourceObservation {
	return SourceObservation{
		SourceKey: key, State: model.AvailabilityAvailable,
		ObservedAt: evaluatedAt.Add(-10 * time.Second), Freshness: model.FreshnessFresh,
	}
}

// worst-of 顺序：critical > degraded > required 缺失 unknown > healthy。
func TestServiceHealthWorstOfPrecedence(t *testing.T) {
	latency := binding("golden_create_latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	errorRate := binding("golden_create_errors", model.SignalFamilyGolden, model.SemanticErrors, above(5, 15))
	profile := profileWith(latency, errorRate)

	cases := []struct {
		name         string
		observations []SignalObservation
		wantHealth   model.Health
		wantReasons  []string
	}{
		{
			name:         "全部新鲜未越阈值 -> Healthy",
			observations: []SignalObservation{fresh(latency.Key, 800), fresh(errorRate.Key, 1)},
			wantHealth:   model.HealthHealthy,
			wantReasons:  []string{ReasonAllRequiredSignalsHealthy},
		},
		{
			name:         "一个 Degraded -> Degraded",
			observations: []SignalObservation{fresh(latency.Key, 3820), fresh(errorRate.Key, 1)},
			wantHealth:   model.HealthDegraded,
			wantReasons:  []string{"signal_degraded:golden_create_latency"},
		},
		{
			name:         "Critical 压过 Degraded",
			observations: []SignalObservation{fresh(latency.Key, 3820), fresh(errorRate.Key, 20)},
			wantHealth:   model.HealthCritical,
			wantReasons:  []string{"signal_critical:golden_create_errors"},
		},
		{
			// 关键点：Critical 优先于 required 缺失。当前确实在烧，不能因为另一个信号
			// 拿不到就把结论软化成 Unknown。
			name:         "Critical 压过 required 缺失",
			observations: []SignalObservation{fresh(errorRate.Key, 20)},
			wantHealth:   model.HealthCritical,
			wantReasons:  []string{"signal_critical:golden_create_errors"},
		},
		{
			name:         "Degraded 压过 required 缺失",
			observations: []SignalObservation{fresh(latency.Key, 3820)},
			wantHealth:   model.HealthDegraded,
			wantReasons:  []string{"signal_degraded:golden_create_latency"},
		},
		{
			name:         "required 缺失且无越阈值 -> Unknown",
			observations: []SignalObservation{fresh(latency.Key, 800)},
			wantHealth:   model.HealthUnknown,
			wantReasons:  []string{"signal_missing:golden_create_errors"},
		},
		{
			name:         "全部缺失 -> Unknown 且列出全部缺失 binding",
			observations: nil,
			wantHealth:   model.HealthUnknown,
			wantReasons: []string{
				"signal_missing:golden_create_errors",
				"signal_missing:golden_create_latency",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: tc.observations})
			if result.Health.Status != tc.wantHealth {
				t.Fatalf("health=%s want %s (reasons=%v)", result.Health.Status, tc.wantHealth, result.Health.ReasonCodes)
			}
			if !reflect.DeepEqual(result.Health.ReasonCodes, tc.wantReasons) {
				t.Fatalf("reason codes=%v want %v", result.Health.ReasonCodes, tc.wantReasons)
			}
			if result.Health.EvaluatedAt != model.NewTimestamp(evaluatedAt) {
				t.Fatalf("evaluated_at=%s", result.Health.EvaluatedAt)
			}
		})
	}
}

// reason codes 只列产生最终状态的 binding：Degraded 结论里不得混入无关的 healthy 或
// 缺失 binding，否则页面无法定位到底是哪一项在烧。
func TestReasonCodesOnlyListFinalStateBindings(t *testing.T) {
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	errorRate := binding("errors", model.SignalFamilyGolden, model.SemanticErrors, above(5, 15))
	saturation := binding("saturation", model.SignalFamilyGolden, model.SemanticSaturation, above(85, 95))
	profile := profileWith(latency, errorRate, saturation)

	result := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: []SignalObservation{
		fresh(latency.Key, 3820),    // degraded
		fresh(errorRate.Key, 1),     // healthy
		unavailable(saturation.Key), // 缺数
	}})
	if result.Health.Status != model.HealthDegraded {
		t.Fatalf("health=%s want degraded", result.Health.Status)
	}
	want := []string{"signal_degraded:latency"}
	if !reflect.DeepEqual(result.Health.ReasonCodes, want) {
		t.Fatalf("reason codes=%v want %v", result.Health.ReasonCodes, want)
	}
}

// 两种阈值方向都要覆盖，并且边界值（value == degraded / value == critical）算越界。
func TestThresholdDirections(t *testing.T) {
	cases := []struct {
		name       string
		thresholds *model.SignalThresholds
		value      float64
		want       model.SignalHealth
	}{
		{"above 未越界", above(80, 95), 10, model.SignalHealthHealthy},
		{"above 命中 degraded 边界", above(80, 95), 80, model.SignalHealthDegraded},
		{"above 超过 degraded", above(80, 95), 90, model.SignalHealthDegraded},
		{"above 命中 critical 边界", above(80, 95), 95, model.SignalHealthCritical},
		{"above 超过 critical", above(80, 95), 99, model.SignalHealthCritical},
		{"below 未越界", below(90, 50), 99, model.SignalHealthHealthy},
		{"below 命中 degraded 边界", below(90, 50), 90, model.SignalHealthDegraded},
		{"below 低于 degraded", below(90, 50), 70, model.SignalHealthDegraded},
		{"below 命中 critical 边界", below(90, 50), 50, model.SignalHealthCritical},
		{"below 低于 critical", below(90, 50), 10, model.SignalHealthCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := binding("availability_ratio", model.SignalFamilyGolden, model.SemanticErrors, tc.thresholds)
			result := Evaluate(Input{
				Profile: profileWith(target), Now: evaluatedAt,
				Signals: []SignalObservation{fresh(target.Key, tc.value)},
			})
			if len(result.Signals) != 1 {
				t.Fatalf("signal 数量=%d", len(result.Signals))
			}
			if result.Signals[0].Health != tc.want {
				t.Fatalf("signal health=%s want %s", result.Signals[0].Health, tc.want)
			}
		})
	}
}

// 没有方向阈值的信号（Traffic/Rate 是典型）永远 Neutral，不进入 worst-of。
// 但它缺失时仍让 required coverage 不完整——这两件事必须同时成立。
func TestNeutralSignalsDoNotEnterWorstOf(t *testing.T) {
	traffic := binding("golden_create_traffic", model.SignalFamilyGolden, model.SemanticTraffic, nil)
	traffic.BaselineRef = "baseline:e2b:create_traffic:7d_same_hour"
	rate := binding("red_rate", model.SignalFamilyRED, model.SemanticRate, nil)
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	profile := profileWith(traffic, rate, latency)

	// 流量暴涨到很大的数值，仍然只能是 Neutral，绝不能被着红或着绿。
	healthy := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: []SignalObservation{
		fresh(traffic.Key, 99999), fresh(rate.Key, 88888), fresh(latency.Key, 800),
	}})
	if healthy.Health.Status != model.HealthHealthy {
		t.Fatalf("health=%s want healthy (reasons=%v)", healthy.Health.Status, healthy.Health.ReasonCodes)
	}
	for _, signal := range healthy.Signals {
		if signal.Binding.Thresholds == nil && signal.Health != model.SignalHealthNeutral {
			t.Fatalf("%s health=%s，无方向阈值必须 Neutral", signal.Binding.Key, signal.Health)
		}
	}

	// Traffic 缺失仍让 required coverage 不完整 -> 服务 Unknown。
	missing := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: []SignalObservation{
		fresh(rate.Key, 18), fresh(latency.Key, 800),
	}})
	if missing.Health.Status != model.HealthUnknown {
		t.Fatalf("health=%s want unknown", missing.Health.Status)
	}
	if !reflect.DeepEqual(missing.Health.ReasonCodes, []string{"signal_missing:golden_create_traffic"}) {
		t.Fatalf("reason codes=%v", missing.Health.ReasonCodes)
	}

	// 有 baseline_ref 的 Traffic 仍然保留它作为 threshold source，用于显示基线变化。
	for _, signal := range healthy.Signals {
		if signal.Binding.Key == traffic.Key && signal.ThresholdSource != traffic.BaselineRef {
			t.Fatalf("Traffic threshold_source=%q want %q", signal.ThresholdSource, traffic.BaselineRef)
		}
	}
}

// 过期观测不得复用为绿值：signal 自身退化成 Unknown（Neutral 信号保持 Neutral，
// 因为它本来就没有健康意见），并让服务 Health Unknown。
func TestStaleSignalsNeverStayGreen(t *testing.T) {
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	traffic := binding("traffic", model.SignalFamilyGolden, model.SemanticTraffic, nil)
	profile := profileWith(latency, traffic)

	result := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: []SignalObservation{
		staleObservation(latency.Key, 800), // 数值本身是"健康"的，但已经过期
		fresh(traffic.Key, 14.6),
	}})
	if result.Health.Status != model.HealthUnknown {
		t.Fatalf("health=%s want unknown", result.Health.Status)
	}
	if !reflect.DeepEqual(result.Health.ReasonCodes, []string{"signal_stale:latency"}) {
		t.Fatalf("reason codes=%v", result.Health.ReasonCodes)
	}
	staleSignal := result.Signals[0]
	if staleSignal.Freshness != model.FreshnessStale {
		t.Fatalf("freshness=%s want stale", staleSignal.Freshness)
	}
	if staleSignal.Health != model.SignalHealthUnknown {
		t.Fatalf("过期 signal health=%s，绝不能保留旧的 healthy", staleSignal.Health)
	}
	// 值与观测时间仍然保留：页面要能显示"这是 5 分钟前的读数"，而不是凭空清空。
	if staleSignal.Value == nil || *staleSignal.Value != 800 {
		t.Fatalf("过期 signal 应保留原值与 observed_at 供展示")
	}
	if !staleSignal.Eligible && !staleSignal.RequiredCoverageMissing {
		t.Fatalf("过期 signal 必须计入 required coverage 缺口")
	}
}

// 缺失、Forbidden、Unavailable、无法判断新鲜度、以及 available 但值为 null，
// 全部是最小 Unknown：signal 变 Unknown，服务 Health 变 Unknown，其它信号不受影响。
func TestMinimalUnknownCauses(t *testing.T) {
	target := binding("target", model.SignalFamilyUSE, model.SemanticSaturation, above(85, 95))
	healthy := binding("other", model.SignalFamilyGolden, model.SemanticErrors, above(5, 15))
	profile := profileWith(target, healthy)

	valueMissing := fresh(target.Key, 0)
	valueMissing.Value = nil

	freshnessUnknown := fresh(target.Key, 10)
	freshnessUnknown.FreshnessUnknown = true

	cases := []struct {
		name        string
		observation *SignalObservation
		wantReason  string
	}{
		{"缺失", nil, "signal_missing:target"},
		{"Unavailable", ptr(unavailable(target.Key)), "signal_unavailable:target"},
		{"Forbidden", ptr(forbidden(target.Key)), "signal_forbidden:target"},
		{"available 但值为 null", &valueMissing, "signal_value_missing:target"},
		{"无法判断新鲜度", &freshnessUnknown, "signal_freshness_unknown:target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observations := []SignalObservation{fresh(healthy.Key, 1)}
			if tc.observation != nil {
				observations = append(observations, *tc.observation)
			}
			result := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: observations})
			if result.Health.Status != model.HealthUnknown {
				t.Fatalf("health=%s want unknown", result.Health.Status)
			}
			if !reflect.DeepEqual(result.Health.ReasonCodes, []string{tc.wantReason}) {
				t.Fatalf("reason codes=%v want [%s]", result.Health.ReasonCodes, tc.wantReason)
			}
			// 最小降级：另一个信号必须保持它自己的健康判定。
			for _, signal := range result.Signals {
				if signal.Binding.Key == healthy.Key && signal.Health != model.SignalHealthHealthy {
					t.Fatalf("无关信号被连带降级: %+v", signal.Health)
				}
				if signal.Binding.Key == target.Key {
					if signal.Health != model.SignalHealthUnknown {
						t.Fatalf("target health=%s want unknown", signal.Health)
					}
					if signal.UnknownReason == "" {
						t.Fatalf("Unknown signal 必须带 unknown_reason")
					}
				}
			}
		})
	}
}

func ptr(observation SignalObservation) *SignalObservation { return &observation }

// EvidenceRef 的 expired 只属于证据，不得被复用成 signal 或 source 状态。
// signal 侧由 Go 类型隔离（SignalAvailability 没有 expired）；source 侧如果被硬塞进
// 一个 "expired" 字面量，必须 fail closed 成 Unknown -> Data Partial，而不是被当成
// 一次成功查询。
func TestExpiredIsEvidenceOnly(t *testing.T) {
	if model.SignalAvailability(model.EvidenceAvailabilityExpired).IsValid() {
		t.Fatalf("SignalAvailability 不允许 expired")
	}
	if model.Availability(model.EvidenceAvailabilityExpired).IsValid() {
		t.Fatalf("Page View availability 不允许 expired")
	}
	if !model.EvidenceAvailabilityExpired.IsValid() {
		t.Fatalf("EvidenceRef availability 必须允许 expired")
	}

	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	result := Evaluate(Input{
		Profile: profileWith(latency), Now: evaluatedAt,
		Signals: []SignalObservation{fresh(latency.Key, 800)},
		Sources: []SourceObservation{{
			SourceKey: "openobserve",
			State:     model.Availability(model.EvidenceAvailabilityExpired),
			Freshness: model.FreshnessFresh,
		}},
	})
	if result.Data.State != model.DataStatePartial {
		t.Fatalf("非法 source state 必须 fail closed 成 partial, got %s", result.Data.State)
	}
	if len(result.Data.Sources) != 1 {
		t.Fatalf("source 数量=%d", len(result.Data.Sources))
	}
	if result.Data.Sources[0].State != model.AvailabilityUnknown {
		t.Fatalf("非法 source state 应归一成 unknown, got %s", result.Data.Sources[0].State)
	}
	if result.Data.Sources[0].ReasonCode != ReasonSourceStateInvalid {
		t.Fatalf("reason_code=%q want %q", result.Data.Sources[0].ReasonCode, ReasonSourceStateInvalid)
	}
}

// proxy 的 measurement role 与 reason 原样从 Profile binding 带出：页面据此说明
// "这是容量压力 proxy"，而不是把它当真实排队饱和度。
func TestProxyMetadataIsPreservedVerbatim(t *testing.T) {
	const reason = "Phase 1 使用最坏宿主 HugePages 使用率近似服务容量压力。"
	saturation := binding("golden_capacity_saturation", model.SignalFamilyGolden, model.SemanticSaturation, above(85, 95))
	saturation.MeasurementRole = model.MeasurementRoleProxy
	saturation.ProxyReason = reason

	result := Evaluate(Input{
		Profile: profileWith(saturation), Now: evaluatedAt,
		Signals: []SignalObservation{fresh(saturation.Key, 91)},
	})
	signal := result.Signals[0]
	if signal.MeasurementRole != model.MeasurementRoleProxy || signal.ProxyReason != reason {
		t.Fatalf("proxy 元数据被改写: role=%q reason=%q", signal.MeasurementRole, signal.ProxyReason)
	}
	if signal.Health != model.SignalHealthDegraded {
		t.Fatalf("proxy 信号仍要按阈值判定: %s", signal.Health)
	}
	if signal.ThresholdSource != "profile:e2b:v1:golden_capacity_saturation" {
		t.Fatalf("threshold_source=%q", signal.ThresholdSource)
	}

	// adapter 知道更精确的阈值来源时可以显式覆盖，evaluator 原样保留。
	observation := fresh(saturation.Key, 91)
	observation.ThresholdSource = "profile:e2b:v1:golden_capacity_saturation:hugepages_utilization_proxy"
	overridden := Evaluate(Input{
		Profile: profileWith(saturation), Now: evaluatedAt,
		Signals: []SignalObservation{observation},
	})
	if overridden.Signals[0].ThresholdSource != observation.ThresholdSource {
		t.Fatalf("显式 threshold_source 未被保留: %q", overridden.Signals[0].ThresholdSource)
	}
}

// baseline_deviation 模式在 Phase 2 没有 baseline adapter：拿不到偏差就必须 Unknown，
// 绝不能因为"没有可比的基线"而默认判成 Healthy。
func TestBaselineDeviationWithoutBaselineIsUnknown(t *testing.T) {
	target := binding("traffic_deviation", model.SignalFamilyGolden, model.SemanticTraffic,
		&model.SignalThresholds{
			Mode: model.ThresholdModeBaselineDeviation, Direction: model.ThresholdDirectionAbove,
			Degraded: 30, Critical: 60,
		})
	profile := profileWith(target)

	missing := Evaluate(Input{Profile: profile, Now: evaluatedAt,
		Signals: []SignalObservation{fresh(target.Key, 14.6)}})
	if missing.Health.Status != model.HealthUnknown {
		t.Fatalf("health=%s want unknown", missing.Health.Status)
	}
	if !reflect.DeepEqual(missing.Health.ReasonCodes, []string{"signal_baseline_unavailable:traffic_deviation"}) {
		t.Fatalf("reason codes=%v", missing.Health.ReasonCodes)
	}

	withBaseline := fresh(target.Key, 14.6)
	withBaseline.BaselineDeltaPercent = floatPtr(70)
	critical := Evaluate(Input{Profile: profile, Now: evaluatedAt, Signals: []SignalObservation{withBaseline}})
	if critical.Health.Status != model.HealthCritical {
		t.Fatalf("health=%s want critical", critical.Health.Status)
	}
	if critical.Signals[0].BaselineDeltaPercent == nil || *critical.Signals[0].BaselineDeltaPercent != 70 {
		t.Fatalf("baseline delta 未保留")
	}
}

// Profile 缺失 -> Data Unconfigured、Health Unknown，且不产生任何 signal。
func TestProfileAbsentYieldsUnconfigured(t *testing.T) {
	result := Evaluate(Input{Profile: nil, Now: evaluatedAt, Sources: []SourceObservation{
		availableSource("alert_system_registration"),
	}})
	if result.Data.State != model.DataStateUnconfigured {
		t.Fatalf("data=%s want unconfigured", result.Data.State)
	}
	if result.Health.Status != model.HealthUnknown {
		t.Fatalf("health=%s want unknown", result.Health.Status)
	}
	if !reflect.DeepEqual(result.Health.ReasonCodes, []string{ReasonProfileUnconfigured}) {
		t.Fatalf("reason codes=%v", result.Health.ReasonCodes)
	}
	if len(result.Signals) != 0 {
		t.Fatalf("无 Profile 时不应产生 signal: %+v", result.Signals)
	}
	// 已经拿到的 source 事实仍要如实列出，只是整体状态是 Unconfigured。
	if len(result.Data.Sources) != 1 {
		t.Fatalf("source 状态应保留")
	}
}

// Data 优先级：unconfigured > partial > stale > complete。
func TestDataStatusPrecedence(t *testing.T) {
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	profile := profileWith(latency)
	signals := []SignalObservation{fresh(latency.Key, 800)}

	staleSource := availableSource("prometheus_compatible")
	staleSource.Freshness = model.FreshnessStale

	unknownFreshnessSource := availableSource("prometheus_compatible")
	unknownFreshnessSource.Freshness = model.FreshnessUnknown

	emptySource := availableSource("openobserve")
	emptySource.State = model.AvailabilityEmpty

	forbiddenSource := availableSource("openobserve")
	forbiddenSource.State = model.AvailabilityForbidden

	unavailableSource := availableSource("openobserve")
	unavailableSource.State = model.AvailabilityUnavailable

	unknownSource := availableSource("openobserve")
	unknownSource.State = model.AvailabilityUnknown

	cases := []struct {
		name       string
		sources    []SourceObservation
		cacheStale bool
		want       model.DataState
	}{
		{"全部 available fresh -> complete", []SourceObservation{availableSource("a"), availableSource("b")}, false, model.DataStateComplete},
		{"empty 是一次成功查询，不触发 partial", []SourceObservation{availableSource("a"), emptySource}, false, model.DataStateComplete},
		{"forbidden -> partial", []SourceObservation{availableSource("a"), forbiddenSource}, false, model.DataStatePartial},
		{"unavailable -> partial", []SourceObservation{availableSource("a"), unavailableSource}, false, model.DataStatePartial},
		{"unknown -> partial", []SourceObservation{availableSource("a"), unknownSource}, false, model.DataStatePartial},
		{"失败导致的 freshness unknown -> partial", []SourceObservation{unknownFreshnessSource}, false, model.DataStatePartial},
		{"仅有 stale -> stale", []SourceObservation{availableSource("a"), staleSource}, false, model.DataStateStale},
		{"cache snapshot stale -> stale", []SourceObservation{availableSource("a")}, true, model.DataStateStale},
		{"partial 压过 stale", []SourceObservation{staleSource, forbiddenSource}, false, model.DataStatePartial},
		{"partial 压过 cache stale", []SourceObservation{forbiddenSource}, true, model.DataStatePartial},
		{"没有 source -> complete（没有任何来源失败）", nil, false, model.DataStateComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Evaluate(Input{
				Profile: profile, Now: evaluatedAt, Signals: signals,
				Sources: tc.sources, CacheSnapshotStale: tc.cacheStale,
			})
			if result.Data.State != tc.want {
				t.Fatalf("data=%s want %s", result.Data.State, tc.want)
			}
			if result.Data.Sources == nil {
				t.Fatalf("sources 必须是 []，不能是 nil")
			}
		})
	}
}

// Health 与 Data 正交：当前 Critical 与 observed relation Stale 同时成立时，
// Health 仍然是 Critical，Data 是 Stale。任何一侧都不能把另一侧改写掉。
func TestHealthAndDataAreOrthogonal(t *testing.T) {
	errorRate := binding("red_errors", model.SignalFamilyRED, model.SemanticErrors, above(2, 5))
	profile := profileWith(errorRate)

	staleRelationSource := availableSource("topology")
	staleRelationSource.Freshness = model.FreshnessStale
	staleRelationSource.ReasonCode = "observed_relation_stale"

	result := Evaluate(Input{
		Profile: profile, Now: evaluatedAt,
		Signals: []SignalObservation{fresh(errorRate.Key, 7.2)},
		Sources: []SourceObservation{availableSource("prometheus_compatible"), staleRelationSource},
	})
	if result.Health.Status != model.HealthCritical {
		t.Fatalf("health=%s want critical", result.Health.Status)
	}
	if result.Data.State != model.DataStateStale {
		t.Fatalf("data=%s want stale", result.Data.State)
	}
	if !reflect.DeepEqual(result.Health.ReasonCodes, []string{"signal_critical:red_errors"}) {
		t.Fatalf("reason codes=%v", result.Health.ReasonCodes)
	}
}

// source 状态原样投影成 model.SourceStatus，包含 observed_at 与 reason code。
func TestSourceStatusProjection(t *testing.T) {
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	observedAt := evaluatedAt.Add(-30 * time.Second)
	result := Evaluate(Input{
		Profile: profileWith(latency), Now: evaluatedAt,
		Signals: []SignalObservation{fresh(latency.Key, 800)},
		Sources: []SourceObservation{
			{SourceKey: "prometheus_compatible", State: model.AvailabilityUnavailable,
				Freshness: model.FreshnessUnknown, ObservedAt: observedAt,
				ReasonCode: "telemetry_not_connected", Retryable: true},
			{SourceKey: "alert_system_domain", State: model.AvailabilityAvailable,
				Freshness: model.FreshnessFresh, ObservedAt: observedAt},
		},
	})
	// 按 source key 稳定排序，避免同一份数据两次请求顺序不同。
	if result.Data.Sources[0].SourceKey != "alert_system_domain" {
		t.Fatalf("source 未按 key 稳定排序: %+v", result.Data.Sources)
	}
	telemetry := result.Data.Sources[1]
	if telemetry.State != model.AvailabilityUnavailable || telemetry.Freshness != model.FreshnessUnknown {
		t.Fatalf("source 状态被改写: %+v", telemetry)
	}
	if telemetry.ReasonCode != "telemetry_not_connected" || !telemetry.Retryable {
		t.Fatalf("source reason/retryable 被改写: %+v", telemetry)
	}
	if telemetry.ObservedAt != model.NewTimestamp(observedAt) {
		t.Fatalf("observed_at=%s，必须是真实观测时间而不是当前时间", telemetry.ObservedAt)
	}
}

// 占位 observed_at 绝不能用当前时间：那会把"我还没观测到"伪装成"刚刚观测过"。
func TestUnknownSignalKeepsRealObservedAt(t *testing.T) {
	target := binding("target", model.SignalFamilyUSE, model.SemanticSaturation, above(85, 95))
	observedAt := evaluatedAt.Add(-17 * time.Minute)
	observation := unavailable(target.Key)
	observation.ObservedAt = observedAt

	result := Evaluate(Input{Profile: profileWith(target), Now: evaluatedAt,
		Signals: []SignalObservation{observation}})
	signal := result.Signals[0]
	if signal.ObservedAt != observedAt {
		t.Fatalf("observed_at=%s want %s", signal.ObservedAt, observedAt)
	}
	if signal.Value != nil {
		t.Fatalf("Unknown signal 的 value 必须是 nil")
	}
	if signal.Freshness != model.FreshnessUnknown {
		t.Fatalf("freshness=%s want unknown", signal.Freshness)
	}
}

// Profile binding 是权威集合：adapter 返回一个没有声明的 binding 时必须被丢弃，
// 否则页面会出现 Profile 里根本没配的信号。
func TestUndeclaredObservationsAreIgnored(t *testing.T) {
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	result := Evaluate(Input{
		Profile: profileWith(latency), Now: evaluatedAt,
		Signals: []SignalObservation{fresh(latency.Key, 800), fresh("not_in_profile", 99)},
	})
	if len(result.Signals) != 1 || result.Signals[0].Binding.Key != latency.Key {
		t.Fatalf("未声明的观测应被丢弃: %+v", result.Signals)
	}
	if result.Health.Status != model.HealthHealthy {
		t.Fatalf("health=%s", result.Health.Status)
	}
}

// signal 结果按 Profile 声明顺序返回，保证同一份输入两次求值输出一致。
func TestSignalOrderFollowsProfileDeclaration(t *testing.T) {
	first := binding("z_first_declared", model.SignalFamilyGolden, model.SemanticLatency, above(1, 2))
	second := binding("a_second_declared", model.SignalFamilyGolden, model.SemanticErrors, above(1, 2))
	result := Evaluate(Input{
		Profile: profileWith(first, second), Now: evaluatedAt,
		Signals: []SignalObservation{fresh(second.Key, 0), fresh(first.Key, 0)},
	})
	if result.Signals[0].Binding.Key != first.Key || result.Signals[1].Binding.Key != second.Key {
		t.Fatalf("signal 顺序应跟随 Profile 声明: %s,%s",
			result.Signals[0].Binding.Key, result.Signals[1].Binding.Key)
	}
}

// Fault/Incident 数量不改写 Health：evaluator 的入参里根本没有它们，这条测试锁住这个
// 边界，防止未来有人"顺手"把领域计数塞进健康判定。
func TestDomainCountsCannotInfluenceHealth(t *testing.T) {
	inputType := reflect.TypeOf(Input{})
	for i := 0; i < inputType.NumField(); i++ {
		name := inputType.Field(i).Name
		for _, forbidden := range []string{"Fault", "Incident", "Impact", "Alert", "Domain"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("Input 不得携带领域字段 %s：Fault/Incident 数量不能改写 Health", name)
			}
		}
	}
}

// reason codes 受 Page View schema 的 maxItems=20 约束：超出时确定性截断并显式标记，
// 不能静默丢弃也不能超限。
func TestReasonCodesRespectSchemaCap(t *testing.T) {
	bindings := make([]model.SignalBinding, 0, 30)
	for i := 0; i < 30; i++ {
		bindings = append(bindings, binding(
			"binding_"+string(rune('a'+i%26))+string(rune('0'+i/26)),
			model.SignalFamilyUSE, model.SemanticUtilization, above(1, 2)))
	}
	result := Evaluate(Input{Profile: profileWith(bindings...), Now: evaluatedAt})
	if result.Health.Status != model.HealthUnknown {
		t.Fatalf("health=%s want unknown", result.Health.Status)
	}
	if len(result.Health.ReasonCodes) != MaxReasonCodes {
		t.Fatalf("reason codes=%d want %d", len(result.Health.ReasonCodes), MaxReasonCodes)
	}
	last := result.Health.ReasonCodes[len(result.Health.ReasonCodes)-1]
	if last != ReasonCodesTruncated {
		t.Fatalf("截断时最后一项应为 %q, got %q", ReasonCodesTruncated, last)
	}
	for _, code := range result.Health.ReasonCodes {
		if len(code) > 128 {
			t.Fatalf("reason code 超过 schema maxLength: %q", code)
		}
	}
}

// evaluator 是纯函数：同一份输入两次求值结果完全一致，且不改写入参。
func TestEvaluateIsPureAndDeterministic(t *testing.T) {
	latency := binding("latency", model.SignalFamilyGolden, model.SemanticLatency, above(3000, 8000))
	traffic := binding("traffic", model.SignalFamilyGolden, model.SemanticTraffic, nil)
	profile := profileWith(latency, traffic)
	signals := []SignalObservation{staleObservation(latency.Key, 800), unavailable(traffic.Key)}
	sources := []SourceObservation{availableSource("a")}

	input := Input{Profile: profile, Now: evaluatedAt, Signals: signals, Sources: sources}
	first := Evaluate(input)
	second := Evaluate(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同一输入两次求值结果不一致")
	}
	if len(signals) != 2 || signals[0].Value == nil || *signals[0].Value != 800 {
		t.Fatalf("Evaluate 改写了入参")
	}
	if len(profile.SignalBindings) != 2 {
		t.Fatalf("Evaluate 改写了 Profile")
	}
}
