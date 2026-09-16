package app

import (
	"strings"
	"testing"
	"time"

	"alert-system/internal/domain"
)

// 线上 uat 的证据告警全部来自 Alertmanager 权威来源：必须看 source_state，而不是
// 本地响应轴上的 status（已 Ack 的告警 status=acknowledged，但上游可能早已解除）。
func TestAlertCleared_AuthoritativeUsesSourceState(t *testing.T) {
	authoritative := domain.Alert{SourceIntegrationID: "src-1", UpstreamFingerprint: "fp-1"}

	cleared := authoritative
	cleared.SourceState, cleared.Status = domain.SourceStateResolved, domain.StatusAcknowledged
	if !alertCleared(cleared) {
		t.Fatal("权威来源 source_state=resolved 应判定为已解除，与本地 Ack 状态无关")
	}

	firing := authoritative
	firing.SourceState, firing.Status = domain.SourceStateFiring, domain.StatusResolved
	if alertCleared(firing) {
		t.Fatal("权威来源 source_state=firing 不应判定为已解除，即使本地 status 被误写为 resolved")
	}

	legacy := domain.Alert{Status: domain.StatusResolved, SourceState: domain.SourceStateFiring}
	if !alertCleared(legacy) {
		t.Fatal("非权威来源应看本地 status")
	}
}

// 文案里的持续时长必须从进入当前状态开始算，而不是从宣布开始算。
func TestBuildRecoveryNudgeBody_DurationSinceCurrentStatus(t *testing.T) {
	declared := time.Date(2026, 9, 12, 10, 35, 0, 0, time.UTC)
	monitoring := time.Date(2026, 9, 12, 10, 46, 0, 0, time.UTC)
	at := monitoring.Add(96 * time.Hour)
	incident := domain.Incident{Status: domain.IncidentStatusMonitoring, DeclaredAt: declared, MonitoringAt: monitoring}
	detail := domain.IncidentRecoveryNudgeDetail{LatestClearedAt: at.Add(-time.Hour)}

	body := buildRecoveryNudgeBody(incident, detail, at)
	if !strings.Contains(body, "已持续 96h0m0s") {
		t.Fatalf("时长应从 monitoring_at 起算，body=%q", body)
	}
	if strings.Contains(body, "已恢复") {
		t.Fatalf("文案不得出现\"已恢复\"，body=%q", body)
	}

	incident.MonitoringAt = time.Time{}
	if body := buildRecoveryNudgeBody(incident, detail, at); !strings.Contains(body, "已持续 96h11m0s") {
		t.Fatalf("缺失状态时间戳时应回落 declared_at，body=%q", body)
	}
}
