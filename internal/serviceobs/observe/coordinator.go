// Package observe 是 provider-neutral 的 ObservationCoordinator。
//
// 它实现 serviceobs.ObservationReader，因此 facade、API 路由与前端 Page View 不出现任何
// provider 分支：换 Prometheus 后端或换证据存储只改这里注册了哪些 reader。
//
// 六条行为口径在本文件固化，每一条都有对应测试：
//
//  1. Subject 与 Purpose 必须由装配层给出。缺任何一个都 fail closed——一个没有用途的读取
//     请求无法判断"该不该对每一行服务扇出证据查询"，猜错的代价是把一次目录列表放大成
//     上千次上游查询。
//  2. purpose 决定调用哪些来源。directory_summary 只读 signal，**绝不**碰
//     logs/traces/access。
//  3. 每个来源有自己的 deadline 与 limits。单来源超时/失败/panic 只把它自己降级为
//     Unknown/Unavailable，其它来源与 declared topology、domain 事实全部保留。
//  4. 父 context 取消或超时一律向上返回 context 错误，不把已断开的请求伪装成一个可返回的
//     Partial；同时所有子 context 被取消，在途 HTTP 请求随之终止，不遗留 goroutine。
//  5. 完全没有配置任何 provider 时，逐字保留 Phase 2 的 telemetry_not_connected 语义。
//     部分配置时，当前 purpose 已选中但未连接的来源必须显式 *_not_connected，不能省略。
//  6. 当前用户缺少某来源所需权限时，该来源的上游调用次数为 0，只在最小范围 Forbidden。
package observe

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

// Readers 是已注册的来源实现。任一字段为 nil 表示该 provider 未连接。
//
// 用 nil 而不是布尔开关：装配层要么注册了一个真的能查的实现，要么什么都没注册，
// 不存在"注册了但其实不可用"的中间态。
type Readers struct {
	Signals serviceobs.SemanticSignalReader
	Traces  serviceobs.TraceReader
	Logs    serviceobs.LogReader
	Access  serviceobs.AccessReader
}

// AnyConfigured 表示是否至少注册了一个 provider。
func (r Readers) AnyConfigured() bool {
	return r.Signals != nil || r.Traces != nil || r.Logs != nil || r.Access != nil
}

// Limits 是各来源的全局上界。每次请求的有效值取它与 Profile limits 的最小值。
type Limits struct {
	Signals serviceobs.SourceLimits
	Traces  serviceobs.SourceLimits
	Logs    serviceobs.SourceLimits
	Access  serviceobs.SourceLimits
}

const (
	defaultSourceTimeout    = 5 * time.Second
	defaultMaxRows          = 200
	defaultMaxSeries        = 500
	defaultMaxResponseBytes = int64(8 << 20)
)

func withLimitDefaults(limits serviceobs.SourceLimits) serviceobs.SourceLimits {
	if limits.Timeout <= 0 {
		limits.Timeout = defaultSourceTimeout
	}
	if limits.MaxRows <= 0 {
		limits.MaxRows = defaultMaxRows
	}
	if limits.MaxSeries <= 0 {
		limits.MaxSeries = defaultMaxSeries
	}
	if limits.MaxResponseBytes <= 0 {
		limits.MaxResponseBytes = defaultMaxResponseBytes
	}
	return limits
}

// clampToProfile 把全局上界与 Profile limits 取最小值。
//
// 只收紧不放宽：Profile 声明的 max_rows 比全局大时仍然用全局值，否则一个服务可以通过
// 自己的 Profile 把系统级保护抬高。
func clampToProfile(limits serviceobs.SourceLimits, profile model.Profile) serviceobs.SourceLimits {
	if profile.Limits.MaxRows > 0 && profile.Limits.MaxRows < limits.MaxRows {
		limits.MaxRows = profile.Limits.MaxRows
	}
	return limits
}

// Options 是 Coordinator 的构造依赖。
type Options struct {
	Readers Readers
	Limits  Limits
	// Permissions 为 nil 时一切需要权限的来源都被视为未授权：默认从严。
	Permissions serviceobs.PermissionResolver
	// BindingSource 在 provider 已配置时必须提供：执行计划来自 CompiledIntegration。
	BindingSource integration.RuntimeBindingSource
	// SummaryBindingKeys 决定目录摘要读哪些 binding。为 nil 时回退到"全部 golden binding"，
	// 因为目录摘要展示的就是四大黄金信号。
	SummaryBindingKeys func(model.Profile) []string
}

// Coordinator 并行读取已授权且当前 purpose 真正需要的来源。
type Coordinator struct {
	readers       Readers
	limits        Limits
	permissions   serviceobs.PermissionResolver
	bindingSource integration.RuntimeBindingSource
	summaryKeys   func(model.Profile) []string
}

// 编译期断言：Coordinator 必须满足 Phase 2 就固定下来的 ObservationReader 端口。
var _ serviceobs.ObservationReader = (*Coordinator)(nil)

var (
	// ErrSubjectRequired 表示装配层没有传服务端身份。
	ErrSubjectRequired = errors.New("observe: ObservationRequest 缺少服务端 Subject")
	// ErrPurposeRequired 表示装配层没有选择读取用途。
	ErrPurposeRequired = errors.New("observe: ObservationRequest 缺少合法 Purpose")
	// ErrRuntimeBindingRequired 表示 provider 已配置但缺少 RuntimeBindingSource。
	ErrRuntimeBindingRequired = errors.New("observe: provider 已配置但缺少 RuntimeBindingSource")
)

// New 构造 Coordinator。它不校验 readers 是否齐全：完全未配置是合法状态。
func New(opts Options) *Coordinator {
	summaryKeys := opts.SummaryBindingKeys
	if summaryKeys == nil {
		summaryKeys = defaultSummaryBindingKeys
	}
	return &Coordinator{
		readers:       opts.Readers,
		limits:        opts.Limits,
		permissions:   opts.Permissions,
		bindingSource: opts.BindingSource,
		summaryKeys:   summaryKeys,
	}
}

// defaultSummaryBindingKeys 返回四大黄金信号的 binding key。
//
// 目录摘要展示的就是它们；把全部 binding 都查一遍会让一次列表刷新的成本随 Profile 大小
// 线性增长，而多出来的值一个也不会显示。
func defaultSummaryBindingKeys(profile model.Profile) []string {
	bindings := profile.GoldenBindings()
	keys := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		keys = append(keys, binding.Key)
	}
	return keys
}

// sourceOutcome 是单个来源的归一化结果。
type sourceOutcome struct {
	source        health.SourceObservation
	signals       []health.SignalObservation
	evidence      []model.EvidenceRef
	journeyStages []serviceobs.JourneyStageObservation
	hints         []serviceobs.ObservedTopologyHint
	instanceHints []serviceobs.ObservedInstanceHint
	issues        []serviceobs.ReconciliationIssue
	focus         serviceobs.FocusResult
	readAt        time.Time
	// focusReported 表示该来源是否真的表达了 focus 结论。只有表达过的来源才参与合并，
	// 否则一个不读 focus 的来源会把结论拉回 not_requested。
	focusReported bool
}

// ReadObservations 并行读取来源并按稳定 source key 合并。
func (c *Coordinator) ReadObservations(
	ctx context.Context, req serviceobs.ObservationRequest,
) (serviceobs.ObservationFacts, error) {
	if req.Subject.UserID == "" {
		return serviceobs.ObservationFacts{}, ErrSubjectRequired
	}
	if !req.Purpose.IsValid() {
		return serviceobs.ObservationFacts{}, fmt.Errorf("%w: %q", ErrPurposeRequired, req.Purpose)
	}
	// 父 context 已经断开时立刻返回：不发起任何上游调用，也不构造一个看起来完整的结果。
	if err := ctx.Err(); err != nil {
		return serviceobs.ObservationFacts{}, err
	}

	// 完全没有配置任何 provider：source 与 signal 逐字保留 Phase 2 语义。
	if !c.readers.AnyConfigured() {
		// focus 是这里唯一的例外，而且必须是 Unknown：NotRequested 的含义是"你没问"，
		// 但用户确实问了"这个 sandbox_id 有没有被观测到"，真实答案是"我们不知道"。
		// 把后者说成前者等于替用户撤回了问题，也违反"缺数据至少降级为 Unknown"。
		focusUnknown := req.Purpose.NeedsFocusEvidence() && req.Focus != nil
		return c.notConnectedFacts(c.requestedBindingKeys(req), focusUnknown), nil
	}

	permissions, err := c.resolvePermissions(ctx, req)
	if err != nil {
		return serviceobs.ObservationFacts{}, err
	}

	// binding key 只在这里解析一次，然后同时交给 reader 与 merge。
	//
	// 修复前有两套推导：runSources 用 c.summaryKeys（可注入），merge 里又硬编码一遍
	// GoldenBindings。注入自定义 SummaryBindingKeys 时，"请求了哪些 binding"与"给哪些
	// binding 补 Unknown"就会不一致——页面于是缺几条本该显式 Unknown 的信号。
	bindingKeys := c.requestedBindingKeys(req)

	var runtimePlan runtimeSourcePlan
	var compiled integration.CompiledIntegration
	if c.bindingSource != nil {
		resolved, resolveErr := c.bindingSource.ResolveCompiled(
			ctx, req.ServiceID, req.Environment, req.Profile,
		)
		if resolveErr != nil {
			return serviceobs.ObservationFacts{}, fmt.Errorf("observe: 解析 CompiledIntegration 失败: %w", resolveErr)
		}
		compiled = resolved
		runtimePlan = planRuntimeSources(req.Purpose, req.Focus, bindingKeys, compiled)
	} else if c.readers.AnyConfigured() {
		return serviceobs.ObservationFacts{}, ErrRuntimeBindingRequired
	}

	outcomes := c.runSources(ctx, req, runtimePlan, permissions, bindingKeys)

	// 父 context 取消/超时必须向上返回，而不是把一堆"因为断开所以 Unknown"的来源当成
	// 一个可展示的 Partial 返回给用户。
	if err := ctx.Err(); err != nil {
		return serviceobs.ObservationFacts{}, err
	}
	facts := mergeOutcomes(bindingKeys, outcomes)
	if runtimePlan.FocusUnknown {
		facts.Focus = serviceobs.FocusUnknown
	}
	return facts, nil
}

// resolvePermissions 现算权限。resolver 为 nil 时返回空集合（拒绝一切），而不是放行。
func (c *Coordinator) resolvePermissions(
	ctx context.Context, req serviceobs.ObservationRequest,
) (serviceobs.Permissions, error) {
	if c.permissions == nil {
		return serviceobs.NewPermissions(), nil
	}
	permissions, err := c.permissions.ResolvePermissions(ctx, req.Subject, req.ServiceID)
	if err != nil {
		// 权限算不出来时不能"当作有权限"继续：整请求失败，由上层决定页面级错误。
		return serviceobs.Permissions{}, fmt.Errorf("observe: 解析权限失败: %w", err)
	}
	return permissions, nil
}

// notConnectedFacts 构造 Phase 2 的"遥测完全未接入"事实。
//
// focusUnknown 由调用方判定"本次是否真的问过 focus"，理由见 ReadObservations 里的注释。
func (c *Coordinator) notConnectedFacts(bindingKeys []string, focusUnknown bool) serviceobs.ObservationFacts {
	focus := serviceobs.FocusNotRequested
	if focusUnknown {
		focus = serviceobs.FocusUnknown
	}
	facts := serviceobs.ObservationFacts{
		Signals:      make([]health.SignalObservation, 0, len(bindingKeys)),
		EvidenceRefs: []model.EvidenceRef{},
		Focus:        focus,
	}
	for _, key := range bindingKeys {
		facts.Signals = append(facts.Signals, health.SignalObservation{
			BindingKey:    key,
			Availability:  model.SignalAvailabilityUnavailable,
			SourceKey:     serviceobs.SourceKeyPrometheus,
			UnknownReason: serviceobs.ReasonTelemetryNotConnected,
		})
	}
	facts.Sources = []health.SourceObservation{{
		SourceKey:  serviceobs.SourceKeyPrometheus,
		State:      model.AvailabilityUnavailable,
		Freshness:  model.FreshnessUnknown,
		ReasonCode: serviceobs.ReasonTelemetryNotConnected,
		Retryable:  false,
	}}
	return facts
}

// requestedBindingKeys 决定本次真正要读的 binding。
func (c *Coordinator) requestedBindingKeys(req serviceobs.ObservationRequest) []string {
	if len(req.BindingKeys) > 0 {
		return req.BindingKeys
	}
	if req.Purpose == serviceobs.PurposeDirectorySummary {
		return c.summaryKeys(req.Profile)
	}
	keys := make([]string, 0, len(req.Profile.SignalBindings))
	for _, binding := range req.Profile.SignalBindings {
		keys = append(keys, binding.Key)
	}
	return keys
}

// runSources 并行执行来源。
//
// 每个来源拿到的是自己的 child context：它自己的 deadline 到了只影响自己，而父 context
// 取消会同时终止全部在途请求。WaitGroup 保证函数返回前所有 goroutine 都已退出，
// 因此不会有 goroutine 泄漏。
func (c *Coordinator) runSources(
	ctx context.Context,
	req serviceobs.ObservationRequest,
	plan runtimeSourcePlan,
	permissions serviceobs.Permissions,
	bindingKeys []string,
) []sourceOutcome {
	if plan.FocusUnknown {
		plan.Traces = nil
		plan.Logs = nil
		plan.Access = nil
	}
	type task struct {
		sourceKey    string
		permission   serviceobs.Permission
		limits       serviceobs.SourceLimits
		bindings     []integration.CompiledBinding
		run          func(context.Context, serviceobs.SourceLimits) (sourceOutcome, error)
		configured   bool
		notConnect   string
		affectsFocus bool
	}

	tasks := make([]task, 0, 4)
	if len(plan.Signals) > 0 {
		tasks = append(tasks, task{
			sourceKey:    serviceobs.SourceKeyPrometheus,
			permission:   serviceobs.PermissionMetricsRead,
			limits:       clampToProfile(withLimitDefaults(c.limits.Signals), req.Profile),
			bindings:     plan.Signals,
			configured:   c.readers.Signals != nil,
			notConnect:   serviceobs.ReasonPrometheusNotConnected,
			affectsFocus: false,
			run: func(ctx context.Context, limits serviceobs.SourceLimits) (sourceOutcome, error) {
				facts, err := c.readers.Signals.ReadSignals(ctx, serviceobs.SignalRequest{
					Subject:       req.Subject,
					Purpose:       req.Purpose,
					Profile:       req.Profile,
					ServiceID:     req.ServiceID,
					Environment:   req.Environment,
					BindingKeys:   bindingKeys,
					Bindings:      plan.Signals,
					WindowSeconds: req.WindowSeconds,
					From:          req.From,
					To:            req.To,
					Limits:        limits,
				})
				if err != nil {
					return sourceOutcome{}, err
				}
				return sourceOutcome{
					source: facts.Source, signals: facts.Signals, readAt: facts.ReadAt,
				}, nil
			},
		})
	}
	focusRequest := func(bindings []integration.CompiledBinding, limits serviceobs.SourceLimits) serviceobs.FocusRequest {
		return serviceobs.FocusRequest{
			Subject:     req.Subject,
			Purpose:     req.Purpose,
			Profile:     req.Profile,
			ServiceID:   req.ServiceID,
			Environment: req.Environment,
			From:        req.From,
			To:          req.To,
			Focus:       req.Focus,
			Bindings:    bindings,
			Limits:      limits,
		}
	}
	if len(plan.Traces) > 0 {
		tasks = append(tasks, task{
			sourceKey:    serviceobs.SourceKeyOpenObserveTraces,
			permission:   serviceobs.PermissionTracesRead,
			limits:       clampToProfile(withLimitDefaults(c.limits.Traces), req.Profile),
			bindings:     plan.Traces,
			configured:   c.readers.Traces != nil,
			notConnect:   serviceobs.ReasonOpenObserveNotConnected,
			affectsFocus: true,
			run: func(ctx context.Context, limits serviceobs.SourceLimits) (sourceOutcome, error) {
				facts, err := c.readers.Traces.ReadTraces(ctx, focusRequest(plan.Traces, limits))
				if err != nil {
					return sourceOutcome{}, err
				}
				return sourceOutcome{
					source: facts.Source, evidence: facts.EvidenceRefs,
					journeyStages: facts.JourneyStages, hints: facts.TopologyHints,
					instanceHints: facts.InstanceHints, issues: facts.Issues,
					focus: facts.Focus, focusReported: true, readAt: facts.ReadAt,
				}, nil
			},
		})
	}
	if len(plan.Logs) > 0 {
		tasks = append(tasks, task{
			sourceKey:    serviceobs.SourceKeyOpenObserveLogs,
			permission:   serviceobs.PermissionLogsRead,
			limits:       clampToProfile(withLimitDefaults(c.limits.Logs), req.Profile),
			bindings:     plan.Logs,
			configured:   c.readers.Logs != nil,
			notConnect:   serviceobs.ReasonOpenObserveNotConnected,
			affectsFocus: true,
			run: func(ctx context.Context, limits serviceobs.SourceLimits) (sourceOutcome, error) {
				facts, err := c.readers.Logs.ReadLogs(ctx, focusRequest(plan.Logs, limits))
				if err != nil {
					return sourceOutcome{}, err
				}
				return sourceOutcome{
					source: facts.Source, evidence: facts.EvidenceRefs,
					focus: facts.Focus, focusReported: true, readAt: facts.ReadAt,
				}, nil
			},
		})
	}
	if len(plan.Access) > 0 {
		tasks = append(tasks, task{
			sourceKey:    serviceobs.SourceKeyOpenObserveAccess,
			permission:   serviceobs.PermissionLogsRead,
			limits:       clampToProfile(withLimitDefaults(c.limits.Access), req.Profile),
			bindings:     plan.Access,
			configured:   c.readers.Access != nil,
			notConnect:   serviceobs.ReasonOpenObserveNotConnected,
			affectsFocus: true,
			run: func(ctx context.Context, limits serviceobs.SourceLimits) (sourceOutcome, error) {
				facts, err := c.readers.Access.ReadAccess(ctx, focusRequest(plan.Access, limits))
				if err != nil {
					return sourceOutcome{}, err
				}
				return sourceOutcome{
					source: facts.Source, evidence: facts.EvidenceRefs,
					focus: facts.Focus, focusReported: true, readAt: facts.ReadAt,
				}, nil
			},
		})
	}

	outcomes := make([]sourceOutcome, len(tasks))
	var wg sync.WaitGroup
	for i := range tasks {
		current := tasks[i]
		index := i
		// focusRequested 决定占位结果是否携带 focus 结论：只有"参与 focus 判定的来源"且
		// "这次真的请求了 focus"时，它的失败才意味着 focus 不知道。
		focusRequested := current.affectsFocus && req.Focus != nil

		// 未连接与未授权都不启 goroutine：前者没有实现可调，后者必须保证上游调用次数为 0。
		if !current.configured {
			outcomes[index] = notConnectedOutcome(current.sourceKey, current.notConnect, focusRequested)
			continue
		}
		if !permissions.Allows(current.permission) {
			outcomes[index] = forbiddenOutcome(current.sourceKey, focusRequested)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[index] = runOne(ctx, current.sourceKey, current.limits, focusRequested, current.run)
		}()
	}
	wg.Wait()
	return outcomes
}

// runOne 执行一个来源，并把 panic、超时与错误映射成稳定的 source 状态。
func runOne(
	parent context.Context,
	sourceKey string,
	limits serviceobs.SourceLimits,
	focusRequested bool,
	run func(context.Context, serviceobs.SourceLimits) (sourceOutcome, error),
) (outcome sourceOutcome) {
	// panic 隔离：一个 adapter 崩溃不能带走整个请求，也不能让别的来源丢结果。
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = failedOutcome(sourceKey, serviceobs.ReasonSourceInternalError, false, focusRequested)
		}
	}()

	ctx, cancel := context.WithTimeout(parent, limits.Timeout)
	defer cancel()

	result, err := run(ctx, limits)
	if err == nil {
		if result.source.SourceKey == "" {
			result.source.SourceKey = sourceKey
		}
		return result
	}

	// 父 context 断开：这不是"这个来源坏了"，而是整个请求没了。仍然给一个 Unknown 占位，
	// 由 ReadObservations 统一改成向上返回 context 错误。
	if parent.Err() != nil {
		return failedOutcome(sourceKey, serviceobs.ReasonSourceTimeout, true, focusRequested)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failedOutcome(sourceKey, serviceobs.ReasonSourceTimeout, true, focusRequested)
	}
	return failedOutcome(sourceKey, reasonForError(err), retryableForError(err), focusRequested)
}

// reasonForError 把 adapter 的 typed 错误映射成稳定 reason code。
//
// 这里只识别本包定义的 sentinel：provider 特有的错误分类属于 Task 4/5 的 adapter，
// 它们应该在返回前自行映射成这些 code，而不是让 coordinator 猜供应商语义。
func reasonForError(err error) string {
	switch {
	case errors.Is(err, ErrUpstreamAuthFailed):
		return serviceobs.ReasonUpstreamAuthFailed
	case errors.Is(err, ErrUpstreamRateLimited):
		return serviceobs.ReasonUpstreamRateLimited
	case errors.Is(err, ErrResponseTooLarge):
		return serviceobs.ReasonResponseTooLarge
	case errors.Is(err, ErrCardinalityExceeded):
		return serviceobs.ReasonCardinalityExceeded
	case errors.Is(err, ErrAuditUnavailable):
		return serviceobs.ReasonAuditUnavailable
	default:
		return serviceobs.ReasonSourceReadFailed
	}
}

func retryableForError(err error) bool {
	switch {
	case errors.Is(err, ErrUpstreamAuthFailed):
		// 凭据失效重试也不会好：它需要平台去换凭据。
		return false
	case errors.Is(err, ErrResponseTooLarge), errors.Is(err, ErrCardinalityExceeded):
		// 需要用户收窄范围，重试同一个请求没有意义。
		return false
	default:
		return true
	}
}

// adapter 应当返回的 sentinel 错误。它们让"上游凭据失效"与"当前用户没权限"在类型上就分开。
var (
	ErrUpstreamAuthFailed  = errors.New("observe: 上游服务账号鉴权失败")
	ErrUpstreamRateLimited = errors.New("observe: 上游限流")
	ErrResponseTooLarge    = errors.New("observe: 响应超过字节或行数上界")
	ErrCardinalityExceeded = errors.New("observe: series 基数超限")
	ErrAuditUnavailable    = errors.New("observe: 审计不可写，结果不得返回")
)

func notConnectedOutcome(sourceKey, reason string, focusRequested bool) sourceOutcome {
	return withFocusPlaceholder(sourceOutcome{source: health.SourceObservation{
		SourceKey:  sourceKey,
		State:      model.AvailabilityUnavailable,
		Freshness:  model.FreshnessUnknown,
		ReasonCode: reason,
		Retryable:  false,
	}}, focusRequested)
}

func forbiddenOutcome(sourceKey string, focusRequested bool) sourceOutcome {
	return withFocusPlaceholder(sourceOutcome{source: health.SourceObservation{
		SourceKey:  sourceKey,
		State:      model.AvailabilityForbidden,
		Freshness:  model.FreshnessUnknown,
		ReasonCode: serviceobs.ReasonPermissionDenied,
		// 重试不会给用户补上权限。
		Retryable: false,
	}}, focusRequested)
}

func failedOutcome(sourceKey, reason string, retryable bool, focusRequested bool) sourceOutcome {
	return withFocusPlaceholder(sourceOutcome{
		source: health.SourceObservation{
			SourceKey:  sourceKey,
			State:      model.AvailabilityUnavailable,
			Freshness:  model.FreshnessUnknown,
			ReasonCode: reason,
			Retryable:  retryable,
		},
	}, focusRequested)
}

// withFocusPlaceholder 只在"该来源参与 focus 判定且本次确实请求了 focus"时报 FocusUnknown。
//
// 不满足条件时 focusReported 保持 false，因此这条失败完全不参与 focus 合并：一个不读 focus
// 的来源失败，不该让页面对"这个请求有没有被观测到"改变结论。
func withFocusPlaceholder(outcome sourceOutcome, focusRequested bool) sourceOutcome {
	if !focusRequested {
		return outcome
	}
	outcome.focus = serviceobs.FocusUnknown
	outcome.focusReported = true
	return outcome
}

// mergeOutcomes 按稳定 source key 合并结果。
//
// 合并规则刻意保守：
//   - signal 只保留本次真的被请求的 binding，重复 binding 以第一个为准（adapter 不该重复
//     返回，但重复时不能让后面的覆盖前面的，否则结果依赖 map 遍历顺序）；
//   - 缺失的 binding 补一条显式 Unknown，绝不静默消失；
//   - focus 结论按 matched > unknown > empty 取，而不是取"最坏"：已确认命中是正事实，
//     另一个来源失败不能把它抹掉。
func mergeOutcomes(bindingKeys []string, outcomes []sourceOutcome) serviceobs.ObservationFacts {
	facts := serviceobs.ObservationFacts{
		Signals:       []health.SignalObservation{},
		Sources:       []health.SourceObservation{},
		EvidenceRefs:  []model.EvidenceRef{},
		InstanceHints: []serviceobs.ObservedInstanceHint{},
		Focus:         serviceobs.FocusNotRequested,
	}

	var latestReadAt time.Time
	seenBinding := map[string]struct{}{}
	for _, outcome := range outcomes {
		if outcome.readAt.After(latestReadAt) {
			latestReadAt = outcome.readAt
		}
		if outcome.source.SourceKey != "" {
			facts.Sources = append(facts.Sources, outcome.source)
		}
		for _, signal := range outcome.signals {
			if _, exists := seenBinding[signal.BindingKey]; exists {
				continue
			}
			seenBinding[signal.BindingKey] = struct{}{}
			facts.Signals = append(facts.Signals, signal)
		}
		facts.EvidenceRefs = append(facts.EvidenceRefs, outcome.evidence...)
		facts.JourneyStages = append(facts.JourneyStages, outcome.journeyStages...)
		facts.TopologyHints = append(facts.TopologyHints, outcome.hints...)
		facts.InstanceHints = append(facts.InstanceHints, outcome.instanceHints...)
		facts.Issues = append(facts.Issues, outcome.issues...)
	}
	facts.ReadAt = latestReadAt

	// 被请求但没有回值的 binding 必须显式 Unknown。补齐用的就是 ReadObservations 解析出来
	// 的同一份 key 列表，因此"请求集"与"补齐集"不可能不一致。
	signalReason := signalUnknownReason(outcomes)
	for _, key := range bindingKeys {
		if _, exists := seenBinding[key]; exists {
			continue
		}
		seenBinding[key] = struct{}{}
		facts.Signals = append(facts.Signals, health.SignalObservation{
			BindingKey:    key,
			Availability:  model.SignalAvailabilityUnavailable,
			SourceKey:     serviceobs.SourceKeyPrometheus,
			UnknownReason: signalReason,
		})
	}

	facts.Focus = mergeFocus(outcomes)
	sort.SliceStable(facts.Sources, func(i, j int) bool {
		return facts.Sources[i].SourceKey < facts.Sources[j].SourceKey
	})
	sort.SliceStable(facts.Signals, func(i, j int) bool {
		return facts.Signals[i].BindingKey < facts.Signals[j].BindingKey
	})
	serviceobs.SortIssues(facts.Issues)
	return facts
}

// signalUnknownReason 为补齐的 Unknown binding 选一个稳定 reason。
//
// 它取 signal 来源自己的 reason code，因此"Prometheus 超时"与"Prometheus 未连接"在
// binding 层面也能被区分，而不是笼统的一句 unknown。
func signalUnknownReason(outcomes []sourceOutcome) string {
	for _, outcome := range outcomes {
		if outcome.source.SourceKey != serviceobs.SourceKeyPrometheus {
			continue
		}
		if outcome.source.ReasonCode != "" {
			return outcome.source.ReasonCode
		}
	}
	return serviceobs.ReasonSourceReadFailed
}

// mergeFocus 按 matched > unknown > empty 合并 focus 结论。
//
// 优先级的理由是"已确认命中是正事实"：只要有一个来源真的看到了这次请求，它就发生过，
// 另一个来源超时或没权限都不能把它抹掉。反过来，没有任何命中时，只要有一个已请求的来源
// 不知道，就不能宣称 Empty——那会把"查不到"说成"没发生"。只有全部已请求来源都成功确认
// 没有记录，才是 Empty。
//
// 请求本身没有 focus 时恒为 NotRequested：此时没有任何来源会报告 focus 结论。
func mergeFocus(outcomes []sourceOutcome) serviceobs.FocusResult {
	sawUnknown := false
	sawEmpty := false
	for _, outcome := range outcomes {
		if !outcome.focusReported {
			continue
		}
		switch outcome.focus {
		case serviceobs.FocusMatched:
			return serviceobs.FocusMatched
		case serviceobs.FocusUnknown:
			sawUnknown = true
		case serviceobs.FocusEmpty:
			sawEmpty = true
		}
	}
	switch {
	case sawUnknown:
		return serviceobs.FocusUnknown
	case sawEmpty:
		return serviceobs.FocusEmpty
	default:
		return serviceobs.FocusNotRequested
	}
}
