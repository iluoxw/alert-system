// Package facade 是三个只读聚合 API 背后的唯一编排入口。
//
// # 为什么它是 internal/serviceobs 的子包而不是 internal/serviceobs/service.go
//
// 依赖方向的硬约束是 `api -> facade/view/context -> ports`。ports 已经落在
// `package serviceobs`（internal/serviceobs/ports.go），而 context/view/cache/adapters
// 全部 import 它。编排层必须 import context（装配事实）、view（投影页面）与 cache
// （目录摘要），所以把它放进 `package serviceobs` 会形成真实的 import 环
// （serviceobs -> view -> serviceobs）。这里因此只做包边界上的最小偏移：facade 是
// serviceobs 下的一个消费者包，仍然严格遵守 `api -> facade -> view/context -> ports`，
// 对外可观测的契约（三个 API、状态码、reason code、Page View 形状）一个字节都不变。
//
// # 职责边界
//
// facade 负责编排，不负责判定：
//
//   - Health/Data 只由 health 包的同一个 evaluator 算出（经 context 装配）；
//   - 显示投影只由 view 包完成；
//   - 目录只读 cache 快照，绝不在请求内对每一行同步扇出所有 source；
//   - 目录摘要的分母是 metadata 过滤后的**完整**候选集合，health/data_status 只筛
//     结果，不改分母；
//   - 每个候选的缓存条目在一次请求内**恰好读一次**，摘要与结果筛选共用这一份快照，
//     因此不会触发第二轮刷新。
//
// HTTP 语义（404/409/422/503）由 API 层按本包返回的 typed 错误映射，本包不认识 HTTP。
package facade

import (
	"context"
	"errors"
	"sort"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/cache"
	soctx "alert-system/internal/serviceobs/context"
	"alert-system/internal/serviceobs/model"
	"alert-system/internal/serviceobs/view"
)

// 页面与窗口的默认值/上界。它们是产品口径，不是随手选的数字：目录默认 50 行、硬上限
// 200 行来自设计稿与契约的 items maxItems。
const (
	DefaultPageLimit     = 50
	MaxPageLimit         = 200
	DefaultWindowSeconds = 300
)

var (
	// ErrCacheUnavailable 表示装配缺少目录摘要缓存。它是装配缺陷：没有缓存的目录
	// 会退化成逐行扇出，而那正是设计明确禁止的。
	ErrCacheUnavailable = errors.New("serviceobs/facade: 未注入目录摘要缓存")
	// ErrCandidateScannerRequired 表示装配缺少候选扫描端口。
	ErrCandidateScannerRequired = errors.New("serviceobs/facade: 未注入候选扫描端口")
	// ErrAssemblerRequired 表示装配缺少 assembler。
	ErrAssemblerRequired = errors.New("serviceobs/facade: 未注入 assembler")
)

// SummaryCache 是目录摘要缓存的只读视图。*cache.Coordinator 直接满足它。
//
// 抽成接口让 facade 的分页与摘要口径可以用 fake 精确断言"每个候选恰好读一次缓存"，
// 而不需要起后台 worker。
type SummaryCache interface {
	// Lookup 返回摘要及其新鲜度状态，并按需入 bounded refresh queue。
	Lookup(key cache.Key, placeholder cache.Summary) cache.Result
}

// Limits 是 facade 的边界参数。零值全部退回上面的默认常量。
type Limits struct {
	DefaultPageLimit int
	MaxPageLimit     int
	// MaxCandidates 是完整候选集合的安全上界。超过它必须显式失败，不返回一个被截断
	// 的摘要分母。
	MaxCandidates int
	// CandidateBatchSize 是完整候选扫描的单批大小。
	CandidateBatchSize int
	// DefaultWindowSeconds 是没有 Profile 默认窗口时的兜底窗口。
	DefaultWindowSeconds int
	// MaxWindowSeconds 是全局窗口上界，与 assembler 的 Limits 保持一致。
	MaxWindowSeconds int
}

func (l Limits) withDefaults() Limits {
	if l.DefaultPageLimit <= 0 {
		l.DefaultPageLimit = DefaultPageLimit
	}
	if l.MaxPageLimit <= 0 || l.MaxPageLimit > MaxPageLimit {
		l.MaxPageLimit = MaxPageLimit
	}
	if l.DefaultPageLimit > l.MaxPageLimit {
		l.DefaultPageLimit = l.MaxPageLimit
	}
	if l.MaxCandidates <= 0 {
		l.MaxCandidates = serviceobs.DefaultMaxDirectoryCandidates
	}
	if l.CandidateBatchSize <= 0 {
		l.CandidateBatchSize = serviceobs.DefaultCandidateBatchSize
	}
	if l.DefaultWindowSeconds <= 0 {
		l.DefaultWindowSeconds = DefaultWindowSeconds
	}
	return l
}

// Options 是 Service 的构造依赖。全部显式注入：本包不读环境变量、不碰数据库。
type Options struct {
	Assembler  *soctx.Assembler
	Candidates serviceobs.CandidateScanner
	Profiles   soctx.ProfileSource
	// Summaries 可以在构造后用 SetSummaryCache 注入：cache 的 refresh 函数由本
	// Service 提供，两者互为依赖，只能有一处后置赋值。
	Summaries SummaryCache
	Limits    Limits
	// Clock 注入当前时间。它只用于 assembled_at 与窗口推导，绝不用来填 observed_at。
	Clock func() time.Time
	// NewRequestID 为 nil 时使用 context 包的 crypto/rand 实现。
	NewRequestID func() (string, error)
}

// Service 是三个 Page View 的服务端编排入口。
type Service struct {
	assembler    *soctx.Assembler
	candidates   serviceobs.CandidateScanner
	profiles     soctx.ProfileSource
	summaries    SummaryCache
	limits       Limits
	clock        func() time.Time
	newRequestID func() (string, error)
}

// New 构造 facade。
func New(opts Options) (*Service, error) {
	if opts.Assembler == nil {
		return nil, ErrAssemblerRequired
	}
	if opts.Candidates == nil {
		return nil, ErrCandidateScannerRequired
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	newRequestID := opts.NewRequestID
	if newRequestID == nil {
		newRequestID = soctx.NewRequestID
	}
	return &Service{
		assembler:    opts.Assembler,
		candidates:   opts.Candidates,
		profiles:     opts.Profiles,
		summaries:    opts.Summaries,
		limits:       opts.Limits.withDefaults(),
		clock:        clock,
		newRequestID: newRequestID,
	}, nil
}

// SetSummaryCache 注入目录摘要缓存。
//
// 它必须在开始服务请求之前调用恰好一次：cache 的 RefreshSummary 由本 Service 提供，
// 而 Service 的目录路径又要读 cache，装配时只能有一处后置赋值来打破这个环。
func (s *Service) SetSummaryCache(summaries SummaryCache) { s.summaries = summaries }

// DirectoryQuery 是目录查询。
//
// 全部字段都必须由 API 层按白名单校验后填入：本包不解析 HTTP，也不接受自由文本排序
// 字段。Health/DataStatus 是结果筛选，不参与摘要分母，也不下推到候选查询。
type DirectoryQuery struct {
	Subject     soctx.Subject
	Environment string
	TeamID      string
	Tier        string
	Query       string
	Health      []model.Health
	DataStatus  []model.DataState
	Sort        view.DirectorySort
	Cursor      string
	Limit       int
}

// OverviewQuery 是服务总览查询。
type OverviewQuery struct {
	Subject   soctx.Subject
	ServiceID string
	// Environment 为空表示沿用 registration 的环境；显式不匹配按服务不存在处理。
	Environment string
	// WindowSeconds <= 0 表示使用 Profile 默认窗口（无 Profile 时用兜底窗口）。
	WindowSeconds int
}

// WorkspaceQuery 是聚焦工作区查询。
type WorkspaceQuery struct {
	Subject     soctx.Subject
	ServiceID   string
	Environment string
	Focus       *model.Focus
	// From/To 必须成对出现且是绝对 UTC；两者都为零时按 Profile 默认窗口生成。
	From     time.Time
	To       time.Time
	Timezone string
}

// Directory 生成 `/services` 目录视图。
//
// 顺序即口径：
//
//  1. 先解码并校验游标（版本 + 查询指纹）——任何数据库查询之前；
//  2. 扫描 metadata 过滤后的**完整**候选集合（有界，超界显式失败）；
//  3. 每个候选**恰好读一次**缓存，形成一份 request-local 快照；
//  4. 摘要用这份快照统计完整候选集合；
//  5. health/data_status 在同一份快照上筛结果，不触发第二次读取；
//  6. 排序 + 游标定位 + 取一页，投影时使用同一个排序口径。
func (s *Service) Directory(ctx context.Context, query DirectoryQuery) (model.DirectoryView, error) {
	if s.summaries == nil {
		return model.DirectoryView{}, ErrCacheUnavailable
	}
	limit := query.Limit
	if limit <= 0 {
		limit = s.limits.DefaultPageLimit
	}
	if limit > s.limits.MaxPageLimit {
		limit = s.limits.MaxPageLimit
	}

	fingerprint := queryFingerprint(query)
	var cursorKey view.DirectoryKey
	hasCursor := query.Cursor != ""
	if hasCursor {
		decoded, err := decodeCursor(query.Cursor, fingerprint)
		if err != nil {
			return model.DirectoryView{}, err
		}
		cursorKey = decoded
	}

	requestID, err := s.newRequestID()
	if err != nil {
		return model.DirectoryView{}, err
	}
	now := s.clock().UTC()

	candidates, err := s.candidates.ScanCandidates(ctx, serviceobs.CandidateScanRequest{
		Filter: serviceobs.CandidateFilter{
			Environment: query.Environment,
			TeamID:      query.TeamID,
			Tier:        query.Tier,
			Query:       query.Query,
		},
		BatchSize:     s.limits.CandidateBatchSize,
		MaxCandidates: s.limits.MaxCandidates,
	})
	if err != nil {
		return model.DirectoryView{}, err
	}

	// 一次请求、一份快照：每个候选恰好一次 Lookup。摘要与结果筛选都从这里取，
	// 因此不会出现"摘要读一遍、筛选再读一遍"导致的额外刷新或前后不一致。
	rows := make([]view.DirectoryRow, 0, len(candidates.Items))
	for _, identity := range candidates.Items {
		rows = append(rows, s.directoryRow(identity, now))
	}

	// 摘要覆盖完整候选集合。health/data_status 之后才筛，分母不受它们影响。
	summary := view.SummaryOf(rows)

	filtered := make([]view.DirectoryRow, 0, len(rows))
	for _, row := range rows {
		if !matchesHealth(row, query.Health) || !matchesDataStatus(row, query.DataStatus) {
			continue
		}
		filtered = append(filtered, row)
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		return view.DirectoryKeyLess(
			view.DirectoryKeyOfRow(filtered[i]), view.DirectoryKeyOfRow(filtered[j]), query.Sort)
	})

	if hasCursor {
		// 只比较元组 + service ID，不要求游标那一行仍然存在：它在两页之间被删掉时，
		// 同一个比较函数仍然给出同一个插入位置。
		start := len(filtered)
		for i, row := range filtered {
			if afterCursor(cursorKey, view.DirectoryKeyOfRow(row), query.Sort) {
				start = i
				break
			}
		}
		filtered = filtered[start:]
	}

	var nextCursor *string
	page := filtered
	if len(page) > limit {
		page = page[:limit]
		encoded, err := encodeCursor(view.DirectoryKeyOfRow(page[len(page)-1]), fingerprint)
		if err != nil {
			return model.DirectoryView{}, err
		}
		nextCursor = &encoded
	}

	return view.ProjectDirectory(view.DirectoryInput{
		RequestID:   requestID,
		AssembledAt: now,
		Rows:        page,
		Summary:     summary,
		Sort:        query.Sort,
		NextCursor:  nextCursor,
	}), nil
}

// directoryRow 读一个候选的缓存摘要并投影成目录行。
//
// cache miss 立即返回 Unknown/Unconfigured 占位并入 bounded refresh queue：目录请求
// 内绝不对每一行同步扇出所有 source。占位与过期摘要的 observed_at 都是真实事实时间，
// 不是当前时间。
func (s *Service) directoryRow(identity serviceobs.ServiceIdentity, now time.Time) view.DirectoryRow {
	published, hasProfile := s.publishedProfile(identity.ServiceID)
	anchor := identity.FactAnchor()

	key := cache.Key{
		ServiceID:            identity.ServiceID,
		Environment:          identity.Environment,
		DefaultWindowSeconds: s.limits.DefaultWindowSeconds,
	}
	placeholder := cache.UnconfiguredPlaceholder(anchor, now)
	var profileRef *model.ProfileRef
	if hasProfile {
		key.ProfileVersion = published.ProfileVersion
		key.DefaultWindowSeconds = s.windowFor(&published)
		placeholder = cache.UnknownPlaceholder(anchor, now)
		profileRef = view.ProjectProfileRef(&published)
	}

	result := s.summaries.Lookup(key, placeholder)
	row := view.DirectoryRow{
		Identity:      identity,
		Profile:       profileRef,
		Health:        result.Summary.Health,
		DataStatus:    result.Summary.Data,
		GoldenSignals: result.Summary.GoldenSignals,
		DomainCounts:  result.Summary.DomainCounts,
		ObservedAt:    result.Summary.ObservedAt,
		Links:         view.LinksForIdentity(identity),
	}
	if result.Stale {
		// 过期快照必须显式 Stale 并显示 observed_at。未配置 Profile 是配置状态，
		// 优先级高于新鲜度：它不会因为快照过期就变成 stale。
		if row.DataStatus.State != model.DataStateUnconfigured {
			row.DataStatus.State = model.DataStateStale
		}
	}
	return row
}

// Overview 生成 `/services/:service_id` 总览。
//
// Profile 缺失时它仍然可用：identity、责任、declared 依赖与领域状态保留，profile 为
// null、Data 为 Unconfigured。单个 section 失败也只降级那个 section。
func (s *Service) Overview(ctx context.Context, query OverviewQuery) (model.OverviewView, error) {
	from, to := s.explicitWindow(query.WindowSeconds)
	snapshot, err := s.assembler.Assemble(ctx, soctx.Request{
		Kind:        soctx.KindOverview,
		ServiceID:   query.ServiceID,
		Subject:     query.Subject,
		Environment: query.Environment,
		From:        from,
		To:          to,
	})
	if err != nil {
		return model.OverviewView{}, err
	}
	return view.ProjectOverview(snapshot), nil
}

// Workspace 生成 `/services/:service_id/observability` 工作区。
//
// Profile 未配置时返回 serviceobs.ErrProfileUnconfigured（API 层 409）：不猜指标，
// 也不猜布局。
func (s *Service) Workspace(ctx context.Context, query WorkspaceQuery) (model.WorkspaceView, error) {
	snapshot, err := s.assembler.Assemble(ctx, soctx.Request{
		Kind:        soctx.KindWorkspace,
		ServiceID:   query.ServiceID,
		Subject:     query.Subject,
		Environment: query.Environment,
		From:        query.From,
		To:          query.To,
		Timezone:    query.Timezone,
		Focus:       query.Focus,
	})
	if err != nil {
		return model.WorkspaceView{}, err
	}
	return view.ProjectWorkspace(snapshot), nil
}

// RefreshSummary 重新计算一个键的目录摘要。它就是 cache.Coordinator 的 RefreshFunc。
//
// 它走的是与总览完全相同的装配 + evaluator 路径，因此目录与总览不可能对同一个服务
// 给出不同的 Health/Data。摘要只保留显示事实：Health、Data、四张 Golden 卡、领域计数
// 与真实 observed_at——cache 的 value 类型本身就装不下原始日志/trace/证据。
func (s *Service) RefreshSummary(ctx context.Context, key cache.Key) (cache.Summary, error) {
	from, to := s.explicitWindow(key.DefaultWindowSeconds)
	snapshot, err := s.assembler.Assemble(ctx, soctx.Request{
		Kind:        soctx.KindOverview,
		ServiceID:   key.ServiceID,
		Environment: key.Environment,
		From:        from,
		To:          to,
	})
	if err != nil {
		return cache.Summary{}, err
	}
	return cache.Summary{
		Health:        view.ProjectHealth(snapshot.Evaluation),
		Data:          view.ProjectDataStatus(snapshot.Evaluation),
		GoldenSignals: view.GoldenSignals(snapshot),
		DomainCounts:  view.ProjectDomainCounts(snapshot),
		// 摘要的 observed_at 是已提交事实里最新的真实时间，绝不是刷新时刻。
		ObservedAt: soctx.ObservedAtOr(time.Time{}, snapshot.FactAnchor),
	}, nil
}

// explicitWindow 把窗口秒数换算成一对绝对 UTC 时间。
//
// windowSeconds <= 0 时返回两个零值，让 assembler 用 Profile 的默认窗口（它只取一次
// now，保证 from/to 一致）。这里绝不自己发明 Profile 默认值。
func (s *Service) explicitWindow(windowSeconds int) (time.Time, time.Time) {
	if windowSeconds <= 0 {
		return time.Time{}, time.Time{}
	}
	to := s.clock().UTC().Truncate(time.Second)
	return to.Add(-time.Duration(windowSeconds) * time.Second), to
}

// windowFor 返回该 Profile 的默认聚合窗口，它是 cache key 的一个维度。
func (s *Service) windowFor(published *model.Profile) int {
	if published != nil && published.Limits.DefaultWindowSeconds > 0 {
		return published.Limits.DefaultWindowSeconds
	}
	return s.limits.DefaultWindowSeconds
}

func (s *Service) publishedProfile(serviceID string) (model.Profile, bool) {
	if s.profiles == nil {
		return model.Profile{}, false
	}
	return s.profiles.PublishedForService(serviceID)
}

// matchesHealth / matchesDataStatus 是结果筛选。空筛选表示不筛，绝不因此改变摘要分母。
func matchesHealth(row view.DirectoryRow, wanted []model.Health) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, status := range wanted {
		if row.Health.Status == status {
			return true
		}
	}
	return false
}

func matchesDataStatus(row view.DirectoryRow, wanted []model.DataState) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, state := range wanted {
		if row.DataStatus.State == state {
			return true
		}
	}
	return false
}

func normalizedHealthFilter(values []model.Health) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	sort.Strings(out)
	return out
}

func normalizedDataFilter(values []model.DataState) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	sort.Strings(out)
	return out
}
