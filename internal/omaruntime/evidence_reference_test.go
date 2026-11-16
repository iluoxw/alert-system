package omaruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func evidenceWire(query, status string, value int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"query_id": query, "status": status, "observed_at": "2026-01-01T00:00:00Z", "data": map[string]any{"value": value}})
	return raw
}
func evidenceBundle(t *testing.T, items ...json.RawMessage) EvidenceBundle {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema": "iac_evidence_v1", "items": items})
	queries := []string{}
	for _, item := range items {
		var header Evidence
		_ = json.Unmarshal(item, &header)
		queries = append(queries, header.QueryID)
	}
	out, e := normalizeEvidence(raw, queries)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func factReport(id string) string {
	raw, _ := json.Marshal(Report{Schema: ResultSchema, Verdict: "observed_healthy", Summary: "本次样本观测。", Facts: []Fact{{EvidenceID: id, Statement: "该时刻样本值为零。"}}, Inferences: []string{}, EvidenceGaps: []string{}, Recommendations: []string{}})
	return string(raw)
}
func assertPayloadDigest(t *testing.T, item Evidence) {
	t.Helper()
	// JSONB/HTTP may change whitespace and key order; audit the normalized JSON value.
	var value any
	if e := json.Unmarshal(item.Payload, &value); e != nil {
		t.Fatal(e)
	}
	canonical, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(canonical)
	if len(item.SHA256) != 64 || item.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("payload digest mismatch")
	}
}
func TestNewEvidenceReferencesAreLocalDeterministicAndAuditable(t *testing.T) {
	bundle := evidenceBundle(t, evidenceWire("traffic", "ok", 0), evidenceWire("errors", "error", 1))
	seen := map[string]bool{}
	for _, item := range bundle.Items {
		if item.ID != "ev_"+item.QueryID || seen[item.ID] {
			t.Fatal("ambiguous reference")
		}
		seen[item.ID] = true
		assertPayloadDigest(t, item)
	}
	sameQueryAnotherRun := evidenceBundle(t, evidenceWire("traffic", "ok", 123)).Items[0]
	if sameQueryAnotherRun.ID != bundle.Items[0].ID || sameQueryAnotherRun.SHA256 == bundle.Items[0].SHA256 {
		t.Fatal("reference should be query-stable while audit digest tracks payload")
	}
	reordered := evidenceBundle(t, evidenceWire("errors", "error", 1), evidenceWire("traffic", "ok", 0))
	if reordered.Items[1].ID != bundle.Items[0].ID || reordered.Items[1].SHA256 != bundle.Items[0].SHA256 {
		t.Fatal("item ordering changed identity")
	}
	raw, _ := json.Marshal(map[string]any{"schema": "iac_evidence_v1", "items": []json.RawMessage{evidenceWire("traffic", "ok", 0), evidenceWire("traffic", "ok", 1)}})
	if _, e := normalizeEvidence(raw, []string{"traffic"}); e == nil {
		t.Fatal("duplicate query produced ambiguous references")
	}
	errors := errorEvidence([]string{"traffic", "traffic", "errors"})
	if len(errors.Items) != 2 || !errors.Partial {
		t.Fatal("error evidence references not unique")
	}
	for _, item := range errors.Items {
		if item.ID != "ev_"+item.QueryID {
			t.Fatal(item.ID)
		}
		assertPayloadDigest(t, item)
	}
}
func TestShortEvidenceReferencesRemainExactAndCurrentBundleOnly(t *testing.T) {
	bundle := evidenceBundle(t, evidenceWire("traffic", "ok", 0), evidenceWire("errors", "error", 0), evidenceWire("latency", "unknown", 0))
	if _, e := parseReport(factReport("ev_traffic"), bundle, false); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"ev_trafic", "ev_trafficx", "ev_errors", "ev_latency", "ev_freshness", bundle.Items[0].SHA256} {
		if _, e := parseReport(factReport(id), bundle, false); e == nil {
			t.Fatal("unknown, non-ok or audit-only reference accepted", id)
		}
	}
	other := evidenceBundle(t, evidenceWire("freshness", "ok", 0))
	if _, e := parseReport(factReport("ev_traffic"), other, false); e == nil {
		t.Fatal("reference from another bundle accepted")
	}
}
func TestLegacyFullHashEvidenceRemainsReadableAndExact(t *testing.T) {
	id := strings.Repeat("a", 64)
	raw := `{"schema":"sre.analysis-evidence/v1","items":[{"evidence_id":"` + id + `","query_id":"traffic","status":"ok","observed_at":"2026-01-01T00:00:00Z","payload":{"value":0}}],"partial":false}`
	var bundle EvidenceBundle
	if e := json.Unmarshal([]byte(raw), &bundle); e != nil {
		t.Fatal(e)
	}
	if bundle.Items[0].ID != id || bundle.Items[0].SHA256 != "" {
		t.Fatal("legacy evidence rewritten")
	}
	if _, e := parseReport(factReport(id), bundle, false); e != nil {
		t.Fatal(e)
	}
	for _, wrong := range []string{id[:63], "ev_traffic"} {
		if _, e := parseReport(factReport(wrong), bundle, false); e == nil {
			t.Fatal("legacy reference guessed/repaired")
		}
	}
	encoded, _ := json.Marshal(bundle)
	if strings.Contains(string(encoded), "sha256") {
		t.Fatal("legacy digest was invented")
	}
}
