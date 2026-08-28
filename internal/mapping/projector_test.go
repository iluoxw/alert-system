package mapping

import (
	"testing"

	"alert-system/internal/domain"
)

func TestBuildCorrelationKeyDeterministicAndEntityScoped(t *testing.T) {
	labels := map[string]string{"cluster": "prod-a", "instance": "node-1"}
	first := buildCorrelationKey("service/cch/prod", "proxy_degradation", []string{"instance", "cluster"}, labels)
	second := buildCorrelationKey("service/cch/prod", "proxy_degradation", []string{"cluster", "instance"}, labels)
	if first != second {
		t.Fatalf("correlation labels 顺序不应改变 key: %s != %s", first, second)
	}
	otherEntity := buildCorrelationKey("service/alpha/prod", "proxy_degradation", []string{"cluster", "instance"}, labels)
	if otherEntity == first {
		t.Fatal("不同 canonical entity 不得共享 correlation key")
	}
}

func TestAlertLabelViewUsesExplicitLabelsOverResource(t *testing.T) {
	view := alertLabelView(domain.Alert{
		Severity:    domain.SeverityCritical,
		Source:      "alertmanager",
		ResourceRef: map[string]string{"service": "resource-value", "host": "node-1"},
		Labels:      map[string]string{"service": "label-value", "alertname": "Probe"},
	})
	if view["service"] != "label-value" || view["host"] != "node-1" ||
		view["severity"] != "critical" || view["source"] != "alertmanager" {
		t.Fatalf("label view 合并错误: %+v", view)
	}
}

func TestAuthoritativeResolutionUsesSourceState(t *testing.T) {
	alert := domain.Alert{UpstreamFingerprint: "upstream", Status: domain.StatusAcknowledged, SourceState: domain.SourceStateResolved}
	if !isResolved(alert) {
		t.Fatal("Alertmanager occurrence 必须以 source_state 判断恢复")
	}
	alert = domain.Alert{Status: domain.StatusResolved}
	if !isResolved(alert) {
		t.Fatal("兼容来源仍应以 legacy status 判断恢复")
	}
}

func TestAuthoritativeAlertmanagerRequiresRegisteredOccurrenceIdentity(t *testing.T) {
	valid := domain.Alert{Source: "alertmanager", SourceIntegrationID: "source-1", UpstreamFingerprint: "fp-1"}
	if !isAuthoritativeAlertmanager(valid) {
		t.Fatal("已登记 Alertmanager occurrence 应被视为权威来源")
	}
	for _, alert := range []domain.Alert{
		{Source: "generic", SourceIntegrationID: "source-1", UpstreamFingerprint: "fp-1"},
		{Source: "alertmanager", UpstreamFingerprint: "fp-1"},
		{Source: "alertmanager", SourceIntegrationID: "source-1"},
	} {
		if isAuthoritativeAlertmanager(alert) {
			t.Fatalf("非完整权威身份不得进入 Fault Mapping: %+v", alert)
		}
	}
}
