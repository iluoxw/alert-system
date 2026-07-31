package ops

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("加载时区 %s 失败: %v", name, err)
	}
	return loc
}

func TestNextFire_DailyReport_跨日与时区换算(t *testing.T) {
	shanghai := mustLoc(t, "Asia/Shanghai")

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "今天时刻未到_取今天",
			now:  time.Date(2026, 7, 30, 8, 0, 0, 0, shanghai),
			want: time.Date(2026, 7, 30, 9, 0, 0, 0, shanghai),
		},
		{
			name: "今天时刻已过_取明天",
			now:  time.Date(2026, 7, 30, 9, 1, 0, 0, shanghai),
			want: time.Date(2026, 7, 31, 9, 0, 0, 0, shanghai),
		},
		{
			name: "恰好等于配置时刻_视为已过_取明天",
			now:  time.Date(2026, 7, 30, 9, 0, 0, 0, shanghai),
			want: time.Date(2026, 7, 31, 9, 0, 0, 0, shanghai),
		},
		{
			name: "23_59后跨日",
			now:  time.Date(2026, 7, 30, 23, 59, 0, 0, shanghai),
			want: time.Date(2026, 7, 31, 9, 0, 0, 0, shanghai),
		},
	}

	s := Settings{Timezone: "Asia/Shanghai", DailyReportTime: "09:00"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NextFire(TimerKindDailyReport, s, c.now)
			if err != nil {
				t.Fatalf("NextFire 返回意外 error: %v", err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNextFire_跨时区换算_UTC与Shanghai一致(t *testing.T) {
	// UTC 时区下的 01:00 等于 Asia/Shanghai 的 09:00，验证时区确实参与了计算而非硬编码本地时间。
	utcSettings := Settings{Timezone: "UTC", DailyReportTime: "01:00"}
	now := time.Date(2026, 7, 30, 0, 30, 0, 0, time.UTC)
	got, err := NextFire(TimerKindDailyReport, utcSettings, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextFire_时区非法回退UTC(t *testing.T) {
	s := Settings{Timezone: "Not/AReal_Zone", DailyReportTime: "09:00"}
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	got, err := NextFire(TimerKindDailyReport, s, now)
	if err == nil {
		t.Fatalf("期望时区非法时返回 error，实际为 nil")
	}
	want := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("回退 UTC 后计算错误: got %v, want %v", got, want)
	}
}

func TestNextFire_值班播报_与日报共用逻辑(t *testing.T) {
	s := Settings{Timezone: "Asia/Shanghai", OncallBroadcastTime: "09:30"}
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, mustLoc(t, "Asia/Shanghai"))
	got, err := NextFire(TimerKindOncallBroadcast, s, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 7, 31, 9, 30, 0, 0, mustLoc(t, "Asia/Shanghai"))
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextFire_StaleSweep_按interval对齐(t *testing.T) {
	s := Settings{Timezone: "UTC", StaleSweepIntervalSeconds: 1800}
	// epoch 起 3600 秒（恰为一个网格点）之后 100 秒，下一个网格点应为 5400（3600+1800）。
	now := time.Unix(3700, 0).UTC()
	got, err := NextFire(TimerKindStaleSweep, s, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Unix() != 5400 {
		t.Fatalf("got unix %d, want 5400", got.Unix())
	}
	// 对齐必须保证结果严格晚于 now。
	if !got.After(now) {
		t.Fatalf("下一次触发时刻必须晚于 now: got %v, now %v", got, now)
	}
}

func TestNextFire_StaleSweep_恰好落在网格点上仍取下一个网格点(t *testing.T) {
	s := Settings{Timezone: "UTC", StaleSweepIntervalSeconds: 1800}
	now := time.Unix(3600, 0).UTC() // 本身就是网格点
	got, err := NextFire(TimerKindStaleSweep, s, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Unix() != 5400 {
		t.Fatalf("got unix %d, want 5400", got.Unix())
	}
}

func TestNextFire_未知kind返回error(t *testing.T) {
	s := Settings{Timezone: "UTC"}
	_, err := NextFire("not_a_kind", s, time.Now())
	if err == nil {
		t.Fatalf("期望未知 kind 返回 error")
	}
}

func TestNextFire_时刻格式非法返回error(t *testing.T) {
	s := Settings{Timezone: "UTC", DailyReportTime: "not-a-time"}
	_, err := NextFire(TimerKindDailyReport, s, time.Now())
	if err == nil {
		t.Fatalf("期望非法 HH:MM 格式返回 error")
	}
}

func TestPeriodRound_日报同轮重算一致(t *testing.T) {
	s := Settings{Timezone: "Asia/Shanghai", DailyReportTime: "09:00"}
	fireAt := time.Date(2026, 7, 30, 9, 0, 0, 0, mustLoc(t, "Asia/Shanghai"))
	r1 := PeriodRound(TimerKindDailyReport, s, fireAt)
	r2 := PeriodRound(TimerKindDailyReport, s, fireAt)
	if r1 != r2 {
		t.Fatalf("同一 fireAt 重算应得到一致 Round: %d != %d", r1, r2)
	}
	if r1 != 20260730 {
		t.Fatalf("got %d, want 20260730", r1)
	}
}

func TestPeriodRound_跨日变化(t *testing.T) {
	s := Settings{Timezone: "Asia/Shanghai", DailyReportTime: "09:00"}
	day1 := time.Date(2026, 7, 30, 9, 0, 0, 0, mustLoc(t, "Asia/Shanghai"))
	day2 := time.Date(2026, 7, 31, 9, 0, 0, 0, mustLoc(t, "Asia/Shanghai"))
	r1 := PeriodRound(TimerKindDailyReport, s, day1)
	r2 := PeriodRound(TimerKindDailyReport, s, day2)
	if r1 == r2 {
		t.Fatalf("跨日的两次 fireAt 应产生不同 Round，均为 %d", r1)
	}
}

func TestPeriodRound_StaleSweep_窗口对齐重算一致(t *testing.T) {
	s := Settings{StaleSweepIntervalSeconds: 1800}
	// 即便传入的 fireAt 并非精确网格点，也应收敛到同一个窗口起点。
	inWindow1 := time.Unix(3700, 0).UTC()
	inWindow2 := time.Unix(3750, 0).UTC()
	r1 := PeriodRound(TimerKindStaleSweep, s, inWindow1)
	r2 := PeriodRound(TimerKindStaleSweep, s, inWindow2)
	if r1 != r2 {
		t.Fatalf("同一窗口内不同 fireAt 应算出同一 Round: %d != %d", r1, r2)
	}
	if r1 != 3600 {
		t.Fatalf("got %d, want 3600", r1)
	}
}
