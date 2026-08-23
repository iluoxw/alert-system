package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 本文件覆盖 SIP 的安全边界：package 必须无法携带自由查询、可执行代码、凭据、endpoint、
// 上传 renderer，也无法用 label 参数夹带 regex/否定/集合选择器。
//
// 每个 case 都从**合法** fixture 出发只改一处，因此失败原因唯一。

func compileMutated(t *testing.T, apply func(map[string]any)) error {
	t.Helper()
	mutated := mutate(t, readPackage(t, "e2b.package.json"), apply)
	_, err := newCompiler(t, e2bResolver()).Compile(context.Background(), CompileRequest{Raw: mutated})
	return err
}

// 顶层与嵌套的禁止键都必须被拒绝。
//
// 注意期望的 reason 是 sip_schema_invalid 而不是 sip_forbidden_key：schema 每一层都是
// additionalProperties=false，所以这些键连 schema 都过不了。递归键扫描是第二道闸门，
// 由 TestForbiddenKeyScannerIsSecondGate 单独验证。
func TestForbiddenKeysAreRejectedBySchema(t *testing.T) {
	cases := []struct {
		name  string
		apply func(map[string]any)
	}{
		{"顶层 query", func(doc map[string]any) { doc["query"] = "up" }},
		{"顶层 promql", func(doc map[string]any) { doc["promql"] = "rate(x[5m])" }},
		{"顶层 endpoint", func(doc map[string]any) { doc["endpoint"] = "https://prom.internal" }},
		{"顶层 token", func(doc map[string]any) { doc["token"] = "abc" }},
		{"顶层 headers", func(doc map[string]any) { doc["headers"] = map[string]any{"Authorization": "Bearer x"} }},
		{"顶层 prompt", func(doc map[string]any) { doc["prompt"] = "you are an SRE" }},
		{"顶层 html", func(doc map[string]any) { doc["html"] = "<div/>" }},
		{"顶层 executable", func(doc map[string]any) { doc["executable"] = "L3Vzci9iaW4vc2g=" }},
		{
			"params 里塞 sql",
			func(doc map[string]any) {
				bindings := doc["provider_bindings"].([]any)
				bindings[0].(map[string]any)["params"].(map[string]any)["sql"] = "SELECT 1"
			},
		},
		{
			"params 里塞 matcher",
			func(doc map[string]any) {
				bindings := doc["provider_bindings"].([]any)
				bindings[0].(map[string]any)["params"].(map[string]any)["matcher"] = `{job=~".*"}`
			},
		},
		{
			"signal binding 里塞 secret",
			func(doc map[string]any) {
				bindings := profileTemplate(doc)["signal_bindings"].([]any)
				bindings[0].(map[string]any)["secret"] = "s3cr3t"
			},
		},
		{
			"topology_policy 里塞 url",
			func(doc map[string]any) {
				doc["topology_policy"].(map[string]any)["url"] = "http://evil.example"
			},
		},
		{
			"label 项里塞 regex",
			func(doc map[string]any) {
				bindings := doc["provider_bindings"].([]any)
				labels := bindings[0].(map[string]any)["params"].(map[string]any)["labels"].([]any)
				labels[0].(map[string]any)["regex"] = ".*"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compileMutated(t, tc.apply)
			code := ReasonCodeOf(err)
			if code != ReasonSchemaInvalid && code != ReasonUnknownField {
				t.Fatalf("reason=%q err=%v，禁止键必须被 schema/unknown-field 拒绝", code, err)
			}
		})
	}
}

// 递归键扫描是纵深防御：即使将来某层 schema 被放开，它仍然 fail closed。
//
// 直接测 scanner 而不是走 schema：schema 已经会先拒绝，因此只有单独调用 scanner 才能
// 证明第二道闸门真的有效。
func TestForbiddenKeyScannerIsSecondGate(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"顶层", `{"query":"up"}`, "/query"},
		{"嵌套对象", `{"target":{"secret":"x"}}`, "/target/secret"},
		{"数组元素内", `{"provider_bindings":[{"params":{"promql":"x"}}]}`, "/provider_bindings/0/params/promql"},
		{"大小写不敏感", `{"Authorization":"Bearer x"}`, "/Authorization"},
		{"深层 endpoint", `{"a":{"b":{"c":{"endpoint":"http://x"}}}}`, "/a/b/c/endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var generic any
			if err := json.Unmarshal([]byte(tc.doc), &generic); err != nil {
				t.Fatalf("测试文档不是合法 JSON: %v", err)
			}
			path, found := scanForbiddenKeys(generic, "")
			if !found {
				t.Fatal("应命中禁止键")
			}
			if path != tc.want {
				t.Fatalf("path=%q, want %q", path, tc.want)
			}
		})
	}

	// 合法键不能被误伤：value_pattern 是 focus 输入校验用的正则，与查询构造无关。
	legit := `{"profile_template":{"focus_kinds":[{"value_pattern":"^sbx_[A-Za-z0-9]+$"}]},
	           "signal_bindings":[{"proxy_reason":"没有直接 saturation 指标，这里用使用率做 proxy"}]}`
	var generic any
	if err := json.Unmarshal([]byte(legit), &generic); err != nil {
		t.Fatalf("测试文档不是合法 JSON: %v", err)
	}
	if path, found := scanForbiddenKeys(generic, ""); found {
		t.Fatalf("合法文档被误判，命中 %s", path)
	}
}

// label 参数只能是等值字面量：regex、否定、集合选择器与任何 matcher 语法都必须在 schema
// 层面被字符集挡住，而不是靠 adapter 后面再"清理"。
func TestLabelValuesCannotExpressMatchers(t *testing.T) {
	hostile := []string{
		".*",         // regex 通配
		"prod.*",     // 前缀 regex
		"a|b",        // 集合/或
		"!=prod",     // 否定
		"=~prod",     // regex 匹配符
		`{job="x"}`,  // 完整 matcher
		`x"} or up{`, // 闭合注入
		"x\\",        // 反斜杠转义
		"has space",  // 空白
		"x,y",        // 逗号分隔
		"x)or(1",     // 括号
		"$1",         // 模板替换
		"x\ny",       // 换行
	}
	for _, value := range hostile {
		t.Run(strings.ReplaceAll(value, "\n", "\\n"), func(t *testing.T) {
			err := compileMutated(t, func(doc map[string]any) {
				bindings := doc["provider_bindings"].([]any)
				labels := bindings[0].(map[string]any)["params"].(map[string]any)["labels"].([]any)
				labels[0].(map[string]any)["value"] = value
			})
			if code := ReasonCodeOf(err); code != ReasonSchemaInvalid {
				t.Fatalf("value=%q reason=%q err=%v，必须被 schema 拒绝", value, code, err)
			}
		})
	}

	// 反向控制：真实标签值（含点、冒号、斜杠、加号、@）必须继续可用。
	for _, value := range []string{"e2b-api", "prod.example.com", "job:name", "a/b", "v1+2", "svc@host", "10.60.199.160"} {
		t.Run("accept "+value, func(t *testing.T) {
			err := compileMutated(t, func(doc map[string]any) {
				bindings := doc["provider_bindings"].([]any)
				labels := bindings[0].(map[string]any)["params"].(map[string]any)["labels"].([]any)
				labels[0].(map[string]any)["value"] = value
			})
			if err != nil {
				t.Fatalf("value=%q 应被接受，err=%v", value, err)
			}
		})
	}
}

// metric 槽位只接受 Prometheus metric 名的字符集，因此塞不进表达式。
func TestMetricSlotsRejectExpressions(t *testing.T) {
	hostile := []string{
		"rate(up[5m])",
		"up{job=\"x\"}",
		"up + up",
		"sum by (a) (up)",
		"up offset 5m",
		"up;drop",
		"up /* comment */",
		"up\nup",
	}
	for _, value := range hostile {
		t.Run(strings.ReplaceAll(value, "\n", "\\n"), func(t *testing.T) {
			err := compileMutated(t, func(doc map[string]any) {
				bindings := doc["provider_bindings"].([]any)
				for _, item := range bindings {
					binding := item.(map[string]any)
					if binding["template_ref"] == "counter_rate.v1" {
						binding["params"].(map[string]any)["metric"] = value
						return
					}
				}
				t.Fatal("fixture 里应存在 counter_rate.v1 binding")
			})
			if code := ReasonCodeOf(err); code != ReasonSchemaInvalid {
				t.Fatalf("metric=%q reason=%q err=%v，必须被 schema 拒绝", value, code, err)
			}
		})
	}
}

// renderer 只能从服务端已实现的枚举里选，package 上传不了前端代码。
func TestRendererCannotBeUploaded(t *testing.T) {
	for _, renderer := range []string{"custom_react", "<script>alert(1)</script>", "golden_overview_v2", ""} {
		t.Run("renderer="+renderer, func(t *testing.T) {
			err := compileMutated(t, func(doc map[string]any) {
				views := profileTemplate(doc)["views"].([]any)
				views[0].(map[string]any)["renderer"] = renderer
			})
			if code := ReasonCodeOf(err); code != ReasonSchemaInvalid {
				t.Fatalf("renderer=%q reason=%q err=%v，必须被 schema 拒绝", renderer, code, err)
			}
		})
	}
}

// 形状非法的 entity_key 必须被 schema 拒绝。
func TestSelectorRejectsMalformedEntityKeys(t *testing.T) {
	for _, key := range []string{
		"PLATFORM:E2B",   // 大写
		"a",              // 太短
		"platform:e2b ",  // 尾随空格
		"platform:e2b\n", // 换行
		"platform e2b",   // 空格
		"-platform:e2b",  // 首字符非法
		`platform:e2b"`,  // 引号
	} {
		t.Run("entity_key="+strings.ReplaceAll(key, "\n", "\\n"), func(t *testing.T) {
			err := compileMutated(t, func(doc map[string]any) {
				doc["target"].(map[string]any)["service_selector"].(map[string]any)["entity_key"] = key
			})
			if code := ReasonCodeOf(err); code != ReasonSchemaInvalid {
				t.Fatalf("entity_key=%q reason=%q err=%v，必须被 schema 拒绝", key, code, err)
			}
		})
	}
}

// UUID 形状的 entity_key **语法上**是合法 entity_key（数字与连字符都在字符集内），schema
// 因此不会拒绝它。真正的保证在别处：compiler 永远不把 selector 的值当成 ID 用，一律交给
// resolver 查。所以一个写成 UUID 的 selector 只会 fail closed，不会变成"直接使用真实 ID"。
func TestUUIDShapedSelectorIsStillResolvedNotUsedAsID(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	resolver := e2bResolver()
	// 这个 UUID 恰好就是 resolver 里 platform:e2b 的真实 ServiceID：如果 compiler 有任何
	// "看起来像 ID 就直接用"的捷径，这个用例会编译成功。
	err := compileMutated(t, func(doc map[string]any) {
		doc["target"].(map[string]any)["service_selector"].(map[string]any)["entity_key"] = uuid
	})
	if code := ReasonCodeOf(err); code != ReasonSelectorNotFound {
		t.Fatalf("reason=%q err=%v，UUID 形状的 selector 必须走 resolver 并 fail closed", code, err)
	}
	_ = resolver
}

// 文档大小上界必须先于 JSON 解析生效。
func TestOversizedDocumentIsRejectedBeforeParsing(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	oversized := make([]byte, MaxDocumentBytes+1)
	for i := range oversized {
		oversized[i] = ' '
	}
	_, err = validator.Decode(oversized)
	if code := ReasonCodeOf(err); code != ReasonDocumentTooLarge {
		t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonDocumentTooLarge)
	}
}

// 未知字段必须被 unknown-field 闸门拒绝，且错误里只出现字段名、不回显文档正文。
func TestUnknownFieldLeaksOnlyFieldName(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatalf("构造 validator 失败: %v", err)
	}
	raw := mutate(t, readPackage(t, "e2b.package.json"), func(doc map[string]any) {
		doc["totally_new_field"] = "sensitive-looking-value"
	})
	_, err = validator.Decode(raw)
	if code := ReasonCodeOf(err); code != ReasonUnknownField {
		t.Fatalf("reason=%q err=%v，want %s", code, err, ReasonUnknownField)
	}
	if strings.Contains(err.Error(), "sensitive-looking-value") {
		t.Fatalf("错误信息泄露了字段值: %v", err)
	}
}

// 校验错误只输出稳定位置，不回显文档正文。
func TestSchemaErrorDoesNotEchoDocument(t *testing.T) {
	err := compileMutated(t, func(doc map[string]any) {
		bindings := doc["provider_bindings"].([]any)
		labels := bindings[0].(map[string]any)["params"].(map[string]any)["labels"].([]any)
		labels[0].(map[string]any)["value"] = "leak-me-please .*"
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if strings.Contains(err.Error(), "leak-me-please") {
		t.Fatalf("错误信息泄露了文档内容: %v", err)
	}
}

// registry 自身的不变量：每个模板的 Required/Optional 槽位不重叠，Outputs 非空，
// 且 openobserve 侧的 access 模板必须要求 logs.read（access 记录可能含 raw payload）。
func TestTemplateRegistryInvariants(t *testing.T) {
	for _, ref := range TemplateRefs() {
		template, ok := LookupTemplate(ref)
		if !ok {
			t.Fatalf("%s 应存在", ref)
		}
		if len(template.Outputs) == 0 {
			t.Fatalf("%s 没有声明输出字段", ref)
		}
		if template.RequiredPermission == "" {
			t.Fatalf("%s 没有声明所需权限", ref)
		}
		seen := map[ParamSlot]struct{}{}
		for _, slot := range append(append([]ParamSlot{}, template.Required...), template.Optional...) {
			if _, exists := seen[slot]; exists {
				t.Fatalf("%s 的槽位 %s 同时出现在 Required 与 Optional", ref, slot)
			}
			seen[slot] = struct{}{}
		}
		switch template.Capability {
		case CapabilityPrometheus, CapabilityOpenObserve:
		default:
			t.Fatalf("%s 的 capability=%q 不在 allowlist 内", ref, template.Capability)
		}
	}

	access, ok := LookupTemplate("access_by_focus.v1")
	if !ok {
		t.Fatal("access_by_focus.v1 应存在")
	}
	if access.RequiredPermission != PermissionLogsRead {
		t.Fatalf("access_by_focus.v1 要求 %q，除非输出 schema 能保证不含 raw payload，否则必须是 %q",
			access.RequiredPermission, PermissionLogsRead)
	}

	// registry 不得包含任何查询文本：Template 只描述参数契约与输出字段，真正的 PromQL/SQL
	// 构造属于 Task 4/5 的 adapter。这里用反射遍历全部字符串字段，因此将来有人往结构里加
	// 一个装查询的字段会立刻失败——比只看 String() 输出可靠。
	//
	// 检查的是查询**语法**元字符与 SQL 关键字，不是"histogram_quantile"这类合法模板名词。
	queryMetachars := []string{"(", ")", "{", "}", `"`, "'", ";", "\\", "$", "|", "~", "=~"}
	sqlKeywords := []string{"SELECT ", "select ", "FROM ", "from ", "WHERE ", "where "}
	for _, ref := range TemplateRefs() {
		template, _ := LookupTemplate(ref)
		for name, value := range templateStringFields(template) {
			for _, metachar := range queryMetachars {
				if strings.Contains(value, metachar) {
					t.Fatalf("%s 的字段 %s 含查询元字符 %q: %q", ref, name, metachar, value)
				}
			}
			for _, keyword := range sqlKeywords {
				if strings.Contains(value, keyword) {
					t.Fatalf("%s 的字段 %s 含 SQL 关键字 %q: %q", ref, name, keyword, value)
				}
			}
		}
	}
}

// templateStringFields 反射收集 Template 的全部字符串与字符串切片字段。
func templateStringFields(template Template) map[string]string {
	out := map[string]string{}
	value := reflect.ValueOf(template)
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldValue := value.Field(i)
		switch fieldValue.Kind() {
		case reflect.String:
			out[field.Name] = fieldValue.String()
		case reflect.Slice:
			for j := 0; j < fieldValue.Len(); j++ {
				item := fieldValue.Index(j)
				if item.Kind() == reflect.String {
					out[fmt.Sprintf("%s[%d]", field.Name, j)] = item.String()
				}
			}
		}
	}
	return out
}

// 未注册的 template ref 一律 fail closed。
func TestUnknownTemplateRefFailsClosed(t *testing.T) {
	if _, ok := LookupTemplate("definitely_not_registered.v1"); ok {
		t.Fatal("未注册的 ref 不应命中")
	}
	if _, ok := LookupTemplate(""); ok {
		t.Fatal("空 ref 不应命中")
	}
}
