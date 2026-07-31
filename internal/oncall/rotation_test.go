package oncall

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateRotation_常规轮换三人两轮(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	shiftDur := 8 * time.Hour
	shifts, err := GenerateRotation([]string{"u1", "u2", "u3"}, start, shiftDur, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(shifts) != 6 {
		t.Fatalf("expected 6 shifts, got %d", len(shifts))
	}

	// 成员顺序循环：u1,u2,u3,u1,u2,u3
	wantUsers := []string{"u1", "u2", "u3", "u1", "u2", "u3"}
	for i, want := range wantUsers {
		if shifts[i].UserID != want {
			t.Errorf("shift[%d].UserID = %s, want %s", i, shifts[i].UserID, want)
		}
	}

	// 时间连续：每班 StartsAt 递增 shiftDur，前班 EndsAt == 后班 StartsAt
	for i, sh := range shifts {
		wantStart := start.Add(time.Duration(i) * shiftDur)
		if !sh.StartsAt.Equal(wantStart) {
			t.Errorf("shift[%d].StartsAt = %v, want %v", i, sh.StartsAt, wantStart)
		}
		if !sh.EndsAt.Equal(sh.StartsAt.Add(shiftDur)) {
			t.Errorf("shift[%d].EndsAt = %v, want %v", i, sh.EndsAt, sh.StartsAt.Add(shiftDur))
		}
		if i > 0 && !shifts[i-1].EndsAt.Equal(sh.StartsAt) {
			t.Errorf("shift[%d].EndsAt %v != shift[%d].StartsAt %v", i-1, shifts[i-1].EndsAt, i, sh.StartsAt)
		}
	}
}

func TestGenerateRotation_单成员多轮(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	shiftDur := 24 * time.Hour
	shifts, err := GenerateRotation([]string{"solo"}, start, shiftDur, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(shifts) != 5 {
		t.Fatalf("expected 5 shifts, got %d", len(shifts))
	}
	for i, sh := range shifts {
		if sh.UserID != "solo" {
			t.Errorf("shift[%d].UserID = %s, want solo", i, sh.UserID)
		}
		wantStart := start.Add(time.Duration(i) * shiftDur)
		if !sh.StartsAt.Equal(wantStart) {
			t.Errorf("shift[%d].StartsAt = %v, want %v", i, sh.StartsAt, wantStart)
		}
	}
}

func TestGenerateRotation_上限触发(t *testing.T) {
	userIDs := make([]string, 100)
	for i := range userIDs {
		userIDs[i] = strings.Repeat("u", 1) + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 100 * 11 = 1100 > 1000，应报错
	_, err := GenerateRotation(userIDs, start, time.Hour, 11)
	if err == nil {
		t.Fatal("expected error when total shifts exceed limit, got nil")
	}

	// 100 * 10 = 1000，恰好等于上限，应成功
	shifts, err := GenerateRotation(userIDs, start, time.Hour, 10)
	if err != nil {
		t.Fatalf("unexpected error at exact limit: %v", err)
	}
	if len(shifts) != 1000 {
		t.Fatalf("expected 1000 shifts, got %d", len(shifts))
	}
}

func TestGenerateRotation_参数校验(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		userIDs  []string
		shiftDur time.Duration
		rounds   int
	}{
		{"空成员列表", []string{}, time.Hour, 1},
		{"含空字符串", []string{"u1", ""}, time.Hour, 1},
		{"含重复ID", []string{"u1", "u2", "u1"}, time.Hour, 1},
		{"时长为0", []string{"u1"}, 0, 1},
		{"时长为负", []string{"u1"}, -time.Hour, 1},
		{"轮数为0", []string{"u1"}, time.Hour, 0},
		{"轮数为负", []string{"u1"}, time.Hour, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := GenerateRotation(c.userIDs, start, c.shiftDur, c.rounds)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", c.name)
			}
		})
	}
}

func TestGenerateRotation_跨月跨年起点不受时区影响(t *testing.T) {
	// 起点为 2025-12-31 23:00 UTC+8（等价于 2025-12-31 15:00 UTC），验证跨年、
	// 且班次运算保留调用方传入的时区而不做本地转换。
	loc := time.FixedZone("UTC+8", 8*60*60)
	start := time.Date(2025, 12, 31, 23, 0, 0, 0, loc)
	shiftDur := 2 * time.Hour

	shifts, err := GenerateRotation([]string{"a", "b"}, start, shiftDur, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(shifts) != 4 {
		t.Fatalf("expected 4 shifts, got %d", len(shifts))
	}

	// 第一班保留原时区且值不变
	if shifts[0].StartsAt.Location().String() != loc.String() {
		t.Errorf("StartsAt location = %v, want %v", shifts[0].StartsAt.Location(), loc)
	}
	if !shifts[0].StartsAt.Equal(start) {
		t.Errorf("shifts[0].StartsAt = %v, want %v", shifts[0].StartsAt, start)
	}

	// 跨年边界：第二班 StartsAt 应为 2026-01-01 01:00 +08:00
	wantSecond := time.Date(2026, 1, 1, 1, 0, 0, 0, loc)
	if !shifts[1].StartsAt.Equal(wantSecond) {
		t.Errorf("shifts[1].StartsAt = %v, want %v", shifts[1].StartsAt, wantSecond)
	}

	// 用 UTC 时刻交叉验证，确认与本地时区表示无关，底层瞬时时刻正确
	wantSecondUTC := time.Date(2025, 12, 31, 17, 0, 0, 0, time.UTC)
	if !shifts[1].StartsAt.Equal(wantSecondUTC) {
		t.Errorf("shifts[1].StartsAt (UTC-compared) = %v, want %v", shifts[1].StartsAt.UTC(), wantSecondUTC)
	}
}
