package domain

import "testing"

// TestNormalizeTemplate_TokenClasses 直接断言归一化后的字符串（而非哈希），
// 精确覆盖 design.md C2 列出的五类变量 token 与若干边界情况。
func TestNormalizeTemplate_TokenClasses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "IPv4",
			in:   "Ping timeout from 10.0.0.1",
			want: "Ping timeout from <*>",
		},
		{
			name: "IPv6",
			in:   "connect to 2001:db8::1 failed",
			want: "connect to <*> failed",
		},
		{
			name: "UUID小写",
			in:   "request 550e8400-e29b-41d4-a716-446655440000 failed",
			want: "request <*> failed",
		},
		{
			name: "UUID大写",
			in:   "request 550E8400-E29B-41D4-A716-446655440000 failed",
			want: "request <*> failed",
		},
		{
			name: "hex串大于等于8位",
			in:   "commit deadbeef12 merged",
			want: "commit <*> merged",
		},
		{
			name: "hex串恰好8位",
			in:   "commit deadbeef merged",
			want: "commit <*> merged",
		},
		{
			name: "hex串7位不命中",
			in:   "commit deadbee merged",
			want: "commit deadbee merged",
		},
		{
			name: "纯数字",
			in:   "restart count 3 exceeded",
			want: "restart count <*> exceeded",
		},
		{
			name: "纯数字多位",
			in:   "latency 123456 ms",
			want: "latency <*> ms",
		},
		{
			name: "混合多变量",
			in:   "host 10.0.0.1 restarted 3 times, trace 550e8400-e29b-41d4-a716-446655440000",
			want: "host <*> restarted <*> times, trace <*>",
		},
		{
			name: "无变量原样哈希",
			in:   "disk usage warning on primary node",
			want: "disk usage warning on primary node",
		},
		{
			name: "空串",
			in:   "",
			want: "",
		},
		{
			name: "纯变量整串就是一个token",
			in:   "10.0.0.1",
			want: "<*>",
		},
		{
			name: "粘连token边界_字母紧贴数字不整体替换",
			in:   "server42 rebooted",
			want: "server42 rebooted", // "server42" 混合字母数字，整体不匹配任一变量类，不做子串替换
		},
		{
			name: "粘连token边界_IP紧贴字母后缀不识别为IP",
			in:   "10.0.0.1abc unreachable",
			want: "10.0.0.1abc unreachable", // 整 token 校验失败，不会把前半段单独当 IP 替换
		},
		{
			name: "粘连token边界_数字紧贴单位不整体替换",
			in:   "queue depth 100ms exceeded",
			want: "queue depth 100ms exceeded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeTemplate(tc.in); got != tc.want {
				t.Fatalf("normalizeTemplate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTemplateFingerprint_SameTemplateDifferentVariable_SameHash 是 R2 的核心
// 验收点：只差变量的同模板文本必须归一到同一哈希。
func TestTemplateFingerprint_SameTemplateDifferentVariable_SameHash(t *testing.T) {
	fp1 := TemplateFingerprint("Ping timeout from 10.0.0.1")
	fp2 := TemplateFingerprint("Ping timeout from 10.0.0.2")
	fp3 := TemplateFingerprint("Ping timeout from 10.0.0.3")

	if fp1 != fp2 || fp2 != fp3 {
		t.Fatalf("同模板不同变量应产生相同哈希: fp1=%q fp2=%q fp3=%q", fp1, fp2, fp3)
	}
}

func TestTemplateFingerprint_DifferentTemplate_DifferentHash(t *testing.T) {
	fp1 := TemplateFingerprint("Ping timeout from 10.0.0.1")
	fp2 := TemplateFingerprint("Disk full on 10.0.0.1")

	if fp1 == fp2 {
		t.Fatalf("不同模板应产生不同哈希，实际相同: %q", fp1)
	}
}

func TestTemplateFingerprint_PureVariable_SameHashAcrossDifferentValues(t *testing.T) {
	// 整条文本本身就是一个变量 token（无固定前后缀），归一化后都是同一个
	// placeholder，哈希也应相同。
	fp1 := TemplateFingerprint("10.0.0.1")
	fp2 := TemplateFingerprint("550e8400-e29b-41d4-a716-446655440000")

	if fp1 != fp2 {
		t.Fatalf("纯变量文本归一化后应产生相同哈希: fp1=%q fp2=%q", fp1, fp2)
	}
}

func TestTemplateFingerprint_NoVariable_Deterministic(t *testing.T) {
	const text = "disk usage warning on primary node"
	fp1 := TemplateFingerprint(text)
	fp2 := TemplateFingerprint(text)

	if fp1 != fp2 {
		t.Fatalf("同输入应产生同输出（确定性）: fp1=%q fp2=%q", fp1, fp2)
	}
}

func TestTemplateFingerprint_EmptyString_Deterministic(t *testing.T) {
	fp1 := TemplateFingerprint("")
	fp2 := TemplateFingerprint("")

	if fp1 != fp2 {
		t.Fatalf("空串应产生确定性哈希: fp1=%q fp2=%q", fp1, fp2)
	}
	if fp1 == "" {
		t.Fatal("空串的指纹本身不应为空字符串（哈希值恒为 16 位十六进制）")
	}
}

func TestTemplateFingerprint_Determinism_RepeatedCalls(t *testing.T) {
	const text = "Ping timeout from 10.0.0.1 and trace 550e8400-e29b-41d4-a716-446655440000"
	want := TemplateFingerprint(text)
	for i := 0; i < 50; i++ {
		if got := TemplateFingerprint(text); got != want {
			t.Fatalf("iteration %d: TemplateFingerprint() = %q, want %q", i, got, want)
		}
	}
}
