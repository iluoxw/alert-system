package openobserve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"alert-system/internal/serviceobs/integration"
	"alert-system/internal/serviceobs/model"
)

const evidenceSchemaVersion = "1.0"

func buildLogEvidenceRefs(
	binding integration.CompiledBinding,
	serviceID string,
	focus *model.Focus,
	hits []map[string]json.RawMessage,
	readAt time.Time,
) []model.EvidenceRef {
	return buildRecordEvidenceRefs(binding, serviceID, focus, hits, readAt, model.EvidenceKindLog, "log")
}

func buildAccessEvidenceRefs(
	binding integration.CompiledBinding,
	serviceID string,
	focus *model.Focus,
	hits []map[string]json.RawMessage,
	readAt time.Time,
) []model.EvidenceRef {
	return buildRecordEvidenceRefs(binding, serviceID, focus, hits, readAt, model.EvidenceKindLog, "access")
}

func buildTraceEvidenceRefs(
	binding integration.CompiledBinding,
	serviceID string,
	focus *model.Focus,
	hits []map[string]json.RawMessage,
	readAt time.Time,
) []model.EvidenceRef {
	return buildRecordEvidenceRefs(binding, serviceID, focus, hits, readAt, model.EvidenceKindTrace, "trace")
}

func buildRecordEvidenceRefs(
	binding integration.CompiledBinding,
	serviceID string,
	focus *model.Focus,
	hits []map[string]json.RawMessage,
	readAt time.Time,
	kind model.EvidenceKind,
	typeLabel string,
) []model.EvidenceRef {
	refs := make([]model.EvidenceRef, 0, len(hits))
	for _, hit := range hits {
		observedAt := recordTimestamp(hit)
		if observedAt.IsZero() {
			continue
		}
		status := ""
		if binding.Params.StatusField != "" {
			status = fieldString(hit, binding.Params.StatusField)
		}
		recordID := fieldString(hit, "trace_id")
		if recordID == "" {
			recordID = fieldString(hit, "span_id")
		}
		digest := recordDigest(binding.Params.Stream, typeLabel, observedAt, hit, binding.Params.IdentityMappings)
		if recordID == "" {
			recordID = digest
		}
		summary := fmt.Sprintf("%s record at %s status=%s digest=%s",
			typeLabel,
			model.NewTimestamp(observedAt),
			redactStatus(status),
			digest[:12],
		)
		ref := model.EvidenceRef{
			SchemaVersion: evidenceSchemaVersion,
			EvidenceID:    "ev_" + digest[:16],
			Source:        model.EvidenceSourceOpenObserve,
			Kind:          kind,
			Locator: model.EvidenceLocator{
				Provider:         "openobserve",
				Dataset:          binding.Params.Stream,
				RecordID:         recordID,
				QueryTemplateRef: binding.TemplateRef,
			},
			Correlation: model.EvidenceCorrelation{
				ServiceID: serviceID,
				Focus:     evidenceFocus(focus),
			},
			ObservedAt:   model.NewTimestamp(observedAt),
			FetchedAt:    model.NewTimestamp(readAt),
			Freshness:    model.FreshnessFresh,
			Availability: model.EvidenceAvailabilityAvailable,
			Summary:      summary,
			Integrity: &model.EvidenceIntegrity{
				Algorithm: "sha256",
				Digest:    digest,
			},
			Sensitivity: model.SensitivityInternal,
			Redactions:  []string{"body", "headers", "raw_attributes"},
		}
		ref.Normalize()
		refs = append(refs, ref)
	}
	sort.SliceStable(refs, func(i, j int) bool {
		return refs[i].EvidenceID < refs[j].EvidenceID
	})
	return refs
}

func evidenceFocus(focus *model.Focus) *model.EvidenceFocus {
	if focus == nil {
		return nil
	}
	// 只保留 kind，不写入 focus 原文到 EvidenceRef。
	return &model.EvidenceFocus{Kind: focus.Kind}
}

func redactStatus(status string) string {
	if status == "" {
		return "unknown"
	}
	return status
}

func recordDigest(
	stream, typeLabel string,
	observedAt time.Time,
	hit map[string]json.RawMessage,
	mappings []integration.IdentityMapping,
) string {
	parts := []string{stream, typeLabel, observedAt.UTC().Format(time.RFC3339Nano)}
	for _, mapping := range mappings {
		parts = append(parts, mapping.Namespace, string(mapping.Role), fieldString(hit, mapping.Field))
	}
	for _, field := range []string{"trace_id", "span_id", "operation_name", "service_name"} {
		if value := fieldString(hit, field); value != "" {
			parts = append(parts, field, value)
		}
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(h[:])
}
