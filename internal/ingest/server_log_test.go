package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"alert-system/internal/domain"
)

func TestManagedHookRejectLogOmitsPayloadAndFreeTextErrors(t *testing.T) {
	const marker = "synthetic-private-marker"
	tests := []struct {
		kind, category, body, wantResponse string
		status                             int
	}{
		{alertmanagerKind, "parse_failed", `{"alerts":[{"status":"` + marker + `","annotations":{"summary":"` + marker + `"}}]}`,
			"payload 解析失败: 第 0 条告警翻译失败: 非法 status: \"" + marker + "\"\n", http.StatusBadRequest},
		{genericKind, "validation_failed", strings.Replace(validSingleBody, "critical", marker, 1),
			"第 0 条事件校验失败: 非法 severity: \"" + marker + "\"\n", http.StatusBadRequest},
		{genericKind, "enqueue_failed", validSingleBody, "internal error\n", http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.category, func(t *testing.T) {
			fs := defaultFakeStore()
			integ := fs.integrations[testIntegrationID]
			integ.Kind = tc.kind
			fs.integrations[testIntegrationID] = integ
			fs.enqueueErr = errors.New(marker)
			registry := NewRegistry()
			registry.Register(NewGenericAdapter())
			registry.Register(NewAlertmanagerAdapter(domain.SeverityWarning))
			var output bytes.Buffer
			s := NewServer(registry, fs, slog.New(slog.NewJSONHandler(&output, nil)))
			resp := doManagedHook(s, testHookID, tc.body)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != tc.wantResponse || resp.StatusCode != tc.status {
				t.Fatalf("HTTP behavior changed: %d %q (%v)", resp.StatusCode, body, err)
			}
			if strings.Contains(output.String(), marker) || strings.Contains(output.String(), testHookID) || strings.Contains(output.String(), "body_snippet") {
				t.Fatal("managed hook wrote private data to its rejection log")
			}
			var record struct {
				Reason string `json:"reason"`
				ID     string `json:"integration_id"`
				Status int    `json:"status"`
				Bytes  int    `json:"body_bytes_read"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil || record.Reason != tc.category || record.ID != testIntegrationID || record.Status != tc.status || record.Bytes != len(tc.body) {
				t.Fatalf("unexpected rejection audit fields: %+v (%v)", record, err)
			}
		})
	}
}
