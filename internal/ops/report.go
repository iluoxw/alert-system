package ops

import (
	"fmt"
	"strings"
	"time"

	"alert-system/internal/domain"
	"alert-system/internal/notify"
)

// DailyStats 是日报所需的统计聚合，由任务 7 的 handler 通过 store 统计 SQL 算出后
// 传给 BuildDailyReport；本文件只负责把统计结果排成中文文案，不做任何聚合计算。
type DailyStats struct {
	NewCount      int
	ResolvedCount int
	BySeverity    map[string]int // key 为 domain.Severity 字符串值，如 "critical"

	MTTAAvg         time.Duration
	MTTAAvgValid    bool // false 表示当天无 Ack 数据，均值不可算
	MTTAMedian      time.Duration
	MTTAMedianValid bool

	Prev7DayAvgNew float64 // 前 7 天日均新增数，用于环比；0 表示无历史数据可比（显示"—"）
}

// dailyReportSeverityOrder 固定展示顺序，覆盖为 0 的等级也展示，保持文案格式稳定。
var dailyReportSeverityOrder = []domain.Severity{domain.SeverityCritical, domain.SeverityWarning, domain.SeverityInfo}

// severityLabel 把 severity 取值映射成中文展示名。
func severityLabel(sev domain.Severity) string {
	switch sev {
	case domain.SeverityCritical:
		return "严重"
	case domain.SeverityWarning:
		return "警告"
	case domain.SeverityInfo:
		return "提示"
	default:
		return string(sev)
	}
}

// humanizeDuration 把时长格式化成人性化的中文描述（天/小时/分钟取最粗的两级）。
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalMinutes := int64(d / time.Minute)
	days := totalMinutes / (24 * 60)
	hours := (totalMinutes % (24 * 60)) / 60
	minutes := totalMinutes % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d天%d小时", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d小时%d分钟", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%d分钟", minutes)
	default:
		return "不足1分钟"
	}
}

// maxLongOpenListed 是日报中超时未关闭清单的展示上限，超出部分只提示条数（天级兜底，
// 避免长清单把日报正文撑爆）。
const maxLongOpenListed = 20

// BuildDailyReport 构建团队日报文案。date 是报告所指的自然日（通常是"昨天"），
// longOpen 是超过 24 小时仍未关闭的告警清单（由 store.ListOpenLongerThan 传入）。
func BuildDailyReport(teamName string, date time.Time, stats DailyStats, longOpen []domain.Alert) notify.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "昨日新增 %d 条，恢复 %d 条。\n\n", stats.NewCount, stats.ResolvedCount)

	b.WriteString("按等级分布：")
	parts := make([]string, 0, len(dailyReportSeverityOrder))
	for _, sev := range dailyReportSeverityOrder {
		parts = append(parts, fmt.Sprintf("%s %d", severityLabel(sev), stats.BySeverity[string(sev)]))
	}
	b.WriteString(strings.Join(parts, "，"))
	b.WriteString("。\n\n")

	b.WriteString("平均响应时长（MTTA）：均值 ")
	if stats.MTTAAvgValid {
		b.WriteString(humanizeDuration(stats.MTTAAvg))
	} else {
		b.WriteString("—")
	}
	b.WriteString("，中位 ")
	if stats.MTTAMedianValid {
		b.WriteString(humanizeDuration(stats.MTTAMedian))
	} else {
		b.WriteString("—")
	}
	b.WriteString("。\n\n")

	b.WriteString("较过去 7 天日均新增：")
	if stats.Prev7DayAvgNew == 0 {
		b.WriteString("—（无历史数据）")
	} else {
		delta := (float64(stats.NewCount) - stats.Prev7DayAvgNew) / stats.Prev7DayAvgNew * 100
		fmt.Fprintf(&b, "%.1f 条/天，%+.1f%%", stats.Prev7DayAvgNew, delta)
	}
	b.WriteString("。\n\n")

	if len(longOpen) == 0 {
		b.WriteString("无超过 24 小时未关闭的告警。")
	} else {
		fmt.Fprintf(&b, "超过 24 小时未关闭（共 %d 条）：\n", len(longOpen))
		shown := longOpen
		truncated := false
		if len(shown) > maxLongOpenListed {
			shown = shown[:maxLongOpenListed]
			truncated = true
		}
		for _, a := range shown {
			fmt.Fprintf(&b, "- [%s] %s\n", severityLabel(a.Severity), a.Title)
		}
		if truncated {
			fmt.Fprintf(&b, "（仅显示前 %d 条，共 %d 条）", maxLongOpenListed, len(longOpen))
		}
	}

	return notify.Message{
		Severity:   string(domain.SeverityInfo),
		Title:      fmt.Sprintf("%s 团队日报 %s", teamName, date.Format("2006-01-02")),
		Body:       strings.TrimRight(b.String(), "\n"),
		OccurredAt: date,
	}
}

// BuildStaleSweep 构建悬置扫描通报文案。alerts 应已由调用方过滤为"未 Ack 的 critical 告警"
// （见 store.ListUnackedCritical），本函数只负责格式化，不重复过滤——避免过滤口径在
// 两处分叉。
func BuildStaleSweep(teamName string, alerts []domain.Alert, now time.Time) notify.Message {
	var b strings.Builder
	if len(alerts) == 0 {
		b.WriteString("当前无未响应的严重告警。")
	} else {
		fmt.Fprintf(&b, "以下 %d 条严重告警尚未响应：\n", len(alerts))
		for _, a := range alerts {
			fmt.Fprintf(&b, "- %s（已持续 %s）\n", a.Title, humanizeDuration(now.Sub(a.StartsAt)))
		}
	}

	return notify.Message{
		Severity:   string(domain.SeverityInfo),
		Title:      fmt.Sprintf("%s 悬置告警通报", teamName),
		Body:       strings.TrimRight(b.String(), "\n"),
		OccurredAt: now,
	}
}

// BuildOncallBroadcast 构建值班播报文案。oncallNames 为空表示当天无排班（AC 2.3 要求
// 明示"今日无值班安排"，而非静默不发）。
func BuildOncallBroadcast(teamName string, oncallNames []string, date time.Time) notify.Message {
	var body string
	if len(oncallNames) == 0 {
		body = "今日无值班安排。"
	} else {
		body = fmt.Sprintf("今日值班：%s", strings.Join(oncallNames, "、"))
	}

	return notify.Message{
		Severity:   string(domain.SeverityInfo),
		Title:      fmt.Sprintf("%s 值班播报 %s", teamName, date.Format("2006-01-02")),
		Body:       body,
		OccurredAt: date,
	}
}
