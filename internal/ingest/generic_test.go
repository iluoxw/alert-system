package ingest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"alert-system/internal/domain"
)

// fakeStore 是 Store 的测试替身：整合集成信息与已入队事件的记录，均在内存中，
// 无需 Docker/testcontainers 即可覆盖 handleIngest 的完整流程。
type fakeStore struct {
	integrations map[string]Integration
	enqueued     [][]domain.AlertEvent // 每次 EnqueueBatch 调用记录一批
	enqueueErr   error
}

func (f *fakeStore) GetIntegration(_ context.Context, id string) (Integration, error) {
	integ, ok := f.integrations[id]
	if !ok {
		return Integration{}, ErrIntegrationNotFound
	}
	return integ, nil
}

func (f *fakeStore) EnqueueBatch(_ context.Context, _ string, events []domain.AlertEvent) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.enqueued = append(f.enqueued, events)
	return nil
}

const testIntegrationID = "11111111-1111-1111-1111-111111111111"
const testToken = "correct-token"

// newTestServer 构造一个已注册 generic adapter、并预置一个启用中 integration 的 Server，
// 供各测试用例复用；integrations 参数为 nil 时使用默认的单个启用 integration。
func newTestServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	reg := NewRegistry()
	reg.Register(NewGenericAdapter())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(reg, fs, logger)
}

func defaultFakeStore() *fakeStore {
	return &fakeStore{
		integrations: map[string]Integration{
			testIntegrationID: {
				ID:        testIntegrationID,
				Kind:      genericKind,
				Enabled:   true,
				TokenHash: hashToken(testToken),
			},
		},
	}
}

func doIngest(srv *Server, integrationID, authHeader, body string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+integrationID, strings.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

const validSingleBody = `{
	"rule_id": "cpu-high",
	"severity": "critical",
	"status": "firing",
	"title": "CPU 使用率过高",
	"resolve_policy": "source",
	"starts_at": "2026-07-30T00:00:00Z",
	"labels": {"host": "node-1"},
	"custom_field": "extra-info"
}`

const validBatchBody = `[
	{"rule_id":"a","severity":"critical","status":"firing","title":"t1","resolve_policy":"source","starts_at":"2026-07-30T00:00:00Z"},
	{"rule_id":"b","severity":"warning","status":"firing","title":"t2","resolve_policy":"manual","starts_at":"2026-07-30T00:00:00Z"}
]`

func TestHandleIngest_AuthFailures(t *testing.T) {
	cases := []struct {
		name           string
		integrationID  string
		authHeader     string
		integrationsFn func(*fakeStore)
	}{
		{name: "缺少 token", integrationID: testIntegrationID, authHeader: ""},
		{name: "token 错误", integrationID: testIntegrationID, authHeader: "Bearer wrong-token"},
		{name: "integration 不存在", integrationID: "22222222-2222-2222-2222-222222222222", authHeader: "Bearer " + testToken},
		{
			name: "integration 已禁用", integrationID: testIntegrationID, authHeader: "Bearer " + testToken,
			integrationsFn: func(fs *fakeStore) {
				integ := fs.integrations[testIntegrationID]
				integ.Enabled = false
				fs.integrations[testIntegrationID] = integ
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := defaultFakeStore()
			if tc.integrationsFn != nil {
				tc.integrationsFn(fs)
			}
			srv := newTestServer(t, fs)

			resp := doIngest(srv, tc.integrationID, tc.authHeader, validSingleBody)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", resp.StatusCode)
			}
			if len(fs.enqueued) != 0 {
				t.Fatalf("鉴权失败时不应有事件入队，实际 %d 批", len(fs.enqueued))
			}
		})
	}
}

func TestHandleIngest_XIngestTokenHeader(t *testing.T) {
	fs := defaultFakeStore()
	srv := newTestServer(t, fs)

	req := httptest.NewRequest(http.MethodPost, "/ingest/"+testIntegrationID, strings.NewReader(validSingleBody))
	req.Header.Set("X-Ingest-Token", testToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（X-Ingest-Token 应被接受）", rec.Code)
	}
}

func TestHandleIngest_BadPayload(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "非法 JSON", body: `{not-json`},
		{name: "缺少 rule_id", body: `{"severity":"critical","status":"firing","title":"t","resolve_policy":"source","starts_at":"2026-07-30T00:00:00Z"}`},
		{name: "非法 severity", body: `{"rule_id":"a","severity":"super","status":"firing","title":"t","resolve_policy":"source","starts_at":"2026-07-30T00:00:00Z"}`},
		{name: "非法 resolve_policy", body: `{"rule_id":"a","severity":"critical","status":"firing","title":"t","resolve_policy":"nope","starts_at":"2026-07-30T00:00:00Z"}`},
		{name: "缺少 starts_at", body: `{"rule_id":"a","severity":"critical","status":"firing","title":"t","resolve_policy":"source"}`},
		{name: "批量中一条非法则整批拒绝", body: `[{"rule_id":"a","severity":"critical","status":"firing","title":"t1","resolve_policy":"source","starts_at":"2026-07-30T00:00:00Z"},{"rule_id":"","severity":"critical","status":"firing","title":"t2","resolve_policy":"source","starts_at":"2026-07-30T00:00:00Z"}]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := defaultFakeStore()
			srv := newTestServer(t, fs)

			resp := doIngest(srv, testIntegrationID, "Bearer "+testToken, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
			}
			if len(fs.enqueued) != 0 {
				t.Fatalf("校验失败时不应有事件入队，实际 %d 批", len(fs.enqueued))
			}
		})
	}
}

func TestHandleIngest_ValidSingleAndBatch(t *testing.T) {
	t.Run("单条", func(t *testing.T) {
		fs := defaultFakeStore()
		srv := newTestServer(t, fs)

		resp := doIngest(srv, testIntegrationID, "Bearer "+testToken, validSingleBody)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("状态码 = %d，期望 202", resp.StatusCode)
		}
		if len(fs.enqueued) != 1 || len(fs.enqueued[0]) != 1 {
			t.Fatalf("期望恰好 1 批 1 条事件入队，实际 %+v", fs.enqueued)
		}
		event := fs.enqueued[0][0]
		if event.Fingerprint == "" {
			t.Fatal("event.Fingerprint 应被自动补全，实际为空")
		}
		if event.SignalContext == nil {
			t.Fatal("未知字段 custom_field 应进入 SignalContext")
		}
		if !strings.Contains(string(event.SignalContext), "custom_field") {
			t.Fatalf("SignalContext 应包含 custom_field，实际 %s", event.SignalContext)
		}
	})

	t.Run("批量", func(t *testing.T) {
		fs := defaultFakeStore()
		srv := newTestServer(t, fs)

		resp := doIngest(srv, testIntegrationID, "Bearer "+testToken, validBatchBody)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("状态码 = %d，期望 202", resp.StatusCode)
		}
		if len(fs.enqueued) != 1 || len(fs.enqueued[0]) != 2 {
			t.Fatalf("期望恰好 1 批 2 条事件入队（同一事务），实际 %+v", fs.enqueued)
		}
	})
}

func TestHandleIngest_EnqueueFailureReturns500(t *testing.T) {
	fs := defaultFakeStore()
	fs.enqueueErr = context.DeadlineExceeded
	srv := newTestServer(t, fs)

	resp := doIngest(srv, testIntegrationID, "Bearer "+testToken, validSingleBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.StatusCode)
	}
}

func TestHandleIngest_OversizedBody(t *testing.T) {
	fs := defaultFakeStore()
	srv := newTestServer(t, fs)

	bigBody := `{"rule_id":"` + strings.Repeat("a", 2*1024*1024) + `"}`
	resp := doIngest(srv, testIntegrationID, "Bearer "+testToken, bigBody)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("状态码 = %d，期望 4xx", resp.StatusCode)
	}
	if len(fs.enqueued) != 0 {
		t.Fatalf("超大 body 时不应有事件入队，实际 %d 批", len(fs.enqueued))
	}
}
