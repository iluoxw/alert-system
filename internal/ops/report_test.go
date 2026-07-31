package ops

import (
	"strings"
	"testing"
	"time"

	"alert-system/internal/domain"
)

func TestBuildDailyReport_零告警日(t *testing.T) {
	stats := DailyStats{
		BySeverity: map[string]int{},
	}
	date := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	msg := BuildDailyReport("SRE", date, stats, nil)

	if !strings.Contains(msg.Body, "新增 0 条，恢复 0 条") {
		t.Fatalf("零告警日文案缺少新增/恢复计数: %s", msg.Body)
	}
	if !strings.Contains(msg.Body, "无超过 24 小时未关闭的告警") {
		t.Fatalf("零告警日应明示无超时未关闭告警: %s", msg.Body)
	}
	if msg.Severity != string(domain.SeverityInfo) {
		t.Fatalf("severity 应为 info, got %s", msg.Severity)
	}
}

func TestBuildDailyReport_MTTA无效显示占位符(t *testing.T) {
	stats := DailyStats{
		BySeverity:      map[string]int{},
		MTTAAvgValid:    false,
		MTTAMedianValid: false,
	}
	msg := BuildDailyReport("SRE", time.Now(), stats, nil)
	if !strings.Contains(msg.Body, "均值 —") || !strings.Contains(msg.Body, "中位 —") {
		t.Fatalf("无 Ack 数据时 MTTA 均值/中位应显示占位符 —: %s", msg.Body)
	}
}

func TestBuildDailyReport_MTTA有效时显示人性化时长(t *testing.T) {
	stats := DailyStats{
		BySeverity:      map[string]int{},
		MTTAAvg:         90 * time.Minute,
		MTTAAvgValid:    true,
		MTTAMedian:      45 * time.Minute,
		MTTAMedianValid: true,
	}
	msg := BuildDailyReport("SRE", time.Now(), stats, nil)
	if !strings.Contains(msg.Body, "1小时30分钟") {
		t.Fatalf("MTTA 均值应格式化为人性化时长: %s", msg.Body)
	}
	if !strings.Contains(msg.Body, "45分钟") {
		t.Fatalf("MTTA 中位应格式化为人性化时长: %s", msg.Body)
	}
}

func TestBuildDailyReport_7日均值除零显示占位符(t *testing.T) {
	stats := DailyStats{
		BySeverity:     map[string]int{},
		Prev7DayAvgNew: 0,
	}
	msg := BuildDailyReport("SRE", time.Now(), stats, nil)
	if !strings.Contains(msg.Body, "—（无历史数据）") {
		t.Fatalf("7 日均值为 0 时应显示占位符而非除零: %s", msg.Body)
	}
}

func TestBuildDailyReport_7日均值正常对比(t *testing.T) {
	stats := DailyStats{
		BySeverity:     map[string]int{},
		NewCount:       20,
		Prev7DayAvgNew: 10,
	}
	msg := BuildDailyReport("SRE", time.Now(), stats, nil)
	if !strings.Contains(msg.Body, "+100.0%") {
		t.Fatalf("环比应算出 +100.0%%: %s", msg.Body)
	}
}

func TestBuildDailyReport_长清单截断(t *testing.T) {
	longOpen := make([]domain.Alert, 25)
	for i := range longOpen {
		longOpen[i] = domain.Alert{Severity: domain.SeverityWarning, Title: "alert"}
	}
	stats := DailyStats{BySeverity: map[string]int{}}
	msg := BuildDailyReport("SRE", time.Now(), stats, longOpen)

	if !strings.Contains(msg.Body, "共 25 条") {
		t.Fatalf("应显示总条数 25: %s", msg.Body)
	}
	if !strings.Contains(msg.Body, "仅显示前 20 条") {
		t.Fatalf("超过 20 条应提示截断: %s", msg.Body)
	}
	if strings.Count(msg.Body, "- [警告] alert") != 20 {
		t.Fatalf("清单展示条数应恰为 20 条, got %d", strings.Count(msg.Body, "- [警告] alert"))
	}
}

func TestBuildDailyReport_短清单不截断(t *testing.T) {
	longOpen := []domain.Alert{
		{Severity: domain.SeverityCritical, Title: "db down"},
	}
	stats := DailyStats{BySeverity: map[string]int{}}
	msg := BuildDailyReport("SRE", time.Now(), stats, longOpen)
	if strings.Contains(msg.Body, "仅显示前") {
		t.Fatalf("未超过上限不应出现截断提示: %s", msg.Body)
	}
	if !strings.Contains(msg.Body, "- [严重] db down") {
		t.Fatalf("应包含告警条目: %s", msg.Body)
	}
}

func TestBuildStaleSweep_无未响应告警(t *testing.T) {
	msg := BuildStaleSweep("SRE", nil, time.Now())
	if !strings.Contains(msg.Body, "当前无未响应的严重告警") {
		t.Fatalf("空列表应明示无未响应告警: %s", msg.Body)
	}
}

func TestBuildStaleSweep_列出持续时长(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	alerts := []domain.Alert{
		{Title: "db down", StartsAt: now.Add(-2*time.Hour - 15*time.Minute)},
	}
	msg := BuildStaleSweep("SRE", alerts, now)
	if !strings.Contains(msg.Body, "db down（已持续 2小时15分钟）") {
		t.Fatalf("应列出告警标题与持续时长: %s", msg.Body)
	}
}

func TestBuildOncallBroadcast_无排班明示(t *testing.T) {
	msg := BuildOncallBroadcast("SRE", nil, time.Now())
	if !strings.Contains(msg.Body, "今日无值班安排") {
		t.Fatalf("无排班时应明示: %s", msg.Body)
	}
}

func TestBuildOncallBroadcast_有排班列出姓名(t *testing.T) {
	msg := BuildOncallBroadcast("SRE", []string{"张三", "李四"}, time.Now())
	if !strings.Contains(msg.Body, "张三、李四") {
		t.Fatalf("应列出值班人姓名: %s", msg.Body)
	}
}
