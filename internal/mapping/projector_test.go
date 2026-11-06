package mapping

import (
	"testing"
	"time"

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

// TestBuildFaultUpsertInputOwner 覆盖 R3.2/R3.3：owner 只取
// contract.Entity.OwnerTeamID，解析不到（空实体）时置空交给 store 层 NULLIF 转
// NULL；不得依据 Alert 标签推断（用一个带干扰标签的 Alert 验证不会被读取）。
func TestBuildFaultUpsertInputOwner(t *testing.T) {
	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	alert := domain.Alert{
		Title:    "探针失败",
		Severity: domain.SeverityCritical,
		Labels:   map[string]string{"owner_team_id": "label-team-不应被采信"},
	}

	t.Run("有owner", func(t *testing.T) {
		contract := resolvedContract{
			Entity:     domain.ResourceNode{ID: "node-1", EntityKey: "svc/a", Name: "svc-a", OwnerTeamID: "team-1"},
			FaultKind:  "proxy_degradation",
			FaultScope: domain.FaultScopeCapability,
			Confidence: 0.9,
		}
		fault := buildFaultUpsertInput(contract, alert, "corr-1", domain.FaultStatusActive, now, now)
		if fault.OwnerTeamID != "team-1" {
			t.Fatalf("OwnerTeamID = %q, want %q", fault.OwnerTeamID, "team-1")
		}
	})

	t.Run("无owner", func(t *testing.T) {
		contract := resolvedContract{
			Entity:     domain.ResourceNode{ID: "node-2", EntityKey: "svc/b", Name: "svc-b"},
			FaultKind:  "proxy_degradation",
			FaultScope: domain.FaultScopeCapability,
			Confidence: 0.9,
		}
		fault := buildFaultUpsertInput(contract, alert, "corr-2", domain.FaultStatusActive, now, now)
		if fault.OwnerTeamID != "" {
			t.Fatalf("OwnerTeamID = %q, want 空字符串（交由 store 层 NULLIF 转 NULL）", fault.OwnerTeamID)
		}
	})
}

// TestDetermineFaultTransition 覆盖 R5.1/R5.10/R5.11 的转移判定矩阵：created(active)、
// candidate→active、monitoring→active（观察期内复发，不变量 22 的"没有告警了不等于
// 恢复"场景）、已 active 的重复评估不应重复触发、非 active 目标状态不触发。
func TestDetermineFaultTransition(t *testing.T) {
	tests := []struct {
		name     string
		created  bool
		previous domain.FaultStatus
		next     domain.FaultStatus
		want     domain.FaultTransition
	}{
		{
			name:    "新建即active",
			created: true, previous: "", next: domain.FaultStatusActive,
			want: domain.FaultTransitionCreatedActive,
		},
		{
			name:    "candidate升级为active",
			created: false, previous: domain.FaultStatusCandidate, next: domain.FaultStatusActive,
			want: domain.FaultTransitionEnteredActive,
		},
		{
			name:    "monitoring复发升级为active",
			created: false, previous: domain.FaultStatusMonitoring, next: domain.FaultStatusActive,
			want: domain.FaultTransitionEnteredActive,
		},
		{
			name:    "已active不重复触发",
			created: false, previous: domain.FaultStatusActive, next: domain.FaultStatusActive,
			want: domain.FaultTransitionNone,
		},
		{
			name:    "新建但为candidate不触发",
			created: true, previous: "", next: domain.FaultStatusCandidate,
			want: domain.FaultTransitionNone,
		},
		{
			name:    "monitoring不触发",
			created: false, previous: domain.FaultStatusActive, next: domain.FaultStatusMonitoring,
			want: domain.FaultTransitionNone,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := determineFaultTransition(test.created, test.previous, test.next); got != test.want {
				t.Fatalf("determineFaultTransition(%v, %q, %q) = %q, want %q",
					test.created, test.previous, test.next, got, test.want)
			}
		})
	}
}
