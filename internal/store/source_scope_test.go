package store

import (
	"alert-system/internal/domain"
	"errors"
	"testing"
)

func TestSourceScopeFilteringAndSyntheticFailClosed(t *testing.T) {
	c := AlertmanagerSourceConfig{LabelScope: domain.MatcherSet{{Name: "product", Op: domain.MatchEqual, Value: "IAC"}}, RuleIDAllowlist: []string{"iac-go-system-errors"}}
	event := func(name, product, id string) domain.AlertEvent {
		return domain.AlertEvent{RuleID: name, Labels: map[string]string{"product": product, StableRuleIDLabel: id}}
	}
	for _, tc := range []struct {
		name    string
		event   domain.AlertEvent
		count   int
		blocked bool
	}{
		{"owned", event("错误率", "IAC", "iac-go-system-errors"), 1, false},
		{"conflicting-product", event("错误率", "OTHER", "iac-go-system-errors"), 0, true},
		{"other-product", event("错误率", "OTHER", "other-rule"), 0, false},
		{"missing-product", event("错误率", "", "iac-go-system-errors"), 0, true},
		{"not-allowlisted", event("错误率", "IAC", "other-rule"), 0, false},
		{"missing-rule-id", event("错误率", "IAC", ""), 0, true},
		{"unknown-nodata", event("DatasourceNoData", "", ""), 0, true},
		{"unknown-error", event("DatasourceError", "", "iac-go-system-errors"), 0, true},
		{"other-nodata", event("DatasourceNoData", "OTHER", ""), 0, false},
		{"owned-nodata", event("DatasourceNoData", "IAC", "iac-go-system-errors"), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := c.FilterEvents([]domain.AlertEvent{tc.event})
			if tc.blocked {
				if !errors.Is(err, ErrSourceScopeUnverifiable) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || len(out) != tc.count {
				t.Fatalf("out=%v err=%v", out, err)
			}
		})
	}
	invalid := c
	invalid.LabelScope = domain.MatcherSet{{Name: "product", Op: domain.MatchRegexp, Value: ".*"}}
	if invalid.ValidateScope() == nil {
		t.Fatal("不允许宽范围正则")
	}
	if (AlertmanagerSourceConfig{}).ScopeKey() != (AlertmanagerSourceConfig{LabelScope: domain.MatcherSet{}, RuleIDAllowlist: []string{}}).ScopeKey() {
		t.Fatal("空范围表示应兼容")
	}
}
