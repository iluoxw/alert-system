package view

import (
	"sort"
	"time"

	"alert-system/internal/serviceobs"
	"alert-system/internal/serviceobs/model"
)

// DirectoryRow 是目录一行的投影输入。
//
// 它刻意与 snapshot cache 的摘要形状一致：目录读缓存，不在请求内对每一行同步扇出全部
// source。Health/Data 已由服务端 evaluator 算好，投影层不重算。
type DirectoryRow struct {
	Identity      serviceobs.ServiceIdentity
	Profile       *model.ProfileRef
	Health        model.HealthSummary
	DataStatus    model.DataStatus
	GoldenSignals []model.SignalValue
	DomainCounts  model.DomainCounts
	// ObservedAt 必须是真实观测/事实时间。缓存占位行也要用 registration/domain 里最新的
	// 真实时间，绝不能用当前时间伪装成观测。
	ObservedAt time.Time
	Links      []model.Link
}

// DirectoryInput 是目录投影的完整输入。
type DirectoryInput struct {
	RequestID   string
	AssembledAt time.Time
	Rows        []DirectoryRow
	// Summary 统计的是 metadata 过滤后的完整候选集合，不是当前分页，因此必须由上层
	// 传入而不是从 Rows 反推。
	Summary model.DirectorySummary
	// Sort 是本页实际使用的排序口径。零值表示默认分诊排序。
	//
	// 它必须与上层分页时用的口径完全一致：projector 会按它排序，因此绝不会把一个
	// 按用户选择排好的页面又静默按分诊顺序重排——那样 UI 的列头排序会失效，
	// cursor 位置也会与展示顺序错位。
	Sort DirectorySort
	// NextCursor 为 nil 表示没有下一页；空字符串不是合法的"无下一页"表达。
	NextCursor *string
}

// maxDirectoryItems 与契约 directoryView.items 的 maxItems 一致。
const maxDirectoryItems = 200

// maxDirectoryLinks 与契约 directoryItem.links 的 maxItems 一致。
const maxDirectoryLinks = 5

// DirectorySortKey 是目录允许的排序字段。它精确对应既有 UI 的可排序列头，
// 不接受任意字段——排序字段是白名单，不是自由文本。
type DirectorySortKey string

const (
	// SortKeyDefault 是未指定排序时的分诊顺序。
	SortKeyDefault DirectorySortKey = ""
	SortKeyService DirectorySortKey = "service"
	SortKeyOwner   DirectorySortKey = "owner"
	SortKeyTier    DirectorySortKey = "tier"
	SortKeyHealth  DirectorySortKey = "health"
	SortKeyFault   DirectorySortKey = "fault"
	SortKeyData    DirectorySortKey = "data"
	// SortKeyObserved 按最近观测时间排序。
	SortKeyObserved DirectorySortKey = "observed"
)

// DirectorySortKeys 返回全部合法排序字段，供 API 层构造白名单与测试遍历。
func DirectorySortKeys() []DirectorySortKey {
	return []DirectorySortKey{
		SortKeyService, SortKeyOwner, SortKeyTier, SortKeyHealth,
		SortKeyFault, SortKeyData, SortKeyObserved,
	}
}

// IsDirectorySortKey 判断一个字符串是否是合法排序字段。
func IsDirectorySortKey(value string) bool {
	for _, key := range DirectorySortKeys() {
		if string(key) == value {
			return true
		}
	}
	return false
}

// DirectorySort 是一次排序口径。Key 为空时忽略 Descending，使用固定的分诊顺序。
type DirectorySort struct {
	Key        DirectorySortKey
	Descending bool
}

// DirectoryKey 是一行的稳定排序元组。
//
// 它同时服务三件事，因此必须是一个显式类型而不是散落在比较函数里的字段读取：
//
//  1. 排序：DirectoryKeyLess 是目录唯一的比较函数；
//  2. 分页：cursor 携带这个元组（外加 service ID），恢复时用同一个比较函数判断
//     "谁在游标之后"，因此游标行即使在两页之间被删掉，也仍然能按插入位置继续；
//  3. 断言：测试可以直接比较元组，而不是间接比较渲染结果。
//
// 所有字段都取自已投影的显示事实，不重新读任何数据。
type DirectoryKey struct {
	// HealthRank 是分诊严重度序位（Critical 最先）。
	HealthRank int
	// ConfirmedImpacts / ActiveFaults 在默认排序里是降序维度。
	ConfirmedImpacts int
	ActiveFaults     int
	Tier             string
	Name             string
	OwnerTeamName    string
	// DataRank 是数据状态序位（Complete 最先）。
	DataRank int
	// ObservedAtUnixMilli 是真实观测时间。零值表示没有观测。
	ObservedAtUnixMilli int64
	// ServiceID 是最终 tiebreaker，保证比较函数是全序。
	ServiceID string
}

// DirectoryKeyOfRow 从投影输入取出排序元组。
//
// 分页发生在投影之前（上层要先定位游标、再切页），但排序元组必须与投影后的元组逐字节
// 一致，否则"游标位置"和"展示顺序"会错位。因此这里刻意走一次 Timestamp 往返：
// observed_at 在契约里是秒级 RFC3339，直接读 time.Time 的毫秒会带上被投影丢弃的
// 亚秒部分。
func DirectoryKeyOfRow(row DirectoryRow) DirectoryKey {
	return DirectoryKeyOf(model.DirectoryItem{
		Service:      ProjectIdentity(row.Identity),
		Health:       row.Health,
		DataStatus:   row.DataStatus,
		DomainCounts: row.DomainCounts,
		ObservedAt:   model.NewTimestamp(row.ObservedAt),
	})
}

// DirectoryKeyOf 从一个已投影的目录行取出排序元组。
func DirectoryKeyOf(item model.DirectoryItem) DirectoryKey {
	key := DirectoryKey{
		HealthRank:       healthRank(item.Health.Status),
		ConfirmedImpacts: item.DomainCounts.ConfirmedImpacts,
		ActiveFaults:     item.DomainCounts.ActiveFaults,
		Tier:             string(item.Service.Tier),
		Name:             item.Service.Name,
		OwnerTeamName:    item.Service.OwnerTeamName,
		DataRank:         dataRank(item.DataStatus.State),
		ServiceID:        item.Service.ServiceID,
	}
	if observedAt, err := item.ObservedAt.Time(); err == nil {
		key.ObservedAtUnixMilli = observedAt.UnixMilli()
	}
	return key
}

// DirectoryKeyLess 是目录唯一的比较函数。
//
// 默认（Key 为空）是分诊顺序：Health 严重度、confirmed Impact、active Fault、tier、
// 名称、service ID。显式排序则以选定字段为主键，再回落到名称与 service ID——两级
// tiebreaker 保证顺序是全序且稳定，翻页不会因为并列行的相对顺序漂移而重排。
func DirectoryKeyLess(left, right DirectoryKey, spec DirectorySort) bool {
	if spec.Key == SortKeyDefault {
		return triageLess(left, right)
	}
	if primary := comparePrimary(left, right, spec.Key); primary != 0 {
		if spec.Descending {
			return primary > 0
		}
		return primary < 0
	}
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	return left.ServiceID < right.ServiceID
}

func triageLess(left, right DirectoryKey) bool {
	if left.HealthRank != right.HealthRank {
		return left.HealthRank < right.HealthRank
	}
	if left.ConfirmedImpacts != right.ConfirmedImpacts {
		return left.ConfirmedImpacts > right.ConfirmedImpacts
	}
	if left.ActiveFaults != right.ActiveFaults {
		return left.ActiveFaults > right.ActiveFaults
	}
	if left.Tier != right.Tier {
		return left.Tier < right.Tier
	}
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	return left.ServiceID < right.ServiceID
}

// comparePrimary 返回 -1/0/1。它只比较选定字段本身，方向由调用方施加。
func comparePrimary(left, right DirectoryKey, key DirectorySortKey) int {
	switch key {
	case SortKeyService:
		return compareStrings(left.Name, right.Name)
	case SortKeyOwner:
		return compareStrings(left.OwnerTeamName, right.OwnerTeamName)
	case SortKeyTier:
		return compareStrings(left.Tier, right.Tier)
	case SortKeyHealth:
		return compareInts(left.HealthRank, right.HealthRank)
	case SortKeyFault:
		if faults := compareInts(left.ActiveFaults, right.ActiveFaults); faults != 0 {
			return faults
		}
		return compareInts(left.ConfirmedImpacts, right.ConfirmedImpacts)
	case SortKeyData:
		return compareInts(left.DataRank, right.DataRank)
	case SortKeyObserved:
		return compareInt64s(left.ObservedAtUnixMilli, right.ObservedAtUnixMilli)
	default:
		return 0
	}
}

func compareStrings(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareInt64s(left, right int64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// ProjectDirectory 生成 `/services` 目录视图。
//
// 排序口径由 input.Sort 决定：零值是设计稿的分诊顺序（Health 严重度、confirmed
// Impact、active Fault、tier、名称、service ID），显式排序则按用户选定列。它是完全
// 确定的：同一份输入两次投影结果逐字节一致。
func ProjectDirectory(input DirectoryInput) model.DirectoryView {
	items := make([]model.DirectoryItem, 0, len(input.Rows))
	for _, row := range input.Rows {
		item := model.DirectoryItem{
			Service:       ProjectIdentity(row.Identity),
			Profile:       row.Profile,
			Health:        row.Health,
			DataStatus:    row.DataStatus,
			GoldenSignals: capSlice(row.GoldenSignals, 4),
			DomainCounts:  row.DomainCounts,
			ObservedAt:    model.NewTimestamp(row.ObservedAt),
		}
		if len(row.Links) > 0 {
			item.Links = capSlice(row.Links, maxDirectoryLinks)
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return DirectoryKeyLess(DirectoryKeyOf(items[i]), DirectoryKeyOf(items[j]), input.Sort)
	})

	view := model.DirectoryView{
		SchemaVersion: model.SchemaVersion,
		ViewKind:      model.ViewKindServiceDirectory,
		RequestID:     input.RequestID,
		AssembledAt:   model.NewTimestamp(input.AssembledAt),
		Summary:       input.Summary,
		Items:         capSlice(items, maxDirectoryItems),
		NextCursor:    input.NextCursor,
	}
	view.Normalize()
	return view
}

// healthRank 决定分诊顺序：Critical 最先，Healthy 最后。
//
// Unknown 排在 Degraded 之后、Healthy 之前：缺数不是健康，但也不能压过真实的
// Degraded 信号。
func healthRank(status model.Health) int {
	switch status {
	case model.HealthCritical:
		return 0
	case model.HealthDegraded:
		return 1
	case model.HealthUnknown:
		return 2
	default:
		return 3
	}
}

// dataRank 决定数据状态排序：覆盖最完整的在前。
func dataRank(state model.DataState) int {
	switch state {
	case model.DataStateComplete:
		return 0
	case model.DataStatePartial:
		return 1
	case model.DataStateStale:
		return 2
	default:
		return 3
	}
}

// SummaryOf 依据一批行统计目录摘要。
//
// 它只适用于"这批行就是完整候选集合"的场景。分页场景必须由上层按 metadata 过滤后的
// 完整候选集合统计，否则摘要分母会随翻页漂移。
func SummaryOf(rows []DirectoryRow) model.DirectorySummary {
	summary := model.DirectorySummary{Managed: len(rows)}
	for _, row := range rows {
		if row.Health.Status == model.HealthCritical {
			summary.Critical++
		}
		switch row.DataStatus.State {
		case model.DataStatePartial:
			summary.Partial++
		case model.DataStateUnconfigured:
			summary.Unconfigured++
		}
	}
	return summary
}
