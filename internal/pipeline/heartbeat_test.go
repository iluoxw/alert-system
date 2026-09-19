package pipeline

import (
	"testing"
	"time"
)

func TestLastPollSuccessAt_ZeroBeforeRecord(t *testing.T) {
	p := &Pipeline{}
	if !p.LastPollSuccessAt().IsZero() {
		t.Fatalf("未轮询过时 LastPollSuccessAt 应为零值，got %v", p.LastPollSuccessAt())
	}
	if p.PollInterval() != readinessPollFloor {
		t.Fatalf("PollInterval = %v, want readiness 下限 %v", p.PollInterval(), readinessPollFloor)
	}
}

func TestRecordPollSuccess_UpdatesTimestamp(t *testing.T) {
	p := &Pipeline{}
	before := time.Now()
	p.recordPollSuccess()
	got := p.LastPollSuccessAt()
	if got.Before(before) || time.Since(got) > time.Second {
		t.Fatalf("recordPollSuccess 后时间戳不合理: %v (now=%v)", got, time.Now())
	}
}
