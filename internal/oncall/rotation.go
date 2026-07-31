package oncall

import (
	"fmt"
	"time"
)

// RotationShift 是 GenerateRotation 产出的一个班次时段（尚未落库，无 ID）。
type RotationShift struct {
	UserID   string
	StartsAt time.Time
	EndsAt   time.Time
}

// maxRotationShifts 是 GenerateRotation 单次生成的班次数上限：rounds×len(userIDs)
// 超过此值即报错，防误操作（如误填 rounds 为天数而非"轮"）一次性灌入海量行。
const maxRotationShifts = 1000

// GenerateRotation 按 userIDs 顺序循环分配连续时段：第 i 班 StartsAt = start +
// i*shiftDur，EndsAt = StartsAt + shiftDur，共产出 rounds*len(userIDs) 班（C2）。
// 纯函数：不做任何 IO 或本地时区换算，start 的时区由调用方决定，本函数只用
// time.Time.Add 做时间运算，原样透传时区信息。
func GenerateRotation(userIDs []string, start time.Time, shiftDur time.Duration, rounds int) ([]RotationShift, error) {
	if len(userIDs) == 0 {
		return nil, fmt.Errorf("oncall: 轮换生成失败：成员列表不能为空")
	}
	seen := make(map[string]struct{}, len(userIDs))
	for _, id := range userIDs {
		if id == "" {
			return nil, fmt.Errorf("oncall: 轮换生成失败：成员 ID 不能为空字符串")
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("oncall: 轮换生成失败：成员 ID 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
	if shiftDur <= 0 {
		return nil, fmt.Errorf("oncall: 轮换生成失败：班次时长必须大于 0")
	}
	if rounds <= 0 {
		return nil, fmt.Errorf("oncall: 轮换生成失败：轮数必须大于 0")
	}
	total := rounds * len(userIDs)
	if total > maxRotationShifts {
		return nil, fmt.Errorf("oncall: 轮换生成失败：生成班次数 %d 超过上限 %d", total, maxRotationShifts)
	}

	shifts := make([]RotationShift, 0, total)
	shiftStart := start
	for round := 0; round < rounds; round++ {
		for _, userID := range userIDs {
			shiftEnd := shiftStart.Add(shiftDur)
			shifts = append(shifts, RotationShift{
				UserID:   userID,
				StartsAt: shiftStart,
				EndsAt:   shiftEnd,
			})
			shiftStart = shiftEnd
		}
	}
	return shifts, nil
}
