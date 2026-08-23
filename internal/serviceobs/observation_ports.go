package serviceobs

import (
	"context"
	"sort"
	"time"

	"alert-system/internal/serviceobs/health"
	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

// 本文件是 Phase 3 的 source-specific 只读端口。
//
// 它把 Phase 2 那个单一 ObservationReader 拆成"每个真实数据源一个端口"，理由是降级粒度：
// 一个 ObservationReader 失败只能整体降级，而 OpenObserve logs 被拒绝时 traces 仍然可用、
// Prometheus 超时时 observed path 仍然存在。拆开之后每个来源有自己的 deadline、limits、
// source status 与 reason code，"最小范围 Unknown"才真的成立。
//
// 三条约束在类型层面固化：
//
//  1. 请求里带的是**服务端**算出来的 Subject 与服务端选择的 Purpose。URL、query 与 SIP
//     都不能自报身份、用途或权限——客户端自报的角色在 Phase 2 就已经无效，这里继续。
//  2. 端口只收语义 key、范围与 typed limits，绝不收 PromQL/SQL/LogQL 或凭据。
//  3. 每个端口返回"规范化事实 + 自己的 source status"，不返回 Page View，也不自己判 Health。

// source key 是稳定机器码，会进 Page View 的 data_status.sources 与 signal.source_key。
// 只能增加不能改写：前端按 code 本地化文案。
const (
	// SourceKeyPrometheus 是语义指标来源。它与 SourceKeyTelemetry 是同一个值：后者是
	// Phase 2 的历史名字，保留以免既有调用方与快照里的 code 失效。
	SourceKeyPrometheus = SourceKeyTelemetry
	// SourceKeyOpenObserveLogs / Traces / Access 必须分开：logs 被 Forbidden 时 traces
	// 仍然可用，把它们折叠成一个 openobserve source 会让局部权限缺失看起来像整源不可用。
	SourceKeyOpenObserveLogs   = "openobserve_logs"
	SourceKeyOpenObserveTraces = "openobserve_traces"
	SourceKeyOpenObserveAccess = "openobserve_access"
)

// 稳定 reason code。
const (
	// ReasonPrometheusNotConnected / ReasonOpenObserveNotConnected 用于"部分配置"：
	// 当前 purpose 已经选中这个来源，但它没有配置 endpoint。它必须显式出现而不是被省略，
	// 否则页面看起来像"这个来源没有被请求"，用户无法分辨"没查"与"查不到"。
	//
	// 与 ReasonTelemetryNotConnected 的分工：后者表示**完全**没有配置任何 provider，
	// 也就是 Phase 2 的原始语义，必须逐字保留。
	ReasonPrometheusNotConnected  = "prometheus_not_connected"
	ReasonOpenObserveNotConnected = "openobserve_not_connected"
	// ReasonSourceTimeout 是该来源自己的 deadline 到了。父 context 取消不使用这个 code：
	// 请求已经断开时不能伪装成一个可返回的 Partial。
	ReasonSourceTimeout = "source_timeout"
	// ReasonSourceReadFailed 是查询失败（5xx、解析错误、协议错误）。
	ReasonSourceReadFailed = "source_read_failed"
	// ReasonSourceInternalError 是 adapter panic 被隔离后的稳定 code。
	ReasonSourceInternalError = "source_internal_error"
	// ReasonUpstreamAuthFailed 是**平台配置**问题：service account 的凭据失效（401/403）。
	// 它绝不能显示成当前用户的 Forbidden——那会让值班人去申请自己根本不缺的权限。
	ReasonUpstreamAuthFailed = "upstream_auth_failed"
	// ReasonUpstreamRateLimited 是上游限流，可重试。
	ReasonUpstreamRateLimited = "upstream_rate_limited"
	// ReasonPermissionDenied 是**当前用户**缺少该来源所需权限。命中它时一次上游调用都
	// 不会发生。
	ReasonPermissionDenied = "permission_denied"
	// ReasonResponseTooLarge 表示响应超过 byte/row 上界。它是显式失败而不是静默截断：
	// 一个被截断的结果集会让"完整"这件事变得不可判断。
	ReasonResponseTooLarge = "response_too_large"
	// ReasonCardinalityExceeded 表示 series 基数超限。
	ReasonCardinalityExceeded = "cardinality_exceeded"
	// ReasonAliasNotFound / ReasonAliasConflict 与既有 alert mapping 的口径逐字一致。
	ReasonAliasNotFound = "alias_not_found"
	ReasonAliasConflict = "alias_conflict"
	// ReasonAuditUnavailable 表示审计不可写。没有审计的结果一律不得进入 Page View。
	ReasonAuditUnavailable = "audit_unavailable"
	// ReasonNoSeries 表示查询成功但没有匹配 series。
	ReasonNoSeries = "no_series"
	// ReasonStageMappingUnavailable 表示 stage 映射缺失或不可用。
	ReasonStageMappingUnavailable = "stage_mapping_unavailable"
)

// Subject 是服务端从 session 计算出的身份。
//
// 它刻意不包含 capabilities 或 permissions：那两者由服务端 policy 依据 Subject 现算，
// 请求里带过来的任何"我有什么权限"都无效。context.Subject 是装配层的同构类型，
// Task 8 负责把它映射进来；两者的字段集由 ports 的测试守护，防止漂移。
type Subject struct {
	UserID  string
	TeamIDs []string
	Roles   []string
}

// Purpose 是**服务端**选择的读取用途。它决定哪些来源会被调用。
//
// 它必须由装配层按路由决定，不能来自 URL 或 SIP：否则一次目录请求就能通过参数把自己
// 升级成"聚焦工作区"，从而对每一行服务扇出 logs/traces/access 查询。
type Purpose string

const (
	// PurposeDirectorySummary 是目录摘要刷新：只读生成服务摘要所需的 signal，
	// **绝不**扇出 logs/traces/access。
	PurposeDirectorySummary Purpose = "directory_summary"
	// PurposeOverview 是服务总览：读全部 signal binding，不做 focus 证据查询。
	PurposeOverview Purpose = "overview"
	// PurposeFocusedWorkspace 是聚焦工作区：允许按 Profile 与当前权限读证据来源。
	PurposeFocusedWorkspace Purpose = "focused_workspace"
)

// IsValid 判断是否是已知用途。零值不是合法用途：装配层必须显式选择。
func (p Purpose) IsValid() bool {
	switch p {
	case PurposeDirectorySummary, PurposeOverview, PurposeFocusedWorkspace:
		return true
	default:
		return false
	}
}

// NeedsFocusEvidence 表示该用途是否允许调用 logs/traces/access。
//
// 只有聚焦工作区会真的按 focus 查证据。目录与总览一律 false：目录有几百行，对每一行
// 扇出证据查询会把一次列表请求放大成上千次上游查询。
func (p Purpose) NeedsFocusEvidence() bool { return p == PurposeFocusedWorkspace }

// Permission 是细分读权限。metrics/logs/traces/faults 分离是设计交接稿 §14 的要求。
type Permission string

const (
	PermissionMetricsRead Permission = "metrics.read"
	PermissionLogsRead    Permission = "logs.read"
	PermissionTracesRead  Permission = "traces.read"
	PermissionFaultsRead  Permission = "faults.read"
)

// Permissions 是一次请求的有效权限集合。它由服务端 PermissionResolver 生成。
type Permissions struct {
	granted map[Permission]struct{}
}

// NewPermissions 构造权限集合。
func NewPermissions(granted ...Permission) Permissions {
	set := make(map[Permission]struct{}, len(granted))
	for _, permission := range granted {
		set[permission] = struct{}{}
	}
	return Permissions{granted: set}
}

// Allows 判断是否授予某权限。零值 Permissions 拒绝一切：默认从严。
func (p Permissions) Allows(permission Permission) bool {
	if p.granted == nil {
		return false
	}
	_, ok := p.granted[permission]
	return ok
}

// Granted 返回稳定排序的已授权列表，供审计与 permission snapshot 指纹使用。
func (p Permissions) Granted() []Permission {
	out := make([]Permission, 0, len(p.granted))
	for permission := range p.granted {
		out = append(out, permission)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PermissionResolver 由服务端实现：按 Subject 与目标服务现算权限。
//
// 契约：实现不得读取请求参数里的任何"自报权限"。Task 7 提供缺省矩阵实现。
type PermissionResolver interface {
	ResolvePermissions(ctx context.Context, subject Subject, serviceID string) (Permissions, error)
}

// SourceLimits 是单个来源的有效上界。每一项都取
// min(Profile limit, ToolDescriptor limit, permission limit, global limit)。
type SourceLimits struct {
	// Timeout 是该来源自己的 deadline。它不是整请求预算：一个慢来源不能吃掉别人的时间。
	Timeout time.Duration
	MaxRows int
	// MaxSeries 是 series 基数上界，防止把动态 focus 值当 label 造成基数爆炸。
	MaxSeries int
	// MaxResponseBytes 是响应体字节上界。
	MaxResponseBytes int64
}

// FocusResult 是 focus 查询的 typed 结果。
//
// 它把三件事分开：没请求过、查成功但没有匹配、查失败。第二个是 Empty（真实事实），
// 第三个是 Unknown（不知道）。把它们折叠成一个布尔值就会出现"查不到"被显示成"没有发生"。
type FocusResult string

const (
	FocusNotRequested FocusResult = "not_requested"
	FocusMatched      FocusResult = "matched"
	FocusEmpty        FocusResult = "empty"
	FocusUnknown      FocusResult = "unknown"
)

// JourneyStageObservation 是一个 journey stage 的观测事实。
//
// State 只使用 Page View v1 的枚举（ok/failed/pending/skipped/unknown）。内部刻意**不**引入
// 平行的 completed 状态：多一套词汇就一定会在投影时对不上契约。
//
// 本阶段不从"没看到事件"推导 skipped：那个枚举只为将来由受信 adapter 返回的显式、
// Profile 可验证的 not-applicable 事实保留。没有这种事实时一律 unknown，不假跳过。
type JourneyStageObservation struct {
	StageKey   string
	State      model.JourneyStageState
	ObservedAt time.Time
	// EvidenceIDs 必须引用当前 Context 里真实存在的 EvidenceRef。
	EvidenceIDs []string
	// ReasonCode 在 State 为 unknown 时必填，说明为什么不知道。
	ReasonCode string
	SourceKey  string
}

// ExternalIdentity 是 discovery/trace 返回的外部身份。
//
// Namespace 必须来自 SIP topology policy 的 alias namespace 白名单：不在白名单里的
// namespace 一律不解析，对应 hint 保持 Unknown。
type ExternalIdentity struct {
	Namespace string
	Value     string
}

// ObservedTopologyHint 是"在这个时间窗里真的看到了这条调用"。
//
// 它只是 hint：两端都唯一解析成 canonical 对象之后才可能进入 observed overlay，
// 任何一端未解析或冲突就不建 node/relation。方向固定 caller -> callee。
type ObservedTopologyHint struct {
	Source      ExternalIdentity
	Target      ExternalIdentity
	Kind        string
	ObservedAt  time.Time
	EvidenceIDs []string
	SourceKey   string
}

// ReconciliationIssue 是身份解析或边界的可审计缺口。
//
// 它刻意只带 namespace 与稳定 code，**不带**未脱敏的 identity 原文或 focus 值：
// 这些字段会进日志与审计，原文进去就等于绕过脱敏。
type ReconciliationIssue struct {
	ReasonCode string
	Namespace  string
	// Count 是同类问题的数量，用于"有 12 个 hint 无法解析"这种可读摘要。
	Count     int
	SourceKey string
}

// AliasState 是单个 external identity 的解析结果。
type AliasState string

const (
	AliasResolved AliasState = "resolved"
	AliasNotFound AliasState = "not_found"
	AliasConflict AliasState = "conflict"
)

// ObservedInstanceHint 表示本窗口看到某 component 的某 instance。
type ObservedInstanceHint struct {
	Component   ExternalIdentity
	Instance    ExternalIdentity
	ObservedAt  time.Time
	EvidenceIDs []string
	SourceKey   string
}

// AliasResolvedEntry 是已解析 alias 的 canonical entity 投影。
type AliasResolvedEntry struct {
	Identity    ExternalIdentity
	EntityID    string
	EntityKey   string
	Name        string
	Layer       model.EntityLayer
	Kind        string
	Environment string
}

// AliasEntry 是一条解析结果。EntityID 只在 State=resolved 时有意义。
type AliasEntry struct {
	Identity ExternalIdentity
	State    AliasState
	EntityID string
	// Resolved 在 State=resolved 时携带 canonical entity metadata。
	Resolved *AliasResolvedEntry
}

// AliasResolution 是一批解析的结果。
type AliasResolution struct {
	// Entries 按 namespace+value 稳定排序，便于审计与测试。
	Entries []AliasEntry
	Issues  []ReconciliationIssue
	ReadAt  time.Time
}

// AliasResolver 批量解析 external identity。
//
// 必须批量：一次 trace 可能带上百个 span，逐个查 alias 就是 N+1。实现应在单个只读事务里
// 完成，并对 0/1/2+ 命中分别给出 not_found/resolved/conflict。
type AliasResolver interface {
	ResolveAliases(ctx context.Context, identities []ExternalIdentity) (AliasResolution, error)
}

// SignalRequest 是语义指标读取请求。它只携带语义 key 与范围，绝不携带 PromQL 或凭据。
type SignalRequest struct {
	Subject     Subject
	Purpose     Purpose
	Profile     model.Profile
	ServiceID   string
	Environment string
	// BindingKeys 是本次真正需要的 binding。目录摘要只传摘要用的那几个，
	// 而不是把整份 Profile 的 binding 都查一遍。
	BindingKeys   []string
	WindowSeconds int
	From          time.Time
	To            time.Time
	Limits        SourceLimits
	// Bindings 是 Coordinator 从同一 compiled snapshot 选中的 server-only bindings。
	Bindings []integration.CompiledBinding
}

// SignalFacts 是语义指标来源的返回。
type SignalFacts struct {
	Signals []health.SignalObservation
	Source  health.SourceObservation
	ReadAt  time.Time
}

// SemanticSignalReader 读取 Golden/RED/USE 的规范化数值。
type SemanticSignalReader interface {
	ReadSignals(ctx context.Context, req SignalRequest) (SignalFacts, error)
}

// FocusRequest 是按 focus 读取证据/观测的请求。
type FocusRequest struct {
	Subject     Subject
	Purpose     Purpose
	Profile     model.Profile
	ServiceID   string
	Environment string
	From        time.Time
	To          time.Time
	// Focus 为 nil 表示没有聚焦：实现必须返回 FocusNotRequested，不得自行放宽成全量扫描。
	Focus  *model.Focus
	Limits SourceLimits
	// Bindings 是 Coordinator 从同一 compiled snapshot 选中的 server-only bindings。
	Bindings []integration.CompiledBinding
}

// TraceFacts 是 trace 来源的返回：journey 观测、observed topology hint 与 trace 证据。
type TraceFacts struct {
	Focus         FocusResult
	JourneyStages []JourneyStageObservation
	TopologyHints []ObservedTopologyHint
	EvidenceRefs  []model.EvidenceRef
	Issues        []ReconciliationIssue
	Source        health.SourceObservation
	ReadAt        time.Time
}

// LogFacts 是日志来源的返回。它只含脱敏摘要与受控 locator，绝不含正文。
type LogFacts struct {
	Focus        FocusResult
	EvidenceRefs []model.EvidenceRef
	Source       health.SourceObservation
	ReadAt       time.Time
}

// AccessFacts 是 access 记录来源的返回，同样只含摘要。
type AccessFacts struct {
	Focus        FocusResult
	EvidenceRefs []model.EvidenceRef
	Source       health.SourceObservation
	ReadAt       time.Time
}

// TraceReader / LogReader / AccessReader 是三个独立端口。
//
// 分开的理由与 source key 一致：权限、limits、失败模式都不同，合成一个接口会让"只有日志
// 被 Forbidden"这件事无法表达。
type TraceReader interface {
	ReadTraces(ctx context.Context, req FocusRequest) (TraceFacts, error)
}

type LogReader interface {
	ReadLogs(ctx context.Context, req FocusRequest) (LogFacts, error)
}

type AccessReader interface {
	ReadAccess(ctx context.Context, req FocusRequest) (AccessFacts, error)
}

// ToolInvocation 是一次工具调用的审计开头。
type ToolInvocation struct {
	RequestID string
	// ActorID 是真实用户 ID；后台目录刷新使用固定 system actor。
	ActorID string
	// ActorKind 区分 user 与 system，让"谁触发的"在审计里可判。
	ActorKind  string
	ServiceID  string
	ProfileKey string
	ToolKey    string
	SourceKey  string
	StartedAt  time.Time
}

// ToolInvocationResult 是审计收尾。
type ToolInvocationResult struct {
	FinishedAt time.Time
	// Status 是 succeeded / failed / forbidden / timeout 之一（稳定机器码）。
	Status      string
	ReasonCode  string
	Rows        int
	Cardinality int
}

// ToolInvocationAuditor 记录每次工具调用。
//
// 事务边界由实现负责，但契约在这里就写清：Begin 必须先提交才能调用 provider；
// provider 调用完全位于数据库事务之外；Complete 在独立事务里提交。Begin 失败一律不调用
// provider；Complete 失败必须丢弃 provider 结果并返回 audit_unavailable——没有审计的结果
// 不得进入 Page View。
type ToolInvocationAuditor interface {
	Begin(ctx context.Context, invocation ToolInvocation) (string, error)
	Complete(ctx context.Context, auditID string, result ToolInvocationResult) error
}

// EvidenceGrantRequest 是为一条 EvidenceRef 申请 broker deep link 的请求。
type EvidenceGrantRequest struct {
	EvidenceID string
	SourceKey  string
	// RequiredPermission 是打开这条证据所需的权限。broker 打开时必须**重新**检查它，
	// 不能用创建时的快照替代实时鉴权。
	RequiredPermission Permission
	ExpiresAt          time.Time
}

// EvidenceGrant 是创建成功的授权，只含 opaque token 与到期时间。
type EvidenceGrant struct {
	EvidenceID string
	// Token 是 opaque 的：浏览器拿不到内部 locator。
	Token     string
	ExpiresAt time.Time
}

// EvidenceBroker 批量创建 evidence grant。
//
// 契约：只在最终可投影的 EvidenceRef 确定之后批量创建；单视图数量有硬上限；批量写失败
// 只把对应 EvidenceRef 降级为 Unavailable 并移除 broker link，不影响 signals 与 domain。
type EvidenceBroker interface {
	GrantEvidence(ctx context.Context, requests []EvidenceGrantRequest) ([]EvidenceGrant, error)
}

// SortIssues 把 reconciliation issue 按稳定顺序排列，便于确定性投影与测试断言。
func SortIssues(issues []ReconciliationIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].SourceKey != issues[j].SourceKey {
			return issues[i].SourceKey < issues[j].SourceKey
		}
		if issues[i].ReasonCode != issues[j].ReasonCode {
			return issues[i].ReasonCode < issues[j].ReasonCode
		}
		return issues[i].Namespace < issues[j].Namespace
	})
}
