package config

import (
	"testing"
	"time"
)

func TestSchedulerCapacityDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchedulerNormalWorkers != 1 || cfg.SchedulerNotifyWorkers != 2 || cfg.SchedulerExecutionTimeout != 120*time.Second {
		t.Fatalf("defaults %+v", cfg)
	}
	path := writeTempYAML(t, "scheduler_normal_workers: 3\nscheduler_notify_workers: 4\nscheduler_execution_timeout: 90s\n")
	t.Setenv("SCHEDULER_NOTIFY_WORKERS", "5")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchedulerNormalWorkers != 3 || cfg.SchedulerNotifyWorkers != 5 || cfg.SchedulerExecutionTimeout != 90*time.Second {
		t.Fatal("override failed")
	}
}
func TestSchedulerCapacityRejectsInvalid(t *testing.T) {
	for _, tt := range []struct{ key, value string }{{"SCHEDULER_NORMAL_WORKERS", "0"}, {"SCHEDULER_NORMAL_WORKERS", "33"}, {"SCHEDULER_NOTIFY_WORKERS", "-1"}, {"SCHEDULER_NOTIFY_WORKERS", "bad"}, {"SCHEDULER_EXECUTION_TIMEOUT", "241s"}, {"SCHEDULER_EXECUTION_TIMEOUT", "0"}, {"SCHEDULER_POLL_INTERVAL", "0s"}, {"SCHEDULER_POLL_INTERVAL", "2m"}} {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := Load(""); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
