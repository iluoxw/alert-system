//go:build integration

package omaruntime

import (
	"alert-system/internal/iacbroker"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestRuntimeCCloudCapabilityRequiredOnlyForEnabledCanary(t *testing.T) {
	st := testStore(t)
	for _, scenario := range []string{"disabled", "ok", "not_configured", "not_allowed", "datasource_error"} {
		t.Run(scenario, func(t *testing.T) {
			name := "IAC-SRE-OMA-Canary"
			if scenario == "datasource_error" {
				name = "DatasourceError"
			}
			f := setupFixture(t, st, name, "iac-sre-oma-canary")
			enabled := scenario != "disabled"
			f.worker.Options.CCloudL4 = enabled
			f.worker.Options.QueryProfile = iacbroker.Profile
			if enabled {
				f.worker.Options.QueryProfile = iacbroker.ProfileWithCCloud
			}
			if scenario != "not_allowed" {
				f.worker.Options.QueryIDs = append(f.worker.Options.QueryIDs, "ccloud_l4")
			}
			collected := []string{}
			f.worker.Collect = func(ctx context.Context, ids []string) (json.RawMessage, error) {
				collected = append([]string{}, ids...)
				raw, e := goodCollector(ctx, ids)
				if e != nil {
					return nil, e
				}
				if scenario == "not_configured" {
					var bundle map[string]any
					_ = json.Unmarshal(raw, &bundle)
					for _, v := range bundle["items"].([]any) {
						item := v.(map[string]any)
						if item["query_id"] == "ccloud_l4" {
							item["status"] = "unknown"
							item["reason_code"] = "ccloud_not_configured"
						}
					}
					return json.Marshal(bundle)
				}
				return raw, nil
			}
			f.step(t)
			f.step(t)
			run := f.result(t)
			required := scenario != "disabled" && scenario != "datasource_error" && scenario != "not_allowed"
			if slices.Contains(collected, "ccloud_l4") != required || len(collected) > 6 {
				t.Fatal("deterministic collection mismatch", collected)
			}
			var constraints []string
			var requestRaw []byte
			if e := st.Pool.QueryRow(t.Context(), `SELECT request->'constraints' FROM oma_analysis_actions WHERE run_id=$1 AND stage='create_session'`, run.Run.ID).Scan(&requestRaw); e != nil {
				t.Fatal(e)
			}
			if e := json.Unmarshal(requestRaw, &constraints); e != nil {
				t.Fatal(e)
			}
			wantConstraint := enabled && scenario != "datasource_error"
			if strings.Contains(strings.Join(constraints, " "), "ccloud_l4") != wantConstraint {
				t.Fatal("canary instruction capability drift")
			}
			wantPartial := scenario == "not_configured" || scenario == "not_allowed" || scenario == "datasource_error"
			if (run.Report.Status == "partial") != wantPartial {
				t.Fatal("unexpected completion", run.Report.Status, run.Report.ReasonCode)
			}
			if scenario == "not_allowed" && !slices.Contains(run.Report.EvidenceGaps, "required_query_not_allowed:ccloud_l4") {
				t.Fatal("missing explicit coverage gap")
			}
			if scenario == "datasource_error" {
				for _, id := range collected {
					if id != "sources" && id != "freshness" {
						t.Fatal("datasource failure expanded native scope")
					}
				}
			}
		})
	}
}
