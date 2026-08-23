// Package context 组装 Service Observability 的只读事实快照。
//
// 它只组装事实：不发原始查询之外的推断，不重算 Health（那是 health 包的唯一职责），
// 也不做显示格式化（那是 view 包的职责）。
//
// 两个层次刻意分开：
//
//   - model.DiagnosticContext 严格镜像 v1 schema，additionalProperties=false，所以
//     subject、source status、Profile diagnostic 这些装配内部信息一个字节都不能塞进去；
//   - AssemblySnapshot 是不对外序列化的内部超集，持有 DiagnosticContext、每个 source
//     与 section 的状态、Profile diagnostic 与真实事实时间锚。
//
// 三条契约边界在装配入口就被强制，绝不留给序列化时才炸：
//
//   - scope.timezone 必须落在契约的 1..64 字符内；
//   - 时间窗必须是整秒，并且用真实时长（而不是截断后的整数）与上限比较，否则
//     "上限 + 0.9 秒"会被算成正好等于上限而放行，同时让 window_seconds 与 from/to 矛盾；
//   - 没有拓扑观测时 observed_at 用真实事实时间锚或 Unix epoch，绝不用 wall clock，
//     也绝不留下零值渲染出的 year 0001。
//
// 包名与 stdlib 的 context 同名，因此本文件把标准库别名成 stdcontext。调用方通常写
// `import soctx "alert-system/internal/serviceobs/context"`。
package context

import (
	stdcontext "context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/profile"
)

// ErrInvalidTimeRange 表示 from/to 不是一对合法的绝对 UTC 时间。
// 上层按 422 处理：只传一个、顺序颠倒、非整秒或非绝对时间都不静默修正。
var ErrInvalidTimeRange = errors.New("serviceobs/context: 时间窗非法")

// ErrInvalidTimezone 表示 timezone 不满足契约的长度约束。
//
// 契约把 scope.timezone 限制在 1..64 个字符。它只影响显示，但一个超长值会让整个
// Page View 违约，所以必须在装配入口就 typed 拒绝，而不是等序列化时才炸。
var ErrInvalidTimezone = errors.New("serviceobs/context: timezone 非法")

// maxTimezoneLength 与契约 scope.timezone 的 maxLength 一致。
const maxTimezoneLength = 64

// Kind 区分两条装配路径。目录不走 assembler 的单服务装配，它读 snapshot cache。
type Kind string

const (
	// KindOverview 是服务总览：Profile 缺失时仍可用。
	KindOverview Kind = "overview"
	// KindWorkspace 是聚焦工作区：Profile 缺失时返回 ErrProfileUnconfigured。
	KindWorkspace Kind = "workspace"
)

// Subject 是服务端从 session 计算出的身份。
//
// 它刻意不包含 capabilities：capabilities 由 Assembler 依据"哪些 adapter 真的注册并
// 授权了"计算，绝不采信客户端或 URL 自报值。
type Subject struct {
	UserID  string
	TeamIDs []string
	Roles   []string
}

// Request 是一次装配请求。
type Request struct {
	Kind      Kind
	ServiceID string
	Subject   Subject
	// Environment 为空表示沿用 registration 的环境；显式不匹配统一按服务不存在处理，
	// 避免泄露其它环境是否存在。
	Environment string
	// From/To 必须成对出现且是绝对 UTC。两者都为零时按 Profile 默认窗口生成。
	From time.Time
	To   time.Time
	// Timezone 只影响显示，不改变任何查询边界。
	Timezone string
	Focus    *model.Focus
}

// ProfileSource 是 Profile Registry 的只读视图。*profile.Registry 直接满足它。
type ProfileSource interface {
	PublishedForService(serviceID string) (model.Profile, bool)
	DiagnosticForService(serviceID string) (profile.Diagnostic, bool)
}

// Deadlines 给每个 adapter 独立的超时。一个慢 adapter 不能吃掉整个请求预算，
// 否则"局部失败只降级局部"在超时场景下就不成立。
type Deadlines struct {
	Registration time.Duration
	Topology     time.Duration
	Domain       time.Duration
	Observations time.Duration
}

const (
	defaultRegistrationDeadline = 3 * time.Second
	defaultTopologyDeadline     = 3 * time.Second
	defaultDomainDeadline       = 3 * time.Second
	defaultObservationDeadline  = 5 * time.Second
)

func (d Deadlines) withDefaults() Deadlines {
	if d.Registration <= 0 {
		d.Registration = defaultRegistrationDeadline
	}
	if d.Topology <= 0 {
		d.Topology = defaultTopologyDeadline
	}
	if d.Domain <= 0 {
		d.Domain = defaultDomainDeadline
	}
	if d.Observations <= 0 {
		d.Observations = defaultObservationDeadline
	}
	return d
}

// Limits 是系统级全局上界。每次请求的有效限制取它与 Profile limits 的最小值。
type Limits struct {
	MaxWindowSeconds int
	MaxRows          int
	MaxNodes         int
	MaxRelations     int
	MaxDomainRows    int
}

const (
	// minPolicyWindowSeconds 与契约 policy.max_window_seconds 的下界一致。
	minPolicyWindowSeconds   = 60
	defaultMaxWindowSeconds  = 86400
	defaultMaxRows           = 200
	defaultMaxNodes          = 100
	defaultMaxRelations      = 200
	defaultDefaultWindowSecs = 300
)

func (l Limits) withDefaults() Limits {
	if l.MaxWindowSeconds < minPolicyWindowSeconds {
		l.MaxWindowSeconds = defaultMaxWindowSeconds
	}
	if l.MaxRows <= 0 {
		l.MaxRows = defaultMaxRows
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = defaultMaxNodes
	}
	if l.MaxRelations <= 0 {
		l.MaxRelations = defaultMaxRelations
	}
	if l.MaxDomainRows <= 0 {
		l.MaxDomainRows = l.MaxRows
	}
	return l
}

// Assembler 把 ports 的事实组装成内部快照。
type Assembler struct {
	Registrations serviceobs.RegistrationReader
	Topology      serviceobs.TopologyReader
	Domain        serviceobs.DomainReader
	Observations  serviceobs.ObservationReader
	Profiles      ProfileSource
	// TelemetryConnected 表示语义指标 adapter 真的注册并授权了。Phase 2 恒为 false，
	// 因此 view_metrics 一定是 false——capabilities 不能凭"接口存在"就报 true。
	TelemetryConnected bool
	// EvidenceConnected 表示 logs/traces/evidence adapter 真的注册并授权了。
	EvidenceConnected bool
	Deadlines         Deadlines
	Limits            Limits
	// Clock 注入当前时间。它只用于 assembled_at、evaluated_at 与窗口推导，
	// 绝不用来填任何 observed_at。
	Clock func() time.Time
	// NewRequestID 服务端生成 request ID；为 nil 时使用 crypto/rand 默认实现。
	NewRequestID func() (string, error)
}

// AssemblySnapshot 是不对外序列化的装配结果超集。
type AssemblySnapshot struct {
	// Context 只在存在 active published Profile 时非 nil：契约的
	// service.root_entity_ids/profile_key/profile_version 都是必填非空，没有 Profile
	// 时根本不存在一个合法的 DiagnosticContext。Unconfigured 路径直接投影 Page View。
	Context *model.DiagnosticContext

	RequestID   string
	AssembledAt time.Time
	Kind        Kind
	Subject     Subject

	Identity     serviceobs.ServiceIdentity
	Registration serviceobs.ServiceFacts
	Profile      *model.Profile
	// ProfileDiagnostic 非 nil 表示该 service 有被 quarantine 的 Profile 文档：
	// 页面要显示稳定配置错误 reason，而不是伪装成"这个服务没配 Profile"。
	ProfileDiagnostic *profile.Diagnostic

	TopologyFacts serviceobs.TopologyFacts
	DomainFacts   serviceobs.DomainFacts

	Signals []health.SignalObservation
	Sources []health.SourceObservation
	// EvidenceRefs 只保留脱敏摘要、相关键与受控 locator；原始日志与 trace 正文留在
	// 源系统。Phase 2 没有接入 evidence adapter，因此它恒为空。
	EvidenceRefs []model.EvidenceRef
	// Sections 是 adapter 级的 section 状态。projector 可以在其后追加投影级 section。
	Sections   []model.SectionStatus
	Evaluation health.Evaluation

	Scope         model.ContextScope
	Focus         *model.Focus
	WindowSeconds int
	Capabilities  model.Capabilities
	Policy        model.ContextPolicy

	// Placements 是每个非 Golden binding 的挂载结果，按 binding key 升序。它由
	// assembler 一次算出、DiagnosticContext 与 Page View 共同消费：两处各算一遍就会
	// 出现"context 说这个节点 Unknown、页面说它 Critical"的漂移。
	Placements []SignalPlacement
	// UnresolvedBindings 是 scope.refs 缺省、为空或悬空，因而挂不到 bounded topology
	// 的 binding key（升序）。它们仍然完整保留在 Evaluation 与 Context.Signals 里。
	UnresolvedBindings []string
	// AggregateOnlyBindings 是 scope 解析到多个 canonical 对象的 binding key（升序）。
	//
	// 它们的数值是跨对象聚合值，而 Page View v1 的 signalValue.scope_ref 是单值，
	// 没有无损的逐对象展示位置。这类 binding 不做逐对象展示（否则就是伪造逐对象测量），
	// 但健康意见仍逐对象生效，完整判定仍保留在 Evaluation 与 Context.Signals 里。
	AggregateOnlyBindings []string
	// NodeHealth / RelationHealth 由 health.AggregateObjectHealth 从挂上的 signal 算出。
	// 没有条目表示该对象没有任何 signal，健康只能是 Unknown。
	NodeHealth     map[string]model.Health
	RelationHealth map[string]model.Health

	// FactAnchor 是全部已提交事实里最新的真实时间。projector 用它渲染"从未观测过"的
	// signal 的 observed_at：那个字段在契约里必填，但绝不允许用 wall clock 伪装成
	// 刚刚观测过。
	FactAnchor time.Time
}

// Assemble 组装单个服务的事实快照。
//
// 关键依赖与非关键依赖严格分开：registration 读不到就没有 identity，直接返回错误；
// topology/domain/telemetry 失败只转成 source + section 状态，页面仍然 200 + Partial。
func (a *Assembler) Assemble(ctx stdcontext.Context, req Request) (*AssemblySnapshot, error) {
	deadlines := a.Deadlines.withDefaults()
	limits := a.Limits.withDefaults()
	now := a.now()

	requestID, err := a.newRequestID()
	if err != nil {
		return nil, err
	}

	// 1. registration 是关键依赖。
	registration, err := a.readRegistration(ctx, deadlines.Registration, req.ServiceID)
	if err != nil {
		return nil, err
	}
	if req.Environment != "" && req.Environment != registration.Identity.Environment {
		// 显式环境不匹配统一按服务不存在处理，不泄露其它环境是否存在。
		return nil, serviceobs.ErrServiceNotFound
	}

	snapshot := &AssemblySnapshot{
		RequestID:    requestID,
		AssembledAt:  now,
		Kind:         req.Kind,
		Subject:      req.Subject,
		Identity:     registration.Identity,
		Registration: registration,
		Sections:     []model.SectionStatus{},
		Sources:      []health.SourceObservation{},
		Signals:      []health.SignalObservation{},
		EvidenceRefs: []model.EvidenceRef{},
		FactAnchor:   registration.FactAnchor(),
	}
	snapshot.Capabilities = a.capabilities()
	snapshot.addSection(serviceobs.SectionKeyIdentity, model.AvailabilityAvailable, "", false)
	snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
		SourceKey: serviceobs.SourceKeyRegistration,
		State:     model.AvailabilityAvailable,
		// 登记配置在被显式修改前一直有效，不因时间过期。
		Freshness:  model.FreshnessFresh,
		ObservedAt: registration.FactAnchor(),
		Retryable:  true,
	})

	// 2. Profile 查找。缺失时总览仍可用，工作区必须 409。
	if published, ok := a.publishedProfile(req.ServiceID); ok {
		snapshot.Profile = published
	} else if diagnostic, hasDiagnostic := a.profileDiagnostic(req.ServiceID); hasDiagnostic {
		snapshot.ProfileDiagnostic = diagnostic
	}
	if snapshot.Profile == nil && req.Kind == KindWorkspace {
		return nil, serviceobs.ErrProfileUnconfigured
	}

	// 3. scope：绝对 UTC 窗口 + Profile/global 上界校验，超限显式报错不静默截断。
	scope, windowSeconds, err := resolveScope(req, registration.Identity.Environment, snapshot.Profile, limits, now)
	if err != nil {
		return nil, err
	}
	snapshot.Scope = scope
	snapshot.WindowSeconds = windowSeconds

	// 4. focus：只接受已发布 Profile 声明的 focus kind，并且 value 必须过该 focus 的
	// value_pattern。没有 Profile 时不存在合法 focus。
	focus, err := resolveFocus(req.Focus, snapshot.Profile)
	if err != nil {
		return nil, err
	}
	snapshot.Focus = focus

	// 5. topology 与 domain 各自独立，互不牵连。
	a.readTopology(ctx, deadlines.Topology, snapshot, limits)
	if err := a.readDomain(ctx, deadlines.Domain, snapshot, limits); err != nil {
		// focus 非法是 typed 请求错误，必须上抛成 422，不能悄悄降级成"没有领域事实"。
		if errors.Is(err, serviceobs.ErrInvalidFocus) {
			return nil, err
		}
	}

	// 6. 遥测。只有配置了 Profile 才有 binding 可读。
	a.readObservations(ctx, deadlines.Observations, snapshot)

	// 7. Health/Data 只由 evaluator 计算，三页共用同一个入口。
	snapshot.Evaluation = health.Evaluate(health.Input{
		Profile: snapshot.Profile,
		Now:     now,
		Signals: snapshot.Signals,
		Sources: snapshot.Sources,
	})

	// 8. signal 归属与对象健康只算一次，供 context 与三页共用。
	resolvePlacements(snapshot)

	snapshot.Policy = a.policy(snapshot.Profile, limits)
	if snapshot.Profile != nil {
		snapshot.Context = buildDiagnosticContext(snapshot)
	}
	return snapshot, nil
}

// SignalPlacement 记录一个非 Golden binding 解析到的全部 bounded topology 挂载目标。
//
// Profile schema 允许 scope.refs 最多 100 项，所以一个 binding 可以合法地指向多个对象。
// 这里保留**全部**可解析目标：只留最后一个会静默丢掉前面的 ref，而且结果会取决于 ref 与
// 拓扑的遍历顺序。两个列表都按 ID 升序去重，因此输出顺序不影响任何结论。
type SignalPlacement struct {
	BindingKey string
	// NodeIDs 是解析到的 canonical 实体 ID（升序去重）。
	NodeIDs []string
	// RelationIDs 是解析到的 canonical 关系 ID（升序去重）。
	//
	// 多个 ref 指向同一条 canonical 关系（例如同时写了 canonical ID 与被折叠的成员 ID）
	// 时只保留一次：归一化后它们本来就是同一个对象。
	RelationIDs []string
}

// AttachmentCount 返回解析到的 canonical 对象总数。
func (p SignalPlacement) AttachmentCount() int { return len(p.NodeIDs) + len(p.RelationIDs) }

// Resolved 表示这个 binding 至少解析到一个 canonical 对象。
func (p SignalPlacement) Resolved() bool { return p.AttachmentCount() > 0 }

// SingleTarget 在且仅在恰好解析到一个 canonical 对象时返回该对象。
//
// 只有这种情况下数值才明确属于该对象，逐对象展示才是无损的。解析到多个对象时数值是跨对象
// 聚合值，把它复制到每个对象上就是伪造逐对象测量，因此这里返回 false。
func (p SignalPlacement) SingleTarget() (nodeID, relationID string, ok bool) {
	if p.AttachmentCount() != 1 {
		return "", "", false
	}
	if len(p.NodeIDs) == 1 {
		return p.NodeIDs[0], "", true
	}
	return "", p.RelationIDs[0], true
}

// resolvePlacements 解析每个非 Golden binding 的全部归属，并算出各对象的健康。
//
// 五条硬约束：
//
//  1. 保留**全部**可解析的挂载目标。Profile 允许 scope.refs 最多 100 项，只留最后一个
//     会静默丢掉前面的 ref，而且结果会取决于遍历顺序；两个列表都升序去重，因此输出顺序
//     不影响任何结论。
//  2. 指向同一条 canonical 关系的多个 ref（canonical ID 与被折叠的成员 ID）只算一个对象：
//     归一化之后它们本来就是同一个对象。健康意见因此是"每 binding 每 canonical 对象一次"，
//     不会被重复计票。
//  3. scope.refs 缺省、为空或悬空时不发明 node/relation，也不扩大查询边界，更不会把
//     binding 硬挂到 service 节点上冒充节点观测——它只是没有可挂载的位置。
//  4. 这类 binding 仍然完整保留在 evaluator 结果与 Context.Signals 里，缺口通过
//     UnresolvedBindings / AggregateOnlyBindings（进而 section 与 incomplete 摘要）表达。
//  5. Golden 恒在顶层 golden_signals，不参与拓扑挂载。
//
// relation 引用同时接受 canonical ID 与被折叠的成员 ID：canonical 归一化不能让原本有效的
// scope refs 变成悬空引用。
func resolvePlacements(snapshot *AssemblySnapshot) {
	snapshot.Placements = []SignalPlacement{}
	snapshot.UnresolvedBindings = []string{}
	snapshot.AggregateOnlyBindings = []string{}
	snapshot.NodeHealth = map[string]model.Health{}
	snapshot.RelationHealth = map[string]model.Health{}
	if snapshot.Profile == nil {
		return
	}

	nodeIDs := make(map[string]struct{}, len(snapshot.TopologyFacts.Nodes))
	for _, node := range snapshot.TopologyFacts.Nodes {
		nodeIDs[node.EntityID] = struct{}{}
	}

	nodeHealths := map[string][]model.SignalHealth{}
	relationHealths := map[string][]model.SignalHealth{}
	for _, evaluation := range snapshot.Evaluation.Signals {
		binding := evaluation.Binding
		if binding.Family == model.SignalFamilyGolden {
			continue
		}
		placement := SignalPlacement{
			BindingKey:  binding.Key,
			NodeIDs:     []string{},
			RelationIDs: []string{},
		}
		seenNodes := map[string]struct{}{}
		seenRelations := map[string]struct{}{}
		for _, ref := range binding.Scope.Refs {
			switch binding.Scope.Kind {
			case model.ScopeKindRelation:
				for _, relation := range snapshot.TopologyFacts.Relations {
					if !relation.ResolvesRelationRef(ref) {
						continue
					}
					// 一个 ref 至多属于一条 canonical 关系（ID 是数据库主键，每行只属于
					// 一个归一化组），命中即停。
					if _, duplicate := seenRelations[relation.RelationID]; !duplicate {
						seenRelations[relation.RelationID] = struct{}{}
						placement.RelationIDs = append(placement.RelationIDs, relation.RelationID)
					}
					break
				}
			case model.ScopeKindEntity, model.ScopeKindResource:
				if _, ok := nodeIDs[ref]; !ok {
					continue
				}
				if _, duplicate := seenNodes[ref]; !duplicate {
					seenNodes[ref] = struct{}{}
					placement.NodeIDs = append(placement.NodeIDs, ref)
				}
			case model.ScopeKindService:
				// service 级的非 Golden binding 没有拓扑挂载位置。
			}
		}
		sort.Strings(placement.NodeIDs)
		sort.Strings(placement.RelationIDs)

		// 健康意见按"每个 binding、每个 canonical 对象一次"投递：去重后的列表保证同一条
		// binding 不会因为写了两个指向同一对象的 ref 而被重复计票。
		for _, id := range placement.NodeIDs {
			nodeHealths[id] = append(nodeHealths[id], evaluation.Health)
		}
		for _, id := range placement.RelationIDs {
			relationHealths[id] = append(relationHealths[id], evaluation.Health)
		}

		switch {
		case !placement.Resolved():
			snapshot.UnresolvedBindings = append(snapshot.UnresolvedBindings, binding.Key)
		case placement.AttachmentCount() > 1:
			// 数值是跨对象聚合值，逐对象展示会伪造测量。完整判定仍保留在 evaluator 与
			// DiagnosticContext 里，缺口由稳定 reason 表达。
			snapshot.AggregateOnlyBindings = append(snapshot.AggregateOnlyBindings, binding.Key)
		}
		snapshot.Placements = append(snapshot.Placements, placement)
	}
	sort.Strings(snapshot.UnresolvedBindings)
	sort.Strings(snapshot.AggregateOnlyBindings)
	sort.Slice(snapshot.Placements, func(i, j int) bool {
		return snapshot.Placements[i].BindingKey < snapshot.Placements[j].BindingKey
	})

	for id, healths := range nodeHealths {
		snapshot.NodeHealth[id] = health.AggregateObjectHealth(healths)
	}
	for id, healths := range relationHealths {
		snapshot.RelationHealth[id] = health.AggregateObjectHealth(healths)
	}
}

// PlacementFor 返回某个 binding 的挂载结果。
func (s *AssemblySnapshot) PlacementFor(bindingKey string) (SignalPlacement, bool) {
	for _, placement := range s.Placements {
		if placement.BindingKey == bindingKey {
			return placement, true
		}
	}
	return SignalPlacement{}, false
}

// ObjectHealth 返回某个拓扑对象的健康。
//
// 没有任何 signal 挂在它上面时恒为 Unknown：拓扑里存在一个对象不能因此显示 Healthy。
func (s *AssemblySnapshot) ObjectHealth(nodeID, relationID string) model.Health {
	if nodeID != "" {
		if status, ok := s.NodeHealth[nodeID]; ok {
			return status
		}
	}
	if relationID != "" {
		if status, ok := s.RelationHealth[relationID]; ok {
			return status
		}
	}
	return model.HealthUnknown
}

// ListServices 是目录用的轻量身份查询直通。它不组装 Context：目录只需要身份 + 缓存摘要。
func (a *Assembler) ListServices(
	ctx stdcontext.Context, filter serviceobs.RegistrationFilter,
) (serviceobs.RegistrationList, error) {
	deadline := a.Deadlines.withDefaults().Registration
	scoped, cancel := stdcontext.WithTimeout(ctx, deadline)
	defer cancel()
	return a.Registrations.ListServices(scoped, filter)
}

func (a *Assembler) readRegistration(
	ctx stdcontext.Context, deadline time.Duration, serviceID string,
) (serviceobs.ServiceFacts, error) {
	if a.Registrations == nil {
		return serviceobs.ServiceFacts{}, errors.New("serviceobs/context: 未注册 registration reader")
	}
	scoped, cancel := stdcontext.WithTimeout(ctx, deadline)
	defer cancel()
	return a.Registrations.ReadService(scoped, serviceID)
}

// readTopology 读取 bounded canonical neighborhood。失败只降级 topology section 与
// source，绝不影响 domain。
func (a *Assembler) readTopology(
	ctx stdcontext.Context, deadline time.Duration, snapshot *AssemblySnapshot, limits Limits,
) {
	if a.Topology == nil {
		snapshot.addSection(serviceobs.SectionKeyTopology, model.AvailabilityUnavailable,
			serviceobs.ReasonTopologyReadFailed, false)
		snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
			SourceKey: serviceobs.SourceKeyTopology, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonTopologyReadFailed,
		})
		return
	}
	maxNodes, maxRelations := limits.MaxNodes, limits.MaxRelations
	if snapshot.Profile != nil {
		maxNodes = minPositive(maxNodes, snapshot.Profile.Limits.MaxNodes)
		maxRelations = minPositive(maxRelations, snapshot.Profile.Limits.MaxRelations)
	}
	request := serviceobs.TopologyRequest{
		ServiceEntityID:      snapshot.Identity.EntityID,
		DeclaredDependencies: snapshot.Registration.Dependencies,
		MaxNodes:             maxNodes,
		MaxRelations:         maxRelations,
	}
	if snapshot.Profile != nil {
		request.RootEntityIDs = snapshot.Profile.ServiceRef.RootEntityIDs
	}

	scoped, cancel := stdcontext.WithTimeout(ctx, deadline)
	defer cancel()
	facts, err := a.Topology.ReadTopology(scoped, request)
	if err != nil {
		// 拓扑读不到只让 topology section Unavailable；declared 概览无法显示，但
		// identity 与 domain 完全不受影响。
		snapshot.addSection(serviceobs.SectionKeyTopology, model.AvailabilityUnavailable,
			serviceobs.ReasonTopologyReadFailed, true)
		snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
			SourceKey: serviceobs.SourceKeyTopology, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonTopologyReadFailed,
			Retryable: true,
		})
		return
	}
	snapshot.TopologyFacts = facts
	snapshot.FactAnchor = laterOf(snapshot.FactAnchor, facts.ObservedAt)

	state := model.AvailabilityAvailable
	reason := ""
	if len(facts.Nodes) == 0 {
		// 一次成功查询但没有结果是 Empty，不是失败，也不把 Data 变成 Partial。
		state = model.AvailabilityEmpty
	}
	if facts.Partial {
		// 达到 node/relation 上限：已返回的事实与 topology.partial 全部保留，但这个来源
		// 的覆盖确实不完整，所以状态必须是 unknown 而不是 available。
		// availability 词表里没有 "partial"，unknown 是表达"这一块判不准"的合法取值，
		// 它同时把整页 Data 推到 Partial——否则其它来源都正常时会算出 Data Complete，
		// 等于把一张被截断的图报成完整数据。
		state = model.AvailabilityUnknown
		reason = facts.PartialReason
		if reason == "" {
			reason = serviceobs.ReasonTopologyBoundsReached
		}
	}
	snapshot.addSection(serviceobs.SectionKeyTopology, state, reason, false)

	freshness := model.FreshnessFresh
	for _, relation := range facts.Relations {
		// observed overlay 过期只让 Data Stale，declared 底图与 Health 都不受影响。
		if relation.Stale {
			freshness = model.FreshnessStale
			break
		}
	}
	snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
		SourceKey: serviceobs.SourceKeyTopology, State: state, Freshness: freshness,
		ObservedAt: facts.ObservedAt, ReasonCode: reason, Retryable: true,
	})
}

// readDomain 读取稳定边界内的领域事实。它的边界由 domain adapter 独立计算，
// 与 topology 是否成功完全无关。
func (a *Assembler) readDomain(
	ctx stdcontext.Context, deadline time.Duration, snapshot *AssemblySnapshot, limits Limits,
) error {
	if a.Domain == nil {
		snapshot.addSection(serviceobs.SectionKeyDomain, model.AvailabilityUnavailable,
			serviceobs.ReasonDomainReadFailed, false)
		snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
			SourceKey: serviceobs.SourceKeyDomain, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonDomainReadFailed,
		})
		return nil
	}
	request := serviceobs.DomainRequest{
		ServiceID:       snapshot.Identity.ServiceID,
		ServiceEntityID: snapshot.Identity.EntityID,
		Focus:           snapshot.Focus,
		Limit:           limits.MaxDomainRows,
	}
	if snapshot.Profile != nil {
		request.RootEntityIDs = snapshot.Profile.ServiceRef.RootEntityIDs
		request.AllowedFocusKinds = focusKindKeys(*snapshot.Profile)
	}

	scoped, cancel := stdcontext.WithTimeout(ctx, deadline)
	defer cancel()
	facts, err := a.Domain.ReadDomain(scoped, request)
	if err != nil {
		snapshot.addSection(serviceobs.SectionKeyDomain, model.AvailabilityUnavailable,
			serviceobs.ReasonDomainReadFailed, true)
		snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
			SourceKey: serviceobs.SourceKeyDomain, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonDomainReadFailed,
			Retryable: true,
		})
		return err
	}
	snapshot.DomainFacts = facts
	snapshot.FactAnchor = laterOf(snapshot.FactAnchor, facts.LatestFactAt)

	state := model.AvailabilityAvailable
	reason := ""
	if facts.BoundaryTruncated {
		state = model.AvailabilityUnknown
		reason = serviceobs.ReasonDomainBoundsReached
	} else if len(facts.Alerts) == 0 && len(facts.Faults) == 0 &&
		len(facts.Impacts) == 0 && len(facts.Incidents) == 0 {
		state = model.AvailabilityEmpty
		if facts.FocusApplied {
			reason = serviceobs.ReasonFocusNotObserved
		}
	}
	snapshot.addSection(serviceobs.SectionKeyDomain, state, reason, false)
	snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
		SourceKey: serviceobs.SourceKeyDomain, State: state,
		// 领域事实是本系统已提交的状态，读到就是当前值。
		Freshness: model.FreshnessFresh, ObservedAt: facts.LatestFactAt,
		ReasonCode: reason, Retryable: true,
	})
	return nil
}

// readObservations 读取语义信号。Phase 2 注册的是 UnavailableObservationReader，
// 因此这里稳定地把 Golden/RED/USE 降级成 Unknown，而 declared topology 与 domain 保留。
func (a *Assembler) readObservations(
	ctx stdcontext.Context, deadline time.Duration, snapshot *AssemblySnapshot,
) {
	if snapshot.Profile == nil {
		// 没有 Profile 就没有 binding：Data 由 evaluator 判成 Unconfigured。
		snapshot.addSection(serviceobs.SectionKeyGolden, model.AvailabilityUnknown,
			serviceobs.ReasonProfileUnconfigured, false)
		return
	}
	if a.Observations == nil {
		snapshot.addSection(serviceobs.SectionKeyGolden, model.AvailabilityUnavailable,
			serviceobs.ReasonTelemetryNotConnected, false)
		snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
			SourceKey: serviceobs.SourceKeyTelemetry, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonTelemetryNotConnected,
		})
		return
	}
	scoped, cancel := stdcontext.WithTimeout(ctx, deadline)
	defer cancel()
	facts, err := a.Observations.ReadObservations(scoped, serviceobs.ObservationRequest{
		Profile:       *snapshot.Profile,
		ServiceID:     snapshot.Identity.ServiceID,
		Environment:   snapshot.Identity.Environment,
		WindowSeconds: snapshot.WindowSeconds,
		From:          mustParse(snapshot.Scope.TimeRange.From),
		To:            mustParse(snapshot.Scope.TimeRange.To),
		Focus:         snapshot.Focus,
	})
	if err != nil {
		snapshot.addSection(serviceobs.SectionKeyGolden, model.AvailabilityUnavailable,
			serviceobs.ReasonTelemetryNotConnected, true)
		snapshot.Sources = append(snapshot.Sources, health.SourceObservation{
			SourceKey: serviceobs.SourceKeyTelemetry, State: model.AvailabilityUnavailable,
			Freshness: model.FreshnessUnknown, ReasonCode: serviceobs.ReasonTelemetryNotConnected,
			Retryable: true,
		})
		return
	}
	snapshot.Signals = append(snapshot.Signals, facts.Signals...)
	snapshot.Sources = append(snapshot.Sources, facts.Sources...)

	goldenState, goldenReason := observationSectionState(facts.Signals)
	snapshot.addSection(serviceobs.SectionKeyGolden, goldenState, goldenReason, false)

	evidenceState := model.AvailabilityUnavailable
	evidenceReason := serviceobs.ReasonTelemetryNotConnected
	if a.EvidenceConnected {
		snapshot.EvidenceRefs = append(snapshot.EvidenceRefs, facts.EvidenceRefs...)
		evidenceState, evidenceReason = model.AvailabilityAvailable, ""
		if len(facts.EvidenceRefs) == 0 {
			evidenceState = model.AvailabilityEmpty
		}
	}
	snapshot.addSection(serviceobs.SectionKeyEvidence, evidenceState, evidenceReason, false)
}

// observationSectionState 用观测本身的可用性决定 section 状态，绝不因为"接口返回成功"
// 就报 available。
func observationSectionState(signals []health.SignalObservation) (model.Availability, string) {
	if len(signals) == 0 {
		return model.AvailabilityUnavailable, serviceobs.ReasonTelemetryNotConnected
	}
	available := 0
	reason := ""
	for _, signal := range signals {
		if signal.Availability == model.SignalAvailabilityAvailable {
			available++
			continue
		}
		if reason == "" {
			reason = signal.UnknownReason
		}
	}
	switch {
	case available == len(signals):
		return model.AvailabilityAvailable, ""
	case available == 0:
		if reason == "" {
			reason = serviceobs.ReasonTelemetryNotConnected
		}
		return model.AvailabilityUnavailable, reason
	default:
		if reason == "" {
			reason = serviceobs.ReasonTelemetryNotConnected
		}
		return model.AvailabilityUnknown, reason
	}
}

// capabilities 完全由服务端计算：session 决定"这个人能不能"，adapter 注册情况决定
// "系统能不能"。Phase 2 只有 view_domain 为 true。
func (a *Assembler) capabilities() model.Capabilities {
	return model.Capabilities{
		ViewMetrics: a.TelemetryConnected,
		ViewLogs:    a.EvidenceConnected,
		ViewTraces:  a.EvidenceConnected,
		// 当前产品没有 service-level visibility policy，已登录用户都能读领域状态。
		ViewDomain:            a.Domain != nil,
		OpenEvidenceDeepLinks: a.EvidenceConnected,
	}
}

func (a *Assembler) policy(published *model.Profile, limits Limits) model.ContextPolicy {
	policy := model.ContextPolicy{
		AllowedToolKeys:  []string{},
		MaxWindowSeconds: limits.MaxWindowSeconds,
		MaxRows:          limits.MaxRows,
		// 这些字段名进 EvidenceRef 与日志脱敏口径，属于稳定策略而不是内容。
		RedactedFields: []string{"authorization", "cookie", "password", "token"},
	}
	if published != nil {
		policy.AllowedToolKeys = append(policy.AllowedToolKeys, published.ToolRefs...)
		sort.Strings(policy.AllowedToolKeys)
		// 每次调用的有效限制取 Profile 与全局限制的最小值。
		policy.MaxWindowSeconds = minPositive(policy.MaxWindowSeconds, published.Limits.MaxWindowSeconds)
		policy.MaxRows = minPositive(policy.MaxRows, published.Limits.MaxRows)
	}
	if policy.MaxWindowSeconds < minPolicyWindowSeconds {
		policy.MaxWindowSeconds = minPolicyWindowSeconds
	}
	return policy
}

func (a *Assembler) publishedProfile(serviceID string) (*model.Profile, bool) {
	if a.Profiles == nil {
		return nil, false
	}
	published, ok := a.Profiles.PublishedForService(serviceID)
	if !ok {
		return nil, false
	}
	return &published, true
}

func (a *Assembler) profileDiagnostic(serviceID string) (*profile.Diagnostic, bool) {
	if a.Profiles == nil {
		return nil, false
	}
	diagnostic, ok := a.Profiles.DiagnosticForService(serviceID)
	if !ok {
		return nil, false
	}
	return &diagnostic, true
}

func (a *Assembler) now() time.Time {
	if a.Clock == nil {
		return time.Now().UTC()
	}
	return a.Clock().UTC()
}

func (a *Assembler) newRequestID() (string, error) {
	if a.NewRequestID != nil {
		return a.NewRequestID()
	}
	return NewRequestID()
}

// requestIDPattern 与 diagnostic-context schema 的 request_id 一致。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// NewRequestID 生成服务端 request ID。它永远不采信客户端传入的值：request ID 会进
// 审计，客户端可控就等于审计可伪造。
func NewRequestID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("serviceobs/context: 生成 request ID 失败: %w", err)
	}
	id := "req_" + hex.EncodeToString(buffer)
	if !requestIDPattern.MatchString(id) {
		return "", errors.New("serviceobs/context: 生成的 request ID 不符合契约格式")
	}
	return id, nil
}

// PermissionSnapshotID 对 session user ID、规范化角色/capabilities 与 policy version
// 做 SHA-256 指纹。
//
// 它必须由服务端计算：URL 或客户端传入的快照 ID 等于让调用方自选权限。policy version
// 参与指纹，保证策略语义变化时快照 ID 必然变化。
func PermissionSnapshotID(subject Subject, capabilities model.Capabilities) string {
	parts := []string{serviceobs.PolicyVersion, subject.UserID}
	parts = append(parts, "teams="+strings.Join(sortedUnique(subject.TeamIDs), ","))
	parts = append(parts, "roles="+strings.Join(sortedUnique(subject.Roles), ","))
	parts = append(parts, "caps="+strings.Join(sortedUnique(capabilityNames(capabilities)), ","))
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return "psid_" + hex.EncodeToString(digest[:])
}

// capabilityNames 把 capabilities 规范化成有序名称集合，避免布尔字段顺序影响指纹。
func capabilityNames(capabilities model.Capabilities) []string {
	names := []string{}
	for name, enabled := range map[string]bool{
		"view_metrics":             capabilities.ViewMetrics,
		"view_logs":                capabilities.ViewLogs,
		"view_traces":              capabilities.ViewTraces,
		"view_domain":              capabilities.ViewDomain,
		"open_evidence_deep_links": capabilities.OpenEvidenceDeepLinks,
	} {
		if enabled {
			names = append(names, name)
		}
	}
	return names
}

// resolveScope 生成本次请求的 scope，并校验窗口不超过 Profile/global 上界。
//
// from/to 必须成对出现、必须是绝对时间、必须 from < to。只传一个或顺序颠倒都返回
// typed 错误，绝不静默补齐或交换——那会让用户看到一个自己没请求过的时间窗。
func resolveScope(
	req Request, environment string, published *model.Profile, limits Limits, now time.Time,
) (model.ContextScope, int, error) {
	timezone := req.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	if len(timezone) > maxTimezoneLength {
		return model.ContextScope{}, 0, fmt.Errorf("%w: 长度 %d 超过上限 %d",
			ErrInvalidTimezone, len(timezone), maxTimezoneLength)
	}
	defaultWindow := defaultDefaultWindowSecs
	maxWindow := limits.MaxWindowSeconds
	if published != nil {
		if published.Limits.DefaultWindowSeconds > 0 {
			defaultWindow = published.Limits.DefaultWindowSeconds
		}
		maxWindow = minPositive(maxWindow, published.Limits.MaxWindowSeconds)
	}

	from, to := req.From, req.To
	switch {
	case from.IsZero() && to.IsZero():
		// 服务端只取一次 now，保证 from/to 一致。
		to = now
		window := defaultWindow
		if maxWindow > 0 && window > maxWindow {
			// Profile schema 允许 default_window 独立于 max_window 配置；
			// 缺省窗口取 min(default, max)，避免合法 Profile 自我 422。
			window = maxWindow
		}
		from = to.Add(-time.Duration(window) * time.Second)
	case from.IsZero() || to.IsZero():
		return model.ContextScope{}, 0, fmt.Errorf("%w: from 与 to 必须同时出现", ErrInvalidTimeRange)
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return model.ContextScope{}, 0, fmt.Errorf("%w: from 必须早于 to", ErrInvalidTimeRange)
	}
	duration := to.Sub(from)
	// 必须是整秒。契约的 window_seconds 是整数，直接截断会让 3600.9s 的请求算出 3600：
	// 既绕过了 3600s 的上限，也让 window_seconds 与 scope 里的 from/to 互相矛盾。
	if duration%time.Second != 0 {
		return model.ContextScope{}, 0, fmt.Errorf("%w: 窗口必须是整秒，实际 %s",
			ErrInvalidTimeRange, duration)
	}
	windowSeconds := int(duration / time.Second)
	if windowSeconds < 1 {
		return model.ContextScope{}, 0, fmt.Errorf("%w: 窗口不足 1 秒", ErrInvalidTimeRange)
	}
	// 上限比较用真实时长，不用截断后的整数，因此不存在任何绕过上限的余量。
	if duration > time.Duration(maxWindow)*time.Second {
		// 超限显式报错，绝不静默截断成一个用户没请求过的窗口。
		return model.ContextScope{}, 0, fmt.Errorf("%w: 窗口 %ds 超过上限 %ds",
			serviceobs.ErrScopeTooBroad, windowSeconds, maxWindow)
	}

	return model.ContextScope{
		// 当前单租户部署固定 default，不接受 URL 输入。
		Tenant:      serviceobs.TenantDefault,
		Environment: environment,
		TimeRange: model.TimeRange{
			From: model.NewTimestamp(from),
			To:   model.NewTimestamp(to),
		},
		Timezone: timezone,
	}, windowSeconds, nil
}

// resolveFocus 校验 focus 必须由已发布 Profile 声明，且 value 过该 focus 的
// value_pattern。没有 Profile 时不存在合法 focus。
func resolveFocus(requested *model.Focus, published *model.Profile) (*model.Focus, error) {
	if requested == nil {
		return nil, nil
	}
	if published == nil {
		return nil, serviceobs.ErrProfileUnconfigured
	}
	for _, declared := range published.FocusKinds {
		if declared.Key != requested.Kind {
			continue
		}
		pattern, err := regexp.Compile(declared.ValuePattern)
		if err != nil {
			// Registry 已经保证 pattern 可编译；真到这里说明配置被绕过，fail closed。
			return nil, fmt.Errorf("%w: focus value_pattern 无法编译", serviceobs.ErrInvalidFocus)
		}
		if !pattern.MatchString(requested.Value) {
			return nil, fmt.Errorf("%w: focus value 不满足 %s 的 value_pattern",
				serviceobs.ErrInvalidFocus, declared.Key)
		}
		focus := &model.Focus{Kind: requested.Kind, Value: requested.Value}
		// journey/stage 只能来自 Profile 声明，不接受客户端自报。
		for _, journey := range published.Journeys {
			if journey.FocusKind == declared.Key {
				focus.Journey = journey.Key
				break
			}
		}
		return focus, nil
	}
	return nil, fmt.Errorf("%w: focus kind %q 未由已发布 Profile 声明",
		serviceobs.ErrInvalidFocus, requested.Kind)
}

// focusKindKeys 返回已发布 Profile 的 focus_kinds key 精确集合。它就是 domain adapter
// 的 focus 白名单来源。
func focusKindKeys(published model.Profile) []string {
	keys := make([]string, 0, len(published.FocusKinds))
	for _, focus := range published.FocusKinds {
		keys = append(keys, focus.Key)
	}
	return keys
}

// buildDiagnosticContext 生成严格镜像 v1 schema 的只读事实快照。
//
// 它只在有 Profile 时被调用：契约要求 service.root_entity_ids 非空、profile_key 非空、
// profile_version >= 1，Unconfigured 状态下不存在合法的 DiagnosticContext。
func buildDiagnosticContext(snapshot *AssemblySnapshot) *model.DiagnosticContext {
	published := *snapshot.Profile
	context := &model.DiagnosticContext{
		SchemaVersion:  model.SchemaVersion,
		ContextVersion: 1,
		RequestID:      snapshot.RequestID,
		AssembledAt:    model.NewTimestamp(snapshot.AssembledAt),
		Actor: model.Actor{
			UserID:               snapshot.Subject.UserID,
			TeamIDs:              sortedUnique(snapshot.Subject.TeamIDs),
			Roles:                sortedUnique(snapshot.Subject.Roles),
			PermissionSnapshotID: PermissionSnapshotID(snapshot.Subject, snapshot.Capabilities),
		},
		Service: model.ContextService{
			ServiceID:      snapshot.Identity.ServiceID,
			EntityKey:      snapshot.Identity.EntityKey,
			Name:           snapshot.Identity.Name,
			RootEntityIDs:  append([]string{}, published.ServiceRef.RootEntityIDs...),
			ProfileKey:     published.ProfileKey,
			ProfileVersion: published.ProfileVersion,
		},
		Scope: snapshot.Scope,
		Focus: snapshot.Focus,
		Topology: model.ContextTopology{
			SnapshotID: snapshot.TopologyFacts.SnapshotID,
			Partial:    snapshot.TopologyFacts.Partial,
		},
		Signals:      []model.Signal{},
		EvidenceRefs: []model.EvidenceRef{},
		Policy:       snapshot.Policy,
	}
	if context.Topology.SnapshotID == "" {
		// 拓扑读取失败时仍需要一个稳定非空的 snapshot ID：它表示"这次没有可用拓扑"，
		// 而不是某张真实的图。
		context.Topology.SnapshotID = "topo_unavailable"
	}
	// 拓扑不可用或为空时 observed_at 是零值。契约要求它必填且是合法 date-time，但绝不能
	// 用 wall clock 填——那会把"没有拓扑观测"渲染成"刚刚观测过"。口径与 signal 一致：
	// 取已提交事实的真实时间锚，没有锚时退回 Unix epoch。freshness 仍由原始事实判定，
	// 因此这里不会把 unknown 说成 fresh。
	context.Topology.ObservedAt = model.NewTimestamp(
		ObservedAtOr(snapshot.TopologyFacts.ObservedAt, snapshot.FactAnchor))
	context.Topology.Freshness = topologyFreshness(snapshot)
	context.Topology.Nodes = contextNodes(snapshot)
	context.Topology.Relations = contextRelations(snapshot)
	context.Signals = contextSignals(snapshot)
	context.EvidenceRefs = append(context.EvidenceRefs, snapshot.EvidenceRefs...)
	context.DomainState = contextDomainState(snapshot)
	context.Normalize()
	return context
}

func topologyFreshness(snapshot *AssemblySnapshot) model.Freshness {
	if snapshot.TopologyFacts.ObservedAt.IsZero() {
		return model.FreshnessUnknown
	}
	for _, relation := range snapshot.TopologyFacts.Relations {
		if relation.Stale {
			return model.FreshnessStale
		}
	}
	return model.FreshnessFresh
}

func contextNodes(snapshot *AssemblySnapshot) []model.ContextNode {
	nodes := make([]model.ContextNode, 0, len(snapshot.TopologyFacts.Nodes))
	for _, node := range snapshot.TopologyFacts.Nodes {
		nodes = append(nodes, model.ContextNode{
			EntityID:  node.EntityID,
			EntityKey: node.EntityKey,
			Name:      node.Name,
			Layer:     string(node.Layer),
			Kind:      node.Kind,
			// 健康来自共享的 scoped signal 聚合，因此 context 与 Page View 不可能漂移。
			// 没有任何 signal 挂在这个节点上时恒为 Unknown：拓扑里存在一个节点不等于
			// 它健康。
			Health:    snapshot.ObjectHealth(node.EntityID, ""),
			Freshness: nodeFreshness(node),
		})
	}
	return nodes
}

func nodeFreshness(node serviceobs.TopologyNode) model.Freshness {
	if node.ObservedAt.IsZero() {
		return model.FreshnessUnknown
	}
	return model.FreshnessFresh
}

func contextRelations(snapshot *AssemblySnapshot) []model.ContextRelation {
	relations := make([]model.ContextRelation, 0, len(snapshot.TopologyFacts.Relations))
	for _, relation := range snapshot.TopologyFacts.Relations {
		freshness := model.FreshnessFresh
		if relation.Stale {
			freshness = model.FreshnessStale
		}
		relations = append(relations, model.ContextRelation{
			RelationID:     relation.RelationID,
			SourceEntityID: relation.SourceEntityID,
			TargetEntityID: relation.TargetEntityID,
			Kind:           relation.Kind,
			Provenance:     relation.Provenance,
			Freshness:      freshness,
		})
	}
	return relations
}

// contextSignals 把 evaluator 的判定结果投影成契约 signal。
//
// 每个已声明 binding 都必须出现，包括 scope.refs 悬空的那些：evaluator 的判定是事实，
// "挂不到 bounded topology"只是投影层面的局部缺口。
func contextSignals(snapshot *AssemblySnapshot) []model.Signal {
	signals := make([]model.Signal, 0, len(snapshot.Evaluation.Signals))
	for _, evaluation := range snapshot.Evaluation.Signals {
		signals = append(signals, model.Signal{
			BindingKey:      evaluation.Binding.Key,
			Family:          evaluation.Binding.Family,
			Semantic:        evaluation.Binding.Semantic,
			ScopeKind:       evaluation.Binding.Scope.Kind,
			ScopeRef:        ScopeRef(evaluation.Binding, snapshot.Identity.ServiceID),
			Value:           evaluation.Value,
			Unit:            evaluation.Binding.Unit,
			Aggregation:     evaluation.Binding.Aggregation,
			WindowSeconds:   snapshot.WindowSeconds,
			ObservedAt:      model.NewTimestamp(ObservedAtOr(evaluation.ObservedAt, snapshot.FactAnchor)),
			Freshness:       evaluation.Freshness,
			Availability:    evaluation.Availability,
			Health:          evaluation.Health,
			SourceKey:       signalSourceKey(evaluation),
			MeasurementRole: evaluation.MeasurementRole,
			ProxyReason:     evaluation.ProxyReason,
			UnknownReason:   evaluation.UnknownReason,
			ThresholdSource: evaluation.ThresholdSource,
		})
	}
	return signals
}

func signalSourceKey(evaluation health.SignalEvaluation) string {
	if evaluation.SourceKey != "" {
		return evaluation.SourceKey
	}
	// adapter 完全没返回这个 binding 时也要有稳定 source key：它归属声明它的遥测来源。
	return serviceobs.SourceKeyTelemetry
}

// ScopeRef 返回 signal 的 scope 引用。
//
// scope.refs 缺省或为空时退回 service ID：契约要求 scope_ref 非空，而 service 是这个
// binding 唯一可确定的归属对象。这只是标注归属，**不会**把信号挂到任何 topology 节点上，
// 也不会发明节点。
func ScopeRef(binding model.SignalBinding, serviceID string) string {
	if len(binding.Scope.Refs) > 0 && binding.Scope.Refs[0] != "" {
		return binding.Scope.Refs[0]
	}
	return serviceID
}

// ObservedAtOr 决定一个观测的 observed_at。
//
// evaluator 用零值表示"从未观测过"（Slice A 的内部语义）。契约要求 observed_at 必填且
// 是合法 date-time，所以这里必须给一个值——但绝不能用 wall clock now，那会把
// "还没观测到"伪装成"刚刚观测过"。口径与 snapshot cache 占位一致：取已提交事实里最新的
// 真实时间锚。连事实锚都没有时退回 Unix epoch，它明确表示"没有任何观测"。
func ObservedAtOr(observedAt, factAnchor time.Time) time.Time {
	if !observedAt.IsZero() {
		return observedAt.UTC()
	}
	if !factAnchor.IsZero() {
		return factAnchor.UTC()
	}
	return time.Unix(0, 0).UTC()
}

func contextDomainState(snapshot *AssemblySnapshot) model.ContextDomainState {
	state := model.ContextDomainState{
		Alerts:    []model.DomainRef{},
		Faults:    []model.DomainRef{},
		Impacts:   []model.DomainRef{},
		Incidents: []model.DomainRef{},
	}
	for _, alert := range snapshot.DomainFacts.Alerts {
		state.Alerts = append(state.Alerts, model.DomainRef{
			ID: alert.AlertID, State: alert.State, UpdatedAt: model.NewTimestamp(alert.UpdatedAt),
		})
	}
	for _, fault := range snapshot.DomainFacts.Faults {
		state.Faults = append(state.Faults, model.DomainRef{
			ID: fault.FaultID, State: fault.State, UpdatedAt: model.NewTimestamp(fault.UpdatedAt),
		})
	}
	for _, impact := range snapshot.DomainFacts.Impacts {
		state.Impacts = append(state.Impacts, model.DomainRef{
			// FaultImpact 的身份是 (fault, node) 组合，投影成稳定复合 ID。
			ID:    impact.FaultID + ":" + impact.EntityID,
			State: impact.State, UpdatedAt: model.NewTimestamp(impact.UpdatedAt),
		})
	}
	for _, incident := range snapshot.DomainFacts.Incidents {
		state.Incidents = append(state.Incidents, model.DomainRef{
			ID: incident.IncidentID, State: incident.State,
			UpdatedAt: model.NewTimestamp(incident.UpdatedAt),
		})
	}
	return state
}

// addSection 追加一个 section 状态，同一 key 只保留第一次结果。
func (s *AssemblySnapshot) addSection(key string, state model.Availability, reason string, retryable bool) {
	for _, existing := range s.Sections {
		if existing.SectionKey == key {
			return
		}
	}
	s.Sections = append(s.Sections, model.SectionStatus{
		SectionKey: key, State: state, ReasonCode: reason, Retryable: retryable,
	})
}

// SectionState 返回某个 section 的状态，便于 projector 与测试查询。
func (s *AssemblySnapshot) SectionState(key string) (model.SectionStatus, bool) {
	for _, section := range s.Sections {
		if section.SectionKey == key {
			return section, true
		}
	}
	return model.SectionStatus{}, false
}

func mustParse(ts model.Timestamp) time.Time {
	parsed, err := ts.Time()
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func sortedUnique(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func minPositive(a, b int) int {
	if b <= 0 {
		return a
	}
	if a <= 0 || b < a {
		return b
	}
	return a
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
