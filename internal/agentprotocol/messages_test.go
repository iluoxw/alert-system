package agentprotocol

import (
	"testing"
	"time"
)

func TestOccurrenceRefPreservesGrafanaDatabasePrecisionAndLegacyAM(t *testing.T) {
	at := time.Unix(100, 123456789).UTC()
	stored := at.Truncate(time.Microsecond)
	if OccurrenceRef("grafana", "source", "fp", at) != OccurrenceRef("grafana", "source", "fp", stored) {
		t.Fatal("Grafana PG回读不能改变成员身份")
	}
	if OccurrenceRef("alertmanager", "source", "fp", at) == OccurrenceRef("alertmanager", "source", "fp", stored) {
		t.Fatal("Alertmanager旧契约保持纳秒")
	}
	if OccurrenceRef("grafana", "other", "fp", at) == OccurrenceRef("grafana", "source", "fp", at) {
		t.Fatal("来源隔离")
	}
}
