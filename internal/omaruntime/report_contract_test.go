package omaruntime

import (
	"encoding/json"
	"testing"
)

func TestReportStringArraysRejectObjectsAndAcceptGroundedStrings(t *testing.T) {
	evidence := EvidenceBundle{Items: []Evidence{{ID: "observed", Status: "ok"}}}
	valid := map[string]any{"schema": ResultSchema, "verdict": "observed_healthy", "summary": "合成链路验收：本次观测无系统错误，不代表历史故障或完整业务健康。", "facts": []any{map[string]any{"evidence_id": "observed", "statement": "该时刻的系统错误率样本为零。"}}, "inferences": []any{"仅能说明本次观测未见系统错误。"}, "evidence_gaps": []any{"缺少入口端到端观测。"}, "recommendations": []any{"由负责人只读复核趋势。"}}
	raw, e := json.Marshal(valid)
	if e != nil {
		t.Fatal(e)
	}
	report, e := parseReport(string(raw), evidence, false)
	if e != nil || report.Status != "completed" || len(report.Inferences) != 1 || report.Inferences[0] != valid["inferences"].([]any)[0] {
		t.Fatalf("valid string arrays rejected: %+v %v", report, e)
	}
	for _, field := range []string{"inferences", "evidence_gaps", "recommendations"} {
		t.Run(field, func(t *testing.T) {
			invalid := make(map[string]any, len(valid))
			for k, v := range valid {
				invalid[k] = v
			}
			invalid[field] = []any{map[string]any{"statement": "对象不是该字段的字符串元素。"}}
			raw, e := json.Marshal(invalid)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = parseReport(string(raw), evidence, false); e == nil {
				t.Fatalf("object accepted in %s", field)
			}
		})
	}
}
