package agentdelivery

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"alert-system/internal/domain"
)

func TestProjectionRoutesUsingPrivateLabelsAndExportsSanitizedMembers(t *testing.T) {
	raw := json.RawMessage(`{"status":"firing","receiver":"private-receiver","groupKey":"private-group","truncatedAlerts":2,"commonLabels":{"team":"infra","env":"fat"},"groupLabels":{"env":"prod"},"commonAnnotations":{"summary":"example-secret"}}`)
	at := time.Unix(100, 0).UTC()
	events := []domain.AlertEvent{
		{RuleID: "NodeDown", UpstreamFingerprint: "private-member-id", Status: domain.StatusResolved, Severity: domain.SeverityInfo, StartsAt: at},
		{RuleID: "DiskFull", Status: domain.StatusFiring, Severity: domain.SeverityCritical, StartsAt: at, Labels: map[string]string{"credential": "example-secret"}},
	}
	routing := Routing{
		ConfigVersion: "rev1",
		Routes: []Rule{
			{Name: "missing", Role: "unused", Labels: map[string]string{"cluster": ""}},
			{Name: "infra", Role: "ops", Source: "alertmanager", SourceID: "source", EventType: "alert.firing", Severity: "critical", Labels: map[string]string{"team": "infra", "env": "prod"}, TTL: time.Minute},
			{Name: "default", Role: "ops", TTL: time.Hour},
			{Name: "fanout", Role: "incident"},
		}, Fallback: Rule{Name: "fallback", Role: "triage"},
	}
	env, policies, err := projectGroup("source", raw, events, routing)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 2 || policies[0].Role != "incident" || policies[1].RouteID != "infra" || policies[1].TTL != time.Minute || policies[1].ConfigVersion != "rev1" {
		t.Fatalf("unexpected policies: %+v", policies)
	}
	if len(env.Labels) != 0 || env.Severity != "critical" || env.EventType != "alert.firing" || env.DedupKey == "" {
		t.Fatalf("incorrect group header: %+v", env)
	}
	var payload projectedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.TruncatedAlerts != 2 || len(payload.Alerts) != 2 || payload.Alerts[0].Status != domain.StatusResolved || payload.Alerts[1].Alertname != "DiskFull" {
		t.Fatalf("incorrect member projection: %+v (%v)", payload, err)
	}
	out, _ := json.Marshal(env)
	for _, value := range []string{"private-member-id", "private-group", "private-receiver", "example-secret", "credential", `"infra"`, "commonAnnotations", "generatorURL"} {
		if strings.Contains(string(out), value) {
			t.Fatalf("client envelope included restricted field %q", value)
		}
	}
	routing.Routes = routing.Routes[:1]
	if got := selectPolicies(routing, env, nil); len(got) != 1 || got[0].Role != "triage" {
		t.Fatalf("fallback: %+v", got)
	}
}

func TestProjectionIdentitiesAndTruncatedFiring(t *testing.T) {
	first := domain.AlertEvent{RuleID: "Alert", UpstreamFingerprint: "a", Status: domain.StatusResolved, Severity: domain.SeverityInfo, StartsAt: time.Unix(200, 0)}
	second := first
	second.UpstreamFingerprint = "b"
	events := []domain.AlertEvent{first, second}
	makeEnv := func(source, raw string) Envelope {
		t.Helper()
		env, _, err := projectGroup(source, json.RawMessage(raw), events, Routing{})
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	root := `{"status":"firing","truncatedAlerts":2,"groupKey":"key","receiver":"r"}`
	a := makeEnv("s1", root)
	if a.EventType != "alert.firing" {
		t.Fatal("a truncated firing group lost its upstream status")
	}
	getRefs := func(e Envelope) [2]string {
		t.Helper()
		var p projectedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.GroupRef == "" || p.ReceiverRef == "" {
			t.Fatal("missing group references", err)
		}
		return [2]string{p.ReceiverRef, p.GroupRef}
	}
	if getRefs(a) != getRefs(makeEnv("s1", strings.Replace(root, "firing", "resolved", 1))) {
		t.Fatal("receiver and group references must survive a phase change")
	}
	refs, other := getRefs(a), getRefs(makeEnv("s2", root))
	if refs[0] == other[0] || refs[1] == other[1] {
		t.Fatal("receiver and group references must be scoped to the source")
	}
	for _, other := range []Envelope{makeEnv("s2", root), makeEnv("s1", strings.Replace(root, `"r"`, `"r2"`, 1)), makeEnv("s1", strings.Replace(root, "firing", "resolved", 1))} {
		if a.DedupKey == other.DedupKey {
			t.Fatal("source, receiver or phase namespace collision")
		}
	}
	keyless := `{"status":"resolved","receiver":"r"}`
	a = makeEnv("s1", keyless)
	slices.Reverse(events)
	if b := makeEnv("s1", keyless); a.DedupKey != b.DedupKey {
		t.Fatal("keyless group depends on member ordering")
	}
	events[0].StartsAt = events[0].StartsAt.Add(time.Minute)
	if b := makeEnv("s1", keyless); a.DedupKey == b.DedupKey {
		t.Fatal("different keyless occurrences collapsed")
	}
}

func TestGrafanaProjectionPreservesNativeSourceAndPublicRule(t *testing.T) {
	raw := json.RawMessage(`{"status":"firing","groupKey":"secret-group","receiver":"secret-receiver","commonLabels":{"product":"OTHER","secret":"secret-label"}}`)
	events := []domain.AlertEvent{{Source: "grafana", RuleID: "IAC Go 系统错误率超过 1%", Status: domain.StatusResolved, Severity: domain.SeverityCritical, StartsAt: time.Unix(10, 0), Labels: map[string]string{"product": "IAC", "sre.rule.id": "iac-go-system-errors", "token": "secret-token"}}}
	r := Routing{PublicRuleIDs: []string{"iac-go-system-errors"}, Routes: []Rule{{Name: "iac", Source: "grafana", Role: "iac", Labels: map[string]string{"product": "IAC"}}}}
	env, policies, err := projectGroup("source", raw, events, r)
	if err != nil {
		t.Fatal(err)
	}
	if env.Source != "grafana" || env.EventType != "alert.resolved" || len(policies) != 1 || policies[0].Role != "iac" {
		t.Fatalf("%+v %+v", env, policies)
	}
	var p projectedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Alerts[0].RuleID != "iac-go-system-errors" || p.Alerts[0].Alertname != "" {
		t.Fatalf("稳定引用不应依赖中文title: %+v", p)
	}
	b, _ := json.Marshal(env)
	for _, secret := range []string{"secret-group", "secret-receiver", "secret-label", "secret-token", "OTHER"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("leaked " + secret)
		}
	}
	r.PublicRuleIDs = nil
	env, _, err = projectGroup("source", raw, events, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env.Payload), "iac-go-system-errors") {
		t.Fatal("非白名单规则不应导出")
	}
}
