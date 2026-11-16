package omaruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPModelWirePaginationAndSubscription(t *testing.T) {
	now := time.Now().UTC()
	event := func(id, kind, text, stop string) map[string]any {
		return map[string]any{"id": id, "type": kind, "processed_at": now, "content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": map[string]string{"type": stop}}
	}
	var createBody map[string]any
	var sentBody map[string]any
	streamCursor := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "managed-agents-2026-04-01" {
			t.Error("missing required API headers")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/sessions" && r.Method == "POST":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "sesn_test"})
		case r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "sesn_test", "title": "sre-analysis:run", "environment_id": "env_test", "metadata": map[string]string{"run_id": "run"}}}, "next_page": nil})
		case strings.HasSuffix(r.URL.Path, "/events/stream"):
			streamCursor = r.Header.Get("Last-Event-ID")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(": connected\n\n"))
			raw, _ := json.Marshal(event("event-stream", "agent.message", `{"schema":"sre.analysis-intent/v1","query_ids":["sources"],"rationale":"probe"}`, ""))
			_, _ = w.Write(append(append([]byte("id: event-stream\ndata: "), raw...), []byte("\n\n")...))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == "POST":
			_ = json.NewDecoder(r.Body).Decode(&sentBody)
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			if r.URL.Query().Get("order") != "asc" {
				t.Error("events not ordered for replay")
			}
			if r.URL.Query().Get("page") == "two" {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{event("event-idle", "session.status_idle", "", "end_turn")}, "next_page": nil})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{event("event-one", "user.message", `{"submission_id":"run:submit_evidence"}`, "")}, "next_page": "two"})
			}
		}
	}))
	defer server.Close()
	model, e := NewHTTPModel(HTTPModelConfig{BaseURL: server.URL, APIKey: "test-key", AgentID: "agent_test", EnvironmentID: "env_test", AllowHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	session, e := model.Create(ctx, SessionRequest{RunID: "run", SubmissionID: "run:create_session", Input: json.RawMessage(`{"a":1}`)})
	if e != nil || session != "sesn_test" {
		t.Fatal(session, e)
	}
	agent := createBody["agent"].(map[string]any)
	if len(agent["tools"].([]any)) != 0 || agent["type"] != "agent_with_overrides" || createBody["metadata"].(map[string]any)["run_id"] != "run" {
		t.Fatal("unsafe session creation")
	}
	found, ok, e := model.Find(ctx, "run", now)
	if e != nil || !ok || found != session {
		t.Fatal(found, ok, e)
	}
	if e = model.Send(ctx, session, Message{SubmissionID: "run:submit_evidence", Input: json.RawMessage(`{"data":[]}`)}); e != nil {
		t.Fatal(e)
	}
	if len(sentBody["events"].([]any)) != 1 {
		t.Fatal("bad second event body")
	}
	events, e := model.Events(ctx, session)
	if e != nil || len(events) != 2 || events[1].StopReason != "end_turn" || !messageObserved(events, "run:submit_evidence") {
		t.Fatal(events, e)
	}
	connected, received := false, false
	e = model.Subscribe(ctx, session, "prior-cursor", func(event Event) error {
		if event.Type == "runtime.stream_connected" {
			connected = true
			return nil
		}
		received = event.ID == "event-stream"
		return ErrRoundDone
	})
	if !errors.Is(e, ErrRoundDone) || !connected || !received || streamCursor != "prior-cursor" {
		t.Fatal("stream frame/cursor failed", e)
	}
}
func TestHTTPModelDoesNotRedirectCredentials(t *testing.T) {
	leaked := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("x-api-key") != ""
		w.WriteHeader(200)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer server.Close()
	model, e := NewHTTPModel(HTTPModelConfig{BaseURL: server.URL, APIKey: "secret-test", AgentID: "agent", EnvironmentID: "env", AllowHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = model.Create(t.Context(), SessionRequest{}); e == nil || leaked {
		t.Fatal("redirect accepted or credentials leaked")
	}
}
func TestStructuredIntentAndReportBoundaries(t *testing.T) {
	for _, text := range []string{`{"schema":"sre.analysis-intent/v1","query_ids":["shell"],"rationale":"x"}`, `{"schema":"sre.analysis-intent/v1","query_ids":["sources"],"rationale":"x","url":"http://bad"}`, `{"schema":"sre.analysis-intent/v1","query_ids":["sources","sources"],"rationale":"x"}`} {
		if _, e := parseIntent(text, []string{"sources"}); e == nil {
			t.Fatal("unsafe intent accepted")
		}
	}
	evidence := EvidenceBundle{Items: []Evidence{{ID: "good", Status: "ok"}, {ID: "bad", Status: "unknown"}}, Partial: true}
	input := `{"schema":"sre.analysis-result/v1","verdict":"observed_healthy","summary":"current sample","facts":[{"evidence_id":"good","statement":"value observed"}],"inferences":[],"evidence_gaps":[],"recommendations":[]}`
	report, e := parseReport(input, evidence, false)
	if e != nil || report.Status != "partial" || report.Verdict != "unknown" {
		t.Fatal(report, e)
	}
	if _, e = parseReport(strings.Replace(input, `"good"`, `"bad"`, 1), evidence, false); e == nil {
		t.Fatal("unknown evidence accepted as fact")
	}
	ids := includeRequiredQueries([]string{"a", "b", "c", "d", "e", "f"}, []string{"entry_coverage"})
	if len(ids) != 6 || ids[0] != "entry_coverage" {
		t.Fatal(ids)
	}
}
