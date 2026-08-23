package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// profileSchemaPath 是 Profile v1 canonical schema。
const profileSchemaPath = "../../../docs/contracts/service-observability/v1/service-observability-profile.schema.json"

// SIP 能表达的枚举必须是 Profile v1 的**子集**。
//
// 这是编译可行性的结构前提：SIP 里合法但 Profile 里非法的取值，会让一份通过 SIP 校验的
// package 在编译最后一步才失败——用户看到的是"包是对的，但编不出来"。第一次写这份 schema
// 时 aggregation 就多了 ratio/p90/last 三个值，正是被编译产物校验抓住的。这个测试把那类
// 漂移前移到 schema 层，且以后任何一侧新增枚举都会立刻失败。
func TestSIPEnumsAreSubsetOfProfileV1(t *testing.T) {
	embedded, err := EmbeddedSchema()
	if err != nil {
		t.Fatalf("读取 embedded SIP schema 失败: %v", err)
	}
	sip := loadSchema(t, embedded)
	profileSchema := loadSchema(t, readFile(t, profileSchemaPath))

	// 两侧路径不同是因为 SIP 把 thresholds/scope 提成了 $defs，而 Profile 是内联的。
	cases := []struct {
		name        string
		sipPath     string
		profilePath string
	}{
		{
			"signal family",
			"$defs/signalBinding/properties/family",
			"$defs/signalBinding/properties/family",
		},
		{
			"signal semantic",
			"$defs/signalBinding/properties/semantic",
			"$defs/signalBinding/properties/semantic",
		},
		{
			"signal aggregation",
			"$defs/signalBinding/properties/aggregation",
			"$defs/signalBinding/properties/aggregation",
		},
		{
			"threshold mode",
			"$defs/thresholds/properties/mode",
			"$defs/signalBinding/properties/thresholds/properties/mode",
		},
		{
			"threshold direction",
			"$defs/thresholds/properties/direction",
			"$defs/signalBinding/properties/thresholds/properties/direction",
		},
		{
			"scope kind",
			"$defs/signalScope/properties/kind",
			"$defs/signalBinding/properties/scope/properties/kind",
		},
		{
			"renderer",
			"$defs/view/properties/renderer",
			"$defs/view/properties/renderer",
		},
		{
			"journey stage 键形状之外的 focus 引用不涉及枚举，这里改验 relation kind 的封闭性",
			"$defs/relationSelector/properties/kind",
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sipValues := enumAt(t, sip, tc.sipPath)
			if len(sipValues) == 0 {
				t.Fatalf("SIP 路径 %s 没有取到枚举，测试路径大概写错了", tc.sipPath)
			}
			if tc.profilePath == "" {
				// 没有 Profile 对位枚举的项：只断言它确实是封闭枚举，不做子集比较。
				return
			}
			profileValues := enumAt(t, profileSchema, tc.profilePath)
			if len(profileValues) == 0 {
				t.Fatalf("Profile 路径 %s 没有取到枚举，测试路径大概写错了", tc.profilePath)
			}
			allowed := map[string]struct{}{}
			for _, value := range profileValues {
				allowed[value] = struct{}{}
			}
			extra := []string{}
			for _, value := range sipValues {
				if _, ok := allowed[value]; !ok {
					extra = append(extra, value)
				}
			}
			if len(extra) > 0 {
				sort.Strings(extra)
				t.Fatalf("SIP 允许但 Profile v1 不允许的取值: %v", extra)
			}
		})
	}
}

// SIP 的 unit 形状必须能被 Profile v1 的 unit 约束接受。
func TestSIPUnitFitsProfileConstraint(t *testing.T) {
	profileSchema := loadSchema(t, readFile(t, profileSchemaPath))
	node := nodeAt(t, profileSchema, "$defs/signalBinding/properties/unit")
	maxLength, ok := node["maxLength"].(float64)
	if !ok {
		t.Fatal("Profile unit 缺少 maxLength")
	}
	embedded, err := EmbeddedSchema()
	if err != nil {
		t.Fatalf("读取 embedded SIP schema 失败: %v", err)
	}
	sipNode := nodeAt(t, loadSchema(t, embedded), "$defs/signalBinding/properties/unit")
	sipMax, ok := sipNode["maxLength"].(float64)
	if !ok {
		t.Fatal("SIP unit 缺少 maxLength")
	}
	if sipMax > maxLength {
		t.Fatalf("SIP unit maxLength=%v 超过 Profile 的 %v", sipMax, maxLength)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return raw
}

func loadSchema(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	return doc
}

// nodeAt 按 `a/b/c` 路径下钻到一个 schema 节点。
func nodeAt(t *testing.T, schema map[string]any, path string) map[string]any {
	t.Helper()
	current := schema
	for _, segment := range strings.Split(path, "/") {
		next, ok := current[segment].(map[string]any)
		if !ok {
			t.Fatalf("路径 %s 在 %q 处断开", path, segment)
		}
		current = next
	}
	return current
}

func enumAt(t *testing.T, schema map[string]any, path string) []string {
	t.Helper()
	node := nodeAt(t, schema, path)
	raw, ok := node["enum"].([]any)
	if !ok {
		t.Fatalf("路径 %s 不是封闭枚举", path)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		value, ok := item.(string)
		if !ok {
			t.Fatalf("enum 里出现非字符串 %v", item)
		}
		out = append(out, value)
	}
	return out
}
