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
	// NextCursor 为 nil 表示没有下一页；空字符串不是合法的"无下一页"表达。
	NextCursor *string
}

// maxDirectoryItems 与契约 directoryView.items 的 maxItems 一致。
const maxDirectoryItems = 200

// maxDirectoryLinks 与契约 directoryItem.links 的 maxItems 一致。
const maxDirectoryLinks = 5

// ProjectDirectory 生成 `/services` 目录视图。
//
// 排序口径与设计稿一致：Health 严重度、confirmed Impact、active Fault、tier、名称、
// service ID。它是完全确定的：同一份输入两次投影结果逐字节一致。
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
	sort.SliceStable(items, func(i, j int) bool { return directoryLess(items[i], items[j]) })

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

// directoryLess 是默认分诊排序。它只用已投影的显示事实，不重新读任何数据。
func directoryLess(left, right model.DirectoryItem) bool {
	if rank := healthRank(left.Health.Status) - healthRank(right.Health.Status); rank != 0 {
		return rank < 0
	}
	if left.DomainCounts.ConfirmedImpacts != right.DomainCounts.ConfirmedImpacts {
		return left.DomainCounts.ConfirmedImpacts > right.DomainCounts.ConfirmedImpacts
	}
	if left.DomainCounts.ActiveFaults != right.DomainCounts.ActiveFaults {
		return left.DomainCounts.ActiveFaults > right.DomainCounts.ActiveFaults
	}
	if left.Service.Tier != right.Service.Tier {
		return left.Service.Tier < right.Service.Tier
	}
	if left.Service.Name != right.Service.Name {
		return left.Service.Name < right.Service.Name
	}
	return left.Service.ServiceID < right.Service.ServiceID
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

// SummaryOf 依据一批行统计目录摘要。
//
// 它只适用于"这批行就是完整候选集合"的场景（例如没有分页的测试与内部自检）。分页场景
// 必须由上层按 metadata 过滤后的完整集合统计，否则摘要分母会随翻页漂移。
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
