package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLarkWebhookChannelSendSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		var body larkWebhookRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.MsgType != "text" {
			t.Errorf("msg_type = %q", body.MsgType)
		}
		if !strings.Contains(body.Content.Text, "[SRE Control] [CRITICAL] CPU 过载") {
			t.Errorf("text = %q", body.Content.Text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"StatusCode":0,"StatusMessage":"success","code":0,"msg":"success"}`))
	}))
	defer server.Close()

	channel := NewLarkWebhookChannel(time.Second)
	err := channel.Send(context.Background(), server.URL, Message{
		Severity: "critical",
		Title:    "CPU 过载",
		Body:     "CPU 使用率超过 95%",
		Labels:   map[string]string{"env": "e2e"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

func TestLarkWebhookChannelRejectsBusinessErrorOnHTTP200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":19002,"msg":"params error"}`))
	}))
	defer server.Close()

	channel := NewLarkWebhookChannel(time.Second)
	err := channel.Send(context.Background(), server.URL, Message{Title: "test"})
	if err == nil || !strings.Contains(err.Error(), "19002") {
		t.Fatalf("Send() error = %v, want business error", err)
	}
}

func TestLarkWebhookChannelRejectsNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	channel := NewLarkWebhookChannel(time.Second)
	err := channel.Send(context.Background(), server.URL, Message{Title: "test"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Send() error = %v, want HTTP status error", err)
	}
}
