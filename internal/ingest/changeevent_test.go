package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"alert-system/internal/store"
)

// fakeChangeEventStore 是 ChangeEventStore 的测试替身：记录每次 Insert 调用，
// 无需 Docker/testcontainers 即可覆盖 changeEventHandler 的完整流程（对齐
// generic_test.go 里 fakeStore 的做法）。
type fakeChangeEventStore struct {
	inserted  []store.ChangeEvent
	insertErr error
}

func (f *fakeChangeEventStore) Insert(_ context.Context, event store.ChangeEvent) (*store.ChangeEvent, error) {
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	f.inserted = append(f.inserted, event)
	return &event, nil
}

const changeEventTestToken = "change-event-token"

func newTestChangeEventHandler(fs *fakeChangeEventStore) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewChangeEventHandler(fs, changeEventTestToken, logger)
}

func doChangeEvent(h http.Handler, authHeader, body string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "/change-events", strings.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

const validChangeEventBody = `{
	"kind": "deploy",
	"resource_ref": {"service": "checkout", "environment": "prod"},
	"payload": {"version": "1.2.3", "operator": "alice"},
	"occurred_at": "2026-07-30T08:00:00Z"
}`

func TestChangeEventHandler_Success(t *testing.T) {
	fs := &fakeChangeEventStore{}
	h := newTestChangeEventHandler(fs)

	resp := doChangeEvent(h, "Bearer "+changeEventTestToken, validChangeEventBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", resp.StatusCode)
	}
	if len(fs.inserted) != 1 {
		t.Fatalf("入库次数 = %d，期望 1", len(fs.inserted))
	}

	got := fs.inserted[0]
	if got.Kind != "deploy" {
		t.Errorf("Kind = %q，期望 deploy", got.Kind)
	}
	// resource_ref 键名按 OTel semantic conventions 规范化：
	// "service" -> "service.name"，"environment" -> "deployment.environment"。
	want := map[string]string{"service.name": "checkout", "deployment.environment": "prod"}
	if len(got.ResourceRef) != len(want) {
		t.Fatalf("ResourceRef = %v，期望 %v", got.ResourceRef, want)
	}
	for k, v := range want {
		if got.ResourceRef[k] != v {
			t.Errorf("ResourceRef[%q] = %q，期望 %q", k, got.ResourceRef[k], v)
		}
	}

	var payload map[string]string
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload 反序列化失败: %v", err)
	}
	if payload["version"] != "1.2.3" || payload["operator"] != "alice" {
		t.Errorf("Payload 未原样保留: %v", payload)
	}

	wantOccurredAt := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	if !got.OccurredAt.Equal(wantOccurredAt) {
		t.Errorf("OccurredAt = %v，期望 %v", got.OccurredAt, wantOccurredAt)
	}
}

func TestChangeEventHandler_ResourceRefAlreadyOTelKeys_NotDropped(t *testing.T) {
	// 上游若已经直接使用 OTel 键名投递（不含任何 normalizeResourceRef 认识的别名），
	// 规范化结果为空时应回退为原始 resource_ref，而不是整体丢空。
	fs := &fakeChangeEventStore{}
	h := newTestChangeEventHandler(fs)

	body := `{
		"kind": "config_change",
		"resource_ref": {"service.name": "checkout", "custom.tag": "x"},
		"occurred_at": "2026-07-30T08:00:00Z"
	}`

	resp := doChangeEvent(h, "Bearer "+changeEventTestToken, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", resp.StatusCode)
	}
	if len(fs.inserted) != 1 {
		t.Fatalf("入库次数 = %d，期望 1", len(fs.inserted))
	}
	got := fs.inserted[0].ResourceRef
	if got["service.name"] != "checkout" || got["custom.tag"] != "x" {
		t.Errorf("ResourceRef 被意外丢弃: %v", got)
	}
}

func TestChangeEventHandler_OccurredAtDefaultsToNow(t *testing.T) {
	fs := &fakeChangeEventStore{}
	h := newTestChangeEventHandler(fs)

	before := time.Now()
	body := `{"kind": "scale_out", "resource_ref": {"service": "checkout"}}`
	resp := doChangeEvent(h, "Bearer "+changeEventTestToken, body)
	after := time.Now()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", resp.StatusCode)
	}
	got := fs.inserted[0].OccurredAt
	if got.Before(before) || got.After(after) {
		t.Errorf("OccurredAt = %v，期望落在 [%v, %v] 之间", got, before, after)
	}
}

func TestChangeEventHandler_AuthFailures(t *testing.T) {
	cases := []struct {
		name       string
		authHeader string
	}{
		{name: "缺少 token", authHeader: ""},
		{name: "token 错误", authHeader: "Bearer wrong-token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeChangeEventStore{}
			h := newTestChangeEventHandler(fs)

			resp := doChangeEvent(h, tc.authHeader, validChangeEventBody)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", resp.StatusCode)
			}
			if len(fs.inserted) != 0 {
				t.Fatalf("鉴权失败时不应入库，实际 %d 条", len(fs.inserted))
			}
		})
	}
}

func TestChangeEventHandler_EmptyToken_AlwaysRejects(t *testing.T) {
	// token 未配置（空字符串）时，即便请求方恰好也不带 Authorization 头以外的凭据，
	// 也必须视为鉴权失败，而不是放行为"无需鉴权"。
	fs := &fakeChangeEventStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewChangeEventHandler(fs, "", logger)

	resp := doChangeEvent(h, "Bearer anything", validChangeEventBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", resp.StatusCode)
	}
}

func TestChangeEventHandler_ValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "kind 为空", body: `{"resource_ref": {"service": "checkout"}}`},
		{name: "resource_ref 为空", body: `{"kind": "deploy", "resource_ref": {}}`},
		{name: "resource_ref 缺失", body: `{"kind": "deploy"}`},
		{name: "非法 JSON", body: `not-json`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeChangeEventStore{}
			h := newTestChangeEventHandler(fs)

			resp := doChangeEvent(h, "Bearer "+changeEventTestToken, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
			}
			if len(fs.inserted) != 0 {
				t.Fatalf("校验失败时不应入库，实际 %d 条", len(fs.inserted))
			}
		})
	}
}

func TestChangeEventHandler_StoreError(t *testing.T) {
	fs := &fakeChangeEventStore{insertErr: errors.New("db unavailable")}
	h := newTestChangeEventHandler(fs)

	resp := doChangeEvent(h, "Bearer "+changeEventTestToken, validChangeEventBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.StatusCode)
	}
}

func TestChangeEventHandler_MethodNotAllowed(t *testing.T) {
	fs := &fakeChangeEventStore{}
	h := newTestChangeEventHandler(fs)

	req := httptest.NewRequest(http.MethodGet, "/change-events", nil)
	req.Header.Set("Authorization", "Bearer "+changeEventTestToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
}
