// Package health 是三层 /services 页面唯一的 Health/Data 计算入口。
//
// 目录、总览和工作区都调用同一个 Evaluate：前端不重算，各 projector 也不重算。这样
// "同一个服务在三个页面上健康不一致"这类问题在结构上就不可能出现。
//
// 两条正交的真值表：
//
//	Health（信号运行健康，worst-of）
//	  1. 只有 availability=available 且 observed_at 未超过 Profile freshness TTL 的
//	     signal 才 eligible；
//	  2. 任一 eligible signal Critical -> 服务 Critical；
//	  3. 否则任一 eligible signal Degraded -> 服务 Degraded；
//	  4. 否则任一 required signal 缺失/Forbidden/Unavailable/freshness stale|unknown
//	     -> 服务 Unknown；
//	  5. 只有全部 required signal 可用且未越阈值 -> Healthy；
//	  6. 没有方向阈值的信号（Traffic/Rate 是典型）恒 Neutral，不进入 worst-of，但它
//	     缺失时仍让 required coverage 不完整；
//	  7. Fault/Incident 数量不改写 Health——Input 里根本没有这些字段；
//	  8. reason code 必须带产生最终状态的 binding key，不允许综合分数。
//
//	Data（数据覆盖与新鲜度）
//	  profile absent                                        -> unconfigured
//	  任一 source forbidden/unavailable/unknown，或失败导致的
//	  freshness=unknown                                     -> partial
//	  否则任一 source freshness=stale 或 cache snapshot stale -> stale
//	  否则                                                   -> complete
//
// Profile v1 没有 optional signal 字段，所以所有 signal_bindings 都是 required。
// availability=empty 是一次成功查询，只作用于对应 section，不把 Data 变成 Partial。
//
// 边界说明：cache miss 的占位语义属于 snapshot cache 层（它要保证占位的 observed_at
// 取真实时间而不是当前时间），不在这里表达。本包只接受 CacheSnapshotStale 这一个 cache
// 输入，因此一次 cache miss 不可能被误报成一次 source failure。
package health

import (
	"fmt"
	"sort"
	"time"

	"alert-system/internal/serviceobs/model"
)

// reason code 前缀与固定码。它们进 Page View 的 health.reason_codes，前端按 code 分支。
const (
	ReasonPrefixCritical            = "signal_critical:"
	ReasonPrefixDegraded            = "signal_degraded:"
	ReasonPrefixMissing             = "signal_missing:"
	ReasonPrefixForbidden           = "signal_forbidden:"
	ReasonPrefixUnavailable         = "signal_unavailable:"
	ReasonPrefixStale               = "signal_stale:"
	ReasonPrefixFreshnessUnknown    = "signal_freshness_unknown:"
	ReasonPrefixValueMissing        = "signal_value_missing:"
	ReasonPrefixBaselineUnavailable = "signal_baseline_unavailable:"
	// ReasonPrefixStateInvalid 用于 adapter 报了契约词表外的 availability。它是配置/
	// 实现缺陷而不是普通缺数，单独一个 code 才能在页面上区分出来。
	ReasonPrefixStateInvalid = "signal_state_invalid:"

	// ReasonAllRequiredSignalsHealthy 是 Healthy 唯一的 reason code：这个状态不由某个
	// 具体 binding "造成"，所以不带 binding key。
	ReasonAllRequiredSignalsHealthy = "all_required_signals_healthy"
	// ReasonProfileUnconfigured 表示该服务没有 active published Profile。
	ReasonProfileUnconfigured = "profile_unconfigured"
	// ReasonNoRequiredSignals 表示 Profile 一个 binding 都没声明（schema 上不该发生，
	// 但绝不能因此默认 Healthy）。
	ReasonNoRequiredSignals = "no_required_signals"
	// ReasonCodesTruncated 在 reason code 超过 schema 上限时作为最后一项出现，
	// 让"还有更多原因"这件事显式可见。
	ReasonCodesTruncated = "reason_codes_truncated"
	// ReasonSourceStateInvalid 表示 source state 不在 Page View 的 availability 词表里
	// （例如误把 EvidenceRef 的 expired 塞进来）。它 fail closed 成 Unknown。
	ReasonSourceStateInvalid = "source_state_invalid"
)

// MaxReasonCodes 与 Page View schema 的 health.reason_codes maxItems 对齐。
const MaxReasonCodes = 20

// signal 级 unknown reason 的稳定取值。它们既写进 SignalEvaluation.UnknownReason，
// 也被 coverageReason 用来挑选服务级 reason code，所以必须是常量而不是散落的字面量。
const (
	// UnknownReasonNotReturned 表示 adapter 完全没返回这个 required binding。
	UnknownReasonNotReturned = "signal_not_returned_by_adapter"
	// UnknownReasonStateInvalid 表示 adapter 给出的 availability 不在契约词表内
	// （例如把 EvidenceRef 专属的 expired 强转了进来）。fail closed 成 Unknown。
	UnknownReasonStateInvalid = "signal_state_invalid"
	// UnknownReasonValueMissing 表示声称 available 却没有数值，是不自洽的观测。
	UnknownReasonValueMissing = "value_missing"
	// UnknownReasonFreshnessUndeterminable 表示 adapter 拿到了数值但无法判断新鲜度。
	UnknownReasonFreshnessUndeterminable = "freshness_undeterminable"
	// UnknownReasonObservationStale 表示观测已超出 Profile 的 freshness TTL。
	UnknownReasonObservationStale = "observation_stale"
	// UnknownReasonBaselineUnavailable 表示 baseline_deviation 阈值拿不到基线偏差。
	UnknownReasonBaselineUnavailable = "baseline_unavailable"
)

// SignalObservation 是 adapter 返回的一次原始观测。它不带健康判定：阈值、freshness TTL
// 与最终健康全部由 evaluator 依据 Profile 计算。
type SignalObservation struct {
	// BindingKey 必须匹配 Profile 里声明的 signal binding；不匹配的观测会被丢弃。
	BindingKey   string
	Availability model.SignalAvailability
	// Value 只在 Availability=available 时有意义。
	Value *float64
	// ObservedAt 必须是真实观测时间。adapter 绝不能在拿不到数据时填当前时间——那会把
	// "还没观测到"伪装成"刚刚观测过"。
	ObservedAt time.Time
	SourceKey  string
	// UnknownReason 在 Availability != available 时必填（契约要求）。
	UnknownReason string
	// FreshnessUnknown 表示 adapter 查询、解析或求值失败，系统无法判断新鲜度。它与
	// "观测确实过期"(stale) 是两件事：前者进 Data Partial，后者进 Data Stale。
	FreshnessUnknown bool
	// BaselineDeltaPercent 供 mode=baseline_deviation 的阈值使用，也用于展示基线变化。
	BaselineDeltaPercent *float64
	// ThresholdSource 非空时原样保留；为空时由 evaluator 依据 Profile 推导。
	ThresholdSource string
	// Sparkline 只有真的拿到样本时才带；没有样本不得画折线。
	Sparkline []float64
}

// SourceObservation 是一个数据来源在本次请求中的状态。
type SourceObservation struct {
	SourceKey  string
	State      model.Availability
	Freshness  model.Freshness
	ObservedAt time.Time
	ReasonCode string
	Retryable  bool
}

// Input 是一次 Health/Data 求值的全部输入。
//
// 刻意不包含 Fault/Impact/Incident 计数：领域数量只做展示，不能改写信号健康。
type Input struct {
	// Profile 为 nil 表示该服务没有 active published Profile：Health Unknown、
	// Data Unconfigured。
	Profile *model.Profile
	// Now 是求值时刻，显式注入以便测试用固定时钟。
	Now     time.Time
	Signals []SignalObservation
	Sources []SourceObservation
	// CacheSnapshotStale 表示目录摘要来自过期快照。它只影响 Data，不影响 Health。
	CacheSnapshotStale bool
}

// SignalEvaluation 是单个 binding 的判定结果，projector 据此投影成 model.SignalValue。
type SignalEvaluation struct {
	Binding      model.SignalBinding
	Availability model.SignalAvailability
	Freshness    model.Freshness
	Health       model.SignalHealth
	// Value 在 Availability=available 时保留原值（即使已过期也保留，页面要能显示
	// "这是 5 分钟前的读数"），否则为 nil。
	Value                *float64
	ObservedAt           time.Time
	SourceKey            string
	UnknownReason        string
	MeasurementRole      model.MeasurementRole
	ProxyReason          string
	ThresholdSource      string
	BaselineDeltaPercent *float64
	Sparkline            []float64
	// Eligible 表示该 signal 参与了 worst-of 聚合（available 且 fresh 且有可判定的阈值）。
	Eligible bool
	// RequiredCoverageMissing 表示该 signal 让 required coverage 不完整。
	RequiredCoverageMissing bool
}

// Evaluation 是一次求值的完整结果。
type Evaluation struct {
	Health  model.HealthSummary
	Data    model.DataStatus
	Signals []SignalEvaluation
}

// Evaluate 是纯函数：不改写入参，同一输入总是产生同一结果。
func Evaluate(input Input) Evaluation {
	evaluation := Evaluation{
		Health:  model.HealthSummary{EvaluatedAt: model.NewTimestamp(input.Now)},
		Data:    model.DataStatus{Sources: projectSources(input.Sources)},
		Signals: []SignalEvaluation{},
	}

	if input.Profile == nil {
		// 未配置 Profile：不猜指标、不猜布局，Health Unknown + Data Unconfigured。
		evaluation.Health.Status = model.HealthUnknown
		evaluation.Health.ReasonCodes = []string{ReasonProfileUnconfigured}
		evaluation.Data.State = model.DataStateUnconfigured
		return evaluation
	}

	evaluation.Signals = evaluateSignals(*input.Profile, input.Now, input.Signals)
	evaluation.Health.Status, evaluation.Health.ReasonCodes = aggregateHealth(evaluation.Signals)
	evaluation.Data.State = evaluateDataState(evaluation.Data.Sources, input.CacheSnapshotStale)
	return evaluation
}

// evaluateSignals 按 Profile 声明顺序判定每个 binding。顺序固定保证同一份输入两次求值
// 的输出逐字节一致，翻页与刷新不会莫名重排。
func evaluateSignals(profile model.Profile, now time.Time, observations []SignalObservation) []SignalEvaluation {
	byKey := make(map[string]SignalObservation, len(observations))
	for _, observation := range observations {
		// 同一 binding 出现多次时保留第一条，避免顺序影响结果。
		if _, exists := byKey[observation.BindingKey]; !exists {
			byKey[observation.BindingKey] = observation
		}
	}

	ttl := time.Duration(profile.Limits.FreshnessTTLSeconds) * time.Second
	results := make([]SignalEvaluation, 0, len(profile.SignalBindings))
	for _, declared := range profile.SignalBindings {
		observation, found := byKey[declared.Key]
		results = append(results, evaluateSignal(profile, declared, observation, found, now, ttl))
	}
	return results
}

func evaluateSignal(
	profile model.Profile,
	declared model.SignalBinding,
	observation SignalObservation,
	found bool,
	now time.Time,
	ttl time.Duration,
) SignalEvaluation {
	result := SignalEvaluation{
		Binding: declared,
		// proxy 元数据与阈值来源原样自 Profile 带出，projector 不重新推导。
		MeasurementRole: declared.MeasurementRole,
		ProxyReason:     declared.ProxyReason,
		ThresholdSource: thresholdSource(profile, declared, observation),
	}

	if !found {
		// adapter 完全没返回这个 required binding：最小 Unknown，且 observed_at 保持零值
		// （没有任何观测过），绝不能填当前时间。
		return unknownSignal(result, model.SignalAvailabilityUnavailable, UnknownReasonNotReturned)
	}

	result.ObservedAt = observation.ObservedAt
	result.SourceKey = observation.SourceKey
	result.BaselineDeltaPercent = observation.BaselineDeltaPercent
	result.Sparkline = cloneFloats(observation.Sparkline)

	if !observation.Availability.IsValid() {
		// availability 不在契约词表内（例如 EvidenceRef 专属的 expired 被强转进来）。
		// 绝不能把它原样下传：source/signal 的 availability 词表里没有 expired，
		// 下传会让 Page View 直接违约。这里 fail closed 成 Unknown，并强制使用稳定
		// reason code——一个状态本身就非法的观测，它自报的 reason 也不可信。
		return unknownSignal(result, model.SignalAvailabilityUnknown, UnknownReasonStateInvalid)
	}

	result.Availability = observation.Availability
	result.UnknownReason = observation.UnknownReason

	if observation.Availability != model.SignalAvailabilityAvailable {
		// Forbidden/Unavailable/Unknown：契约要求 value=null、health=unknown。
		reason := result.UnknownReason
		if reason == "" {
			reason = string(observation.Availability)
		}
		return unknownSignal(result, observation.Availability, reason)
	}

	if observation.Value == nil {
		// 声称 available 却没有数值，是不自洽的观测。Page View 的 signalValue 要求
		// available => value 是 number，所以不能保留 available+null；归一成合法的
		// non-available Unknown，也绝不把缺失当成 0。
		//
		// 这个判断必须排在 FreshnessUnknown 之前：否则"无法判断新鲜度"且没有数值的
		// 观测会走到下面那条保留 available 的分支，输出 available+null。
		return unknownSignal(result, model.SignalAvailabilityUnknown, UnknownReasonValueMissing)
	}

	// 到这里 availability 一定是 available 且数值存在，所以后续所有分支都保留数值深拷贝，
	// available => number 的契约不变式由此恒成立。
	result.Value = cloneFloat(observation.Value)

	if observation.FreshnessUnknown {
		// adapter 拿到了数值但说"我无法判断新鲜度"：这是失败导致的 unknown，会把 Data
		// 推到 Partial。保留数值供展示，但健康与新鲜度都退化成 unknown，不假绿。
		result.Freshness = model.FreshnessUnknown
		result.Health = model.SignalHealthUnknown
		result.RequiredCoverageMissing = true
		if result.UnknownReason == "" {
			result.UnknownReason = UnknownReasonFreshnessUndeterminable
		}
		return result
	}

	result.Freshness = freshnessFor(observation.ObservedAt, now, ttl)

	if declared.Thresholds == nil {
		// 没有方向阈值就没有健康意见：恒 Neutral，不进入 worst-of。即使过期也仍是
		// Neutral（它本来就没给出过健康结论），但仍计入 required coverage 缺口。
		result.Health = model.SignalHealthNeutral
		result.RequiredCoverageMissing = result.Freshness != model.FreshnessFresh
		return result
	}

	if result.Freshness != model.FreshnessFresh {
		// 过期观测不 eligible：保留数值与观测时间供展示，但健康退化成 Unknown，
		// 绝不复用旧的 healthy。
		result.Health = model.SignalHealthUnknown
		result.RequiredCoverageMissing = true
		if result.UnknownReason == "" {
			result.UnknownReason = UnknownReasonObservationStale
		}
		return result
	}

	comparable, reason := thresholdInput(*declared.Thresholds, *result.Value, observation.BaselineDeltaPercent)
	if reason != "" {
		// baseline_deviation 模式拿不到基线偏差：没有可比基准就必须 Unknown，
		// 不能因为"没有基线"默认判成 Healthy。
		result.Health = model.SignalHealthUnknown
		result.RequiredCoverageMissing = true
		if result.UnknownReason == "" {
			result.UnknownReason = reason
		}
		return result
	}

	result.Eligible = true
	result.Health = thresholdHealth(*declared.Thresholds, comparable)
	return result
}

// thresholdInput 返回参与阈值比较的数值。absolute 模式直接比原值；baseline_deviation
// 模式比基线偏差百分比，拿不到偏差时返回原因码。
func thresholdInput(thresholds model.SignalThresholds, value float64, baselineDelta *float64) (float64, string) {
	if thresholds.Mode != model.ThresholdModeBaselineDeviation {
		return value, ""
	}
	if baselineDelta == nil {
		return 0, UnknownReasonBaselineUnavailable
	}
	return *baselineDelta, ""
}

// unknownSignal 把结果收敛成一个契约合法的 non-available Unknown：value 必为 nil、
// health 必为 unknown、freshness 为 unknown、必带非空 unknown reason，并计入 required
// coverage 缺口。所有"拿不到可信数值"的分支都必须经过它，这样 Page View 的
// "availability=available => value 是 number" 不变式在 evaluator 出口就已经成立。
func unknownSignal(result SignalEvaluation, availability model.SignalAvailability, reason string) SignalEvaluation {
	if !availability.IsValid() || availability == model.SignalAvailabilityAvailable {
		// 兜底：调用方传了 available 或非法值时也不允许泄漏出去。
		availability = model.SignalAvailabilityUnknown
	}
	result.Availability = availability
	result.Value = nil
	result.Freshness = model.FreshnessUnknown
	result.Health = model.SignalHealthUnknown
	result.RequiredCoverageMissing = true
	result.UnknownReason = reason
	if result.UnknownReason == "" {
		result.UnknownReason = string(availability)
	}
	return result
}

// thresholdHealth 用 >= / <= 判定越界：阈值语义是"达到即越界"，所以 value 恰好等于
// degraded 就已经是 Degraded。
func thresholdHealth(thresholds model.SignalThresholds, value float64) model.SignalHealth {
	if thresholds.Direction == model.ThresholdDirectionBelow {
		switch {
		case value <= thresholds.Critical:
			return model.SignalHealthCritical
		case value <= thresholds.Degraded:
			return model.SignalHealthDegraded
		default:
			return model.SignalHealthHealthy
		}
	}
	switch {
	case value >= thresholds.Critical:
		return model.SignalHealthCritical
	case value >= thresholds.Degraded:
		return model.SignalHealthDegraded
	default:
		return model.SignalHealthHealthy
	}
}

// freshnessFor 判断观测是否仍在 Profile 的 freshness TTL 内。零观测时间或未来时间都
// 无法判断，退化成 unknown 而不是乐观地当成 fresh。
func freshnessFor(observedAt, now time.Time, ttl time.Duration) model.Freshness {
	if observedAt.IsZero() || ttl <= 0 {
		return model.FreshnessUnknown
	}
	age := now.Sub(observedAt)
	if age < 0 {
		return model.FreshnessUnknown
	}
	if age > ttl {
		return model.FreshnessStale
	}
	return model.FreshnessFresh
}

// thresholdSource 说明"这个信号的判定依据来自哪里"。adapter 显式提供时原样保留；
// 否则有阈值就指向 Profile 的该 binding，只有 baseline 参考时指向 baseline ref。
func thresholdSource(profile model.Profile, declared model.SignalBinding, observation SignalObservation) string {
	if observation.ThresholdSource != "" {
		return observation.ThresholdSource
	}
	if declared.Thresholds != nil {
		return fmt.Sprintf("profile:%s:v%d:%s", profile.ProfileKey, profile.ProfileVersion, declared.Key)
	}
	return declared.BaselineRef
}

// aggregateHealth 执行 worst-of 聚合，并只把产生最终状态的 binding 写进 reason codes。
func aggregateHealth(signals []SignalEvaluation) (model.Health, []string) {
	if len(signals) == 0 {
		return model.HealthUnknown, []string{ReasonNoRequiredSignals}
	}

	var critical, degraded, coverage []string
	for _, signal := range signals {
		if signal.Eligible {
			switch signal.Health {
			case model.SignalHealthCritical:
				critical = append(critical, ReasonPrefixCritical+signal.Binding.Key)
			case model.SignalHealthDegraded:
				degraded = append(degraded, ReasonPrefixDegraded+signal.Binding.Key)
			}
		}
		if signal.RequiredCoverageMissing {
			coverage = append(coverage, coverageReason(signal))
		}
	}

	switch {
	case len(critical) > 0:
		return model.HealthCritical, capReasonCodes(critical)
	case len(degraded) > 0:
		return model.HealthDegraded, capReasonCodes(degraded)
	case len(coverage) > 0:
		return model.HealthUnknown, capReasonCodes(coverage)
	default:
		return model.HealthHealthy, []string{ReasonAllRequiredSignalsHealthy}
	}
}

// coverageReason 说明这个 required signal 为什么让覆盖不完整。区分 missing/forbidden/
// unavailable/stale/freshness unknown，页面才能给出可操作的下一步。
// 具体的 UnknownReason 先于笼统的 availability 判断：几个 fail-closed 分支会把
// availability 归一成 Unknown，若先看 availability 就会把"没有数值""拿不到基线"
// "adapter 根本没返回"全都压成同一个 signal_unavailable，丢掉可操作的下一步。
func coverageReason(signal SignalEvaluation) string {
	switch signal.UnknownReason {
	case UnknownReasonNotReturned:
		return ReasonPrefixMissing + signal.Binding.Key
	case UnknownReasonValueMissing:
		return ReasonPrefixValueMissing + signal.Binding.Key
	case UnknownReasonBaselineUnavailable:
		return ReasonPrefixBaselineUnavailable + signal.Binding.Key
	case UnknownReasonStateInvalid:
		return ReasonPrefixStateInvalid + signal.Binding.Key
	}
	switch {
	case signal.Availability == model.SignalAvailabilityForbidden:
		return ReasonPrefixForbidden + signal.Binding.Key
	case signal.Availability == model.SignalAvailabilityUnavailable,
		signal.Availability == model.SignalAvailabilityUnknown:
		return ReasonPrefixUnavailable + signal.Binding.Key
	case signal.Freshness == model.FreshnessStale:
		return ReasonPrefixStale + signal.Binding.Key
	default:
		return ReasonPrefixFreshnessUnknown + signal.Binding.Key
	}
}

// capReasonCodes 排序去重并遵守 Page View schema 的 maxItems=20。超出时保留前 19 项
// 并显式追加截断标记，而不是静默丢弃或超限。
func capReasonCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	unique := make([]string, 0, len(codes))
	for _, code := range codes {
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		unique = append(unique, code)
	}
	sort.Strings(unique)
	if len(unique) <= MaxReasonCodes {
		return unique
	}
	capped := make([]string, 0, MaxReasonCodes)
	capped = append(capped, unique[:MaxReasonCodes-1]...)
	return append(capped, ReasonCodesTruncated)
}

// projectSources 把 source 观测投影成契约里的 SourceStatus，并按 source key 稳定排序。
// 不在 availability 词表里的 state（例如误把 EvidenceRef 的 expired 塞进来）fail closed
// 成 unknown，从而把 Data 推到 Partial，绝不当成一次成功查询。
func projectSources(observations []SourceObservation) []model.SourceStatus {
	sources := make([]model.SourceStatus, 0, len(observations))
	for _, observation := range observations {
		status := model.SourceStatus{
			SourceKey:  observation.SourceKey,
			State:      observation.State,
			Freshness:  observation.Freshness,
			ReasonCode: observation.ReasonCode,
			Retryable:  observation.Retryable,
		}
		if !observation.State.IsValid() {
			status.State = model.AvailabilityUnknown
			status.ReasonCode = ReasonSourceStateInvalid
		}
		if !observation.Freshness.IsValid() {
			status.Freshness = model.FreshnessUnknown
		}
		if !observation.ObservedAt.IsZero() {
			status.ObservedAt = model.NewTimestamp(observation.ObservedAt)
		}
		sources = append(sources, status)
	}
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].SourceKey < sources[j].SourceKey })
	return sources
}

// evaluateDataState 实现 Data 优先级：unconfigured 已在 Evaluate 里短路，这里处理
// partial > stale > complete。
func evaluateDataState(sources []model.SourceStatus, cacheSnapshotStale bool) model.DataState {
	stale := cacheSnapshotStale
	for _, source := range sources {
		switch source.State {
		case model.AvailabilityForbidden, model.AvailabilityUnavailable, model.AvailabilityUnknown:
			return model.DataStatePartial
		}
		// 失败导致的 freshness unknown 表示"系统无法判断新鲜度"，比 stale 更严重。
		if source.Freshness == model.FreshnessUnknown {
			return model.DataStatePartial
		}
		if source.Freshness == model.FreshnessStale {
			stale = true
		}
	}
	if stale {
		return model.DataStateStale
	}
	return model.DataStateComplete
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneFloats(values []float64) []float64 {
	if values == nil {
		return nil
	}
	out := make([]float64, len(values))
	copy(out, values)
	return out
}
