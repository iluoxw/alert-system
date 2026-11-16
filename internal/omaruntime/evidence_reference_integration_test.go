//go:build integration

package omaruntime

import (
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
)

func TestRuntimeLegacyEvidenceReadRemainsUnchanged(t *testing.T) {
	st := testStore(t)
	f := setupFixture(t, st, "GenericFixture", "iac-go-traffic-zero")
	f.step(t)
	f.step(t)
	ctx := t.Context()
	var id string
	if e := st.Pool.QueryRow(ctx, `SELECT id::text FROM oma_analysis_runs WHERE role=$1`, f.role).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if e := st.WithTx(ctx, func(tx pgx.Tx) error {
		result, e := f.worker.Repo.Result(ctx, tx, id)
		if e != nil {
			return e
		}
		if len(result.Evidence) == 0 {
			t.Fatal("new evidence missing")
		}
		for _, item := range result.Evidence {
			if item.ID != "ev_"+item.QueryID {
				t.Fatal("new reference lost during persistence")
			}
			assertPayloadDigest(t, item)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	oldID := strings.Repeat("a", 64)
	legacy := `{"schema":"sre.analysis-evidence/v1","items":[{"evidence_id":"` + oldID + `","query_id":"traffic","status":"ok","observed_at":"2026-01-01T00:00:00Z","payload":{"value":0}}],"partial":false}`
	if _, e := st.Pool.Exec(ctx, `UPDATE oma_analysis_runs SET evidence=$2::jsonb WHERE id=$1`, id, legacy); e != nil {
		t.Fatal(e)
	}
	report := factReport(oldID)
	if _, e := st.Pool.Exec(ctx, `UPDATE analysis_results SET content_hash='legacy-fixture',report=$2::jsonb WHERE run_id=$1`, id, report); e != nil {
		t.Fatal(e)
	}
	if e := st.WithTx(ctx, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); e != nil {
			return e
		}
		result, e := f.worker.Repo.Result(ctx, tx, id)
		if e != nil {
			return e
		}
		if len(result.Evidence) != 1 || result.Evidence[0].ID != oldID || result.Evidence[0].SHA256 != "" {
			t.Fatal("stored legacy evidence changed")
		}
		raw, _ := json.Marshal(result.Report)
		if _, e = parseReport(string(raw), EvidenceBundle{Items: result.Evidence}, false); e != nil {
			t.Fatal("legacy report no longer grounded", e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	var after string
	if e := st.Pool.QueryRow(ctx, `SELECT evidence->'items'->0->>'evidence_id' FROM oma_analysis_runs WHERE id=$1`, id).Scan(&after); e != nil || after != oldID {
		t.Fatal("read changed stored reference", e)
	}
}
