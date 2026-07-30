package domain

import "testing"

func TestFingerprint_SameLabelsRegardlessOfOrder(t *testing.T) {
	base := AlertEvent{
		RuleID:     "high_cpu",
		SignalKind: SignalKindMetric,
		Labels: map[string]string{
			"alertname": "HighCPU",
			"instance":  "host-1",
			"job":       "node-exporter",
		},
	}

	want := Fingerprint(base)

	// map 内容相同、构造顺序不同的多个副本应得到同一指纹（内部按 key 排序消除了
	// Go map 遍历顺序的随机性）。
	for i := 0; i < 20; i++ {
		e := AlertEvent{
			RuleID:     "high_cpu",
			SignalKind: SignalKindMetric,
			Labels: map[string]string{
				"job":       "node-exporter",
				"instance":  "host-1",
				"alertname": "HighCPU",
			},
		}
		if got := Fingerprint(e); got != want {
			t.Fatalf("iteration %d: Fingerprint() = %q, want %q (should be order-independent)", i, got, want)
		}
	}
}

func TestFingerprint_LogKind_ContentLabelsIgnored(t *testing.T) {
	resourceRef := map[string]string{
		"service.name": "checkout",
		"host.name":    "host-1",
	}

	e1 := AlertEvent{
		RuleID:      "error_rate",
		SignalKind:  SignalKindLog,
		ResourceRef: resourceRef,
		Labels: map[string]string{
			"log.line": "connection refused at 10:00:01",
		},
	}
	e2 := AlertEvent{
		RuleID:      "error_rate",
		SignalKind:  SignalKindLog,
		ResourceRef: resourceRef,
		Labels: map[string]string{
			"log.line": "timeout at 10:05:32", // 日志内容变化
		},
	}

	fp1, fp2 := Fingerprint(e1), Fingerprint(e2)
	if fp1 != fp2 {
		t.Fatalf("log 类告警内容标签变化不应改变指纹: fp1=%q fp2=%q", fp1, fp2)
	}
}

func TestFingerprint_TraceKind_ContentLabelsIgnored(t *testing.T) {
	resourceRef := map[string]string{"service.name": "checkout"}

	e1 := AlertEvent{
		RuleID:      "slow_span",
		SignalKind:  SignalKindTrace,
		ResourceRef: resourceRef,
		Labels:      map[string]string{"trace_id": "abc123"},
	}
	e2 := AlertEvent{
		RuleID:      "slow_span",
		SignalKind:  SignalKindTrace,
		ResourceRef: resourceRef,
		Labels:      map[string]string{"trace_id": "def456"}, // trace_id 变化
	}

	if Fingerprint(e1) != Fingerprint(e2) {
		t.Fatal("trace 类告警内容标签变化不应改变指纹")
	}
}

func TestFingerprint_MetricKind_AnyLabelValueChangeChangesFingerprint(t *testing.T) {
	e1 := AlertEvent{
		RuleID:     "high_cpu",
		SignalKind: SignalKindMetric,
		Labels: map[string]string{
			"alertname": "HighCPU",
			"instance":  "host-1",
		},
	}
	e2 := e1
	e2.Labels = map[string]string{
		"alertname": "HighCPU",
		"instance":  "host-2", // 任一标签值变化
	}

	if Fingerprint(e1) == Fingerprint(e2) {
		t.Fatal("metric 类告警任一标签值变化应改变指纹")
	}
}

func TestFingerprint_SeparatorAmbiguity(t *testing.T) {
	// 不加分隔符直接拼接时 {"a":"bc"} 与 {"ab":"c"} 都会得到 "abc"，必须能区分。
	e1 := AlertEvent{
		RuleID:     "r",
		SignalKind: SignalKindMetric,
		Labels:     map[string]string{"a": "bc"},
	}
	e2 := AlertEvent{
		RuleID:     "r",
		SignalKind: SignalKindMetric,
		Labels:     map[string]string{"ab": "c"},
	}

	fp1, fp2 := Fingerprint(e1), Fingerprint(e2)
	if fp1 == fp2 {
		t.Fatalf("分隔符歧义用例应产生不同指纹，实际相同: %q", fp1)
	}
}

func TestFingerprint_RuleIDChangeChangesFingerprint(t *testing.T) {
	labels := map[string]string{"alertname": "HighCPU"}

	e1 := AlertEvent{RuleID: "rule_a", SignalKind: SignalKindMetric, Labels: labels}
	e2 := AlertEvent{RuleID: "rule_b", SignalKind: SignalKindMetric, Labels: labels}

	if Fingerprint(e1) == Fingerprint(e2) {
		t.Fatal("不同 RuleID 应产生不同指纹")
	}
}

func TestFingerprint_UnspecifiedSignalKindDefaultsToMetricStrategy(t *testing.T) {
	e := AlertEvent{
		RuleID: "high_cpu",
		Labels: map[string]string{"instance": "host-1"},
		ResourceRef: map[string]string{
			"instance": "host-2", // 与 Labels 不同，若误用 resource_ref 策略会得到不同结果
		},
	}
	eMetric := e
	eMetric.SignalKind = SignalKindMetric

	if Fingerprint(e) != Fingerprint(eMetric) {
		t.Fatal("未声明 SignalKind 时应等价于 metric 策略（使用 Labels 而非 ResourceRef）")
	}
}
