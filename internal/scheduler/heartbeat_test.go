package scheduler

import (
	"testing"
	"time"
)

func TestLastPollSuccessAt_ZeroBeforeRecord(t *testing.T) {
	s := &Scheduler{pollInterval: 5 * time.Second}
	if !s.LastPollSuccessAt().IsZero() {
		t.Fatalf("未轮询过时 LastPollSuccessAt 应为零值，got %v", s.LastPollSuccessAt())
	}
	if s.PollInterval() != 5*time.Second {
		t.Fatalf("PollInterval = %v, want 5s", s.PollInterval())
	}
}

func TestRecordPollSuccess_UpdatesTimestamp(t *testing.T) {
	s := &Scheduler{pollInterval: time.Second}
	before := time.Now()
	s.recordPollSuccess()
	got := s.LastPollSuccessAt()
	if got.Before(before) || time.Since(got) > time.Second {
		t.Fatalf("recordPollSuccess 后时间戳不合理: %v (now=%v)", got, time.Now())
	}
}
