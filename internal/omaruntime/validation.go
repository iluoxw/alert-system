package omaruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

var urlPattern = regexp.MustCompile(`https?://[^\s"<>]+`)

func safeText(s string, max int) string {
	s = urlPattern.ReplaceAllString(s, "[redacted-url]")
	if len(s) > max {
		s = s[:max]
	}
	return s
}
func decodeJSON(text string, out any) error {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json") && strings.HasSuffix(text, "```") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
	}
	if len(text) > 64*1024 {
		return errors.New("oma runtime: model JSON too large")
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return errors.New("oma runtime: invalid model JSON")
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		return errors.New("oma runtime: extra model output")
	}
	return nil
}
func parseIntent(text string, allowed []string) (Intent, error) {
	var v Intent
	if e := decodeJSON(text, &v); e != nil {
		return v, e
	}
	if v.Schema != IntentSchema || len(v.QueryIDs) < 1 || len(v.QueryIDs) > 6 {
		return v, errors.New("oma runtime: invalid query intent")
	}
	set := map[string]bool{}
	for _, id := range allowed {
		set[id] = true
	}
	seen := map[string]bool{}
	for _, id := range v.QueryIDs {
		if !set[id] || seen[id] {
			return v, errors.New("oma runtime: query intent outside allowlist")
		}
		seen[id] = true
	}
	v.Rationale = safeText(v.Rationale, 2000)
	return v, nil
}
func normalizeEvidence(raw json.RawMessage, queries []string) (EvidenceBundle, error) {
	var wire struct {
		Schema  string            `json:"schema"`
		Items   []json.RawMessage `json:"items"`
		Partial bool              `json:"partial"`
	}
	out := EvidenceBundle{Schema: "sre.analysis-evidence/v1", Items: []Evidence{}}
	if len(raw) > 512*1024 || json.Unmarshal(raw, &wire) != nil || wire.Schema == "" || len(wire.Items) > 6 {
		return out, errors.New("oma runtime: invalid broker evidence")
	}
	allowed := map[string]bool{}
	for _, id := range queries {
		allowed[id] = true
	}
	seen := map[string]bool{}
	for _, item := range wire.Items {
		var header Evidence
		if json.Unmarshal(item, &header) != nil || !allowed[header.QueryID] || seen[header.QueryID] || header.ObservedAt.IsZero() {
			return out, errors.New("oma runtime: evidence scope mismatch")
		}
		seen[header.QueryID] = true
		if header.Status != "ok" && header.Status != "unknown" && header.Status != "error" {
			return out, errors.New("oma runtime: invalid evidence status")
		}
		// The registered broker emits numeric samples and a fixed public scope. No arbitrary tool output is accepted.
		var scrub any
		if json.Unmarshal(item, &scrub) != nil {
			return out, errors.New("oma runtime: invalid evidence JSON")
		}
		scrub = scrubJSON(scrub)
		clean, e := json.Marshal(scrub)
		if e != nil {
			return out, e
		}
		sum := sha256.Sum256(clean)
		header.ID = hex.EncodeToString(sum[:])
		header.Payload = clean
		out.Items = append(out.Items, header)
		if header.Status != "ok" {
			out.Partial = true
		}
	}
	if len(seen) != len(allowed) {
		out.Partial = true
	}
	out.Partial = out.Partial || wire.Partial
	return out, nil
}
func scrubJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range v {
			switch strings.ToLower(k) {
			case "authorization", "api_key", "apikey", "password", "secret", "token", "cookie", "dsn", "target", "url":
				out[k] = "[redacted]"
			default:
				out[k] = scrubJSON(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = scrubJSON(item)
		}
		return out
	case string:
		return safeText(v, 8192)
	default:
		return value
	}
}
func systemReport(status, code string) Report {
	return Report{Schema: ResultSchema, Status: status, Verdict: "insufficient_evidence", Summary: "分析未完整完成；请查看证据不足与运行原因。", Facts: []Fact{}, Inferences: []string{}, EvidenceGaps: []string{code}, Recommendations: []string{"结合已持久化证据人工复核；提交结果未知时禁止盲目重复外部请求。"}, ReasonCode: code}
}
func parseReport(text string, evidence EvidenceBundle, observationFault bool) (Report, error) {
	var report Report
	if e := decodeJSON(text, &report); e != nil {
		return report, e
	}
	if report.Schema != ResultSchema || report.Summary == "" || len(report.Facts) > 32 || len(report.Inferences) > 32 || len(report.EvidenceGaps) > 32 || len(report.Recommendations) > 32 {
		return report, errors.New("oma runtime: invalid result schema")
	}
	switch report.Verdict {
	case "observed_healthy", "observed_degraded", "insufficient_evidence", "unknown":
	default:
		return report, errors.New("oma runtime: invalid verdict")
	}
	ids := map[string]bool{}
	for _, item := range evidence.Items {
		ids[item.ID] = item.Status == "ok"
	}
	for i, f := range report.Facts {
		if !ids[f.EvidenceID] || f.Statement == "" {
			return report, errors.New("oma runtime: ungrounded fact")
		}
		report.Facts[i].Statement = safeText(f.Statement, 2000)
	}
	if (report.Verdict == "observed_healthy" || report.Verdict == "observed_degraded") && len(report.Facts) == 0 {
		return report, errors.New("oma runtime: verdict without evidence")
	}
	report.Status = "completed"
	report.ReasonCode = ""
	if evidence.Partial || observationFault {
		report.Status = "partial"
		report.Verdict = "unknown"
		report.Summary = "证据存在未覆盖项，以下推断不代表完整业务健康：" + report.Summary
		report.EvidenceGaps = append(report.EvidenceGaps, "partial_observation_cannot_prove_business_health")
	}
	report.EvidenceGaps = append(report.EvidenceGaps, evidence.CoverageGaps...)
	report.Summary = safeText(report.Summary, 4000)
	for _, list := range [][]string{report.Inferences, report.EvidenceGaps, report.Recommendations} {
		for i := range list {
			list[i] = safeText(list[i], 2000)
		}
	}
	if report.Facts == nil {
		report.Facts = []Fact{}
	}
	if report.Inferences == nil {
		report.Inferences = []string{}
	}
	if report.EvidenceGaps == nil {
		report.EvidenceGaps = []string{}
	}
	if report.Recommendations == nil {
		report.Recommendations = []string{}
	}
	return report, nil
}
func messageObserved(events []Event, submissionID string) bool {
	for _, e := range events {
		if e.Type != "user.message" {
			continue
		}
		var content struct {
			SubmissionID string `json:"submission_id"`
		}
		if json.Unmarshal([]byte(e.Text), &content) == nil && content.SubmissionID == submissionID {
			return true
		}
	}
	return false
}

// Any abnormal termination observed before the report checkpoint overrides an earlier valid JSON/end_turn.
// We do not predict events arriving after that checkpoint or silently rewrite an immutable completed report.
func latestRound(events []Event) (text string, done bool, err error) {
	for _, e := range events {
		if e.Type == "agent.message" && strings.TrimSpace(e.Text) != "" {
			text = e.Text
		}
		if e.Type == "session.status_idle" {
			if e.StopReason == "end_turn" {
				done = true
			} else {
				err = ErrModelTerminated
			}
		}
		if e.Type == "session.error" || e.Type == "session.status_terminated" {
			err = ErrModelTerminated
		}
	}
	return text, done, err
}

func errorEvidence(ids []string) EvidenceBundle {
	out := EvidenceBundle{Schema: "sre.analysis-evidence/v1", Items: []Evidence{}, Partial: true}
	for _, id := range ids {
		raw, _ := json.Marshal(map[string]any{"query_id": id, "status": "error", "reason_code": "evidence_collection_unavailable"})
		sum := sha256.Sum256(raw)
		out.Items = append(out.Items, Evidence{ID: hex.EncodeToString(sum[:]), QueryID: id, Status: "error", ObservedAt: time.Now().UTC(), Payload: raw})
	}
	return out
}

func requiredCoverage(group GroupPayload) []string {
	out := []string{}
	for _, m := range group.Alerts {
		id := ""
		switch m.RuleID {
		case "iac-api-probe-not-all-200", "iac-api-probe-delay-high":
			id = "entry_coverage"
		case "iac-redis002-vip-ras-down":
			id = "vip_coverage"
		}
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
func includeRequiredQueries(requested, required []string) []string {
	out := append([]string{}, required...)
	for _, id := range requested {
		if len(out) < 6 && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
