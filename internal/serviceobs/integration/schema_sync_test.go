package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// docsDir 是 SIP canonical 契约目录。embedded 镜像必须与它逐字节等价。
const docsDir = "../../../docs/contracts/service-integration-package/v1"

// embedded schema 与 canonical 文件漂移会让"文档说的"和"binary 执行的"不是一回事。
func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	embedded, err := EmbeddedSchema()
	if err != nil {
		t.Fatalf("读取 embedded schema 失败: %v", err)
	}
	canonical, err := os.ReadFile(filepath.Join(docsDir, "service-integration-package.schema.json"))
	if err != nil {
		t.Fatalf("读取 canonical schema 失败: %v", err)
	}
	if string(embedded) != string(canonical) {
		t.Fatal("embedded SIP schema 与 canonical 文件不一致")
	}
	var embeddedDoc, canonicalDoc any
	if err := json.Unmarshal(embedded, &embeddedDoc); err != nil {
		t.Fatalf("embedded schema 不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal(canonical, &canonicalDoc); err != nil {
		t.Fatalf("canonical schema 不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(embeddedDoc, canonicalDoc) {
		t.Fatal("规范化 JSON 不一致")
	}
}

// canonical 目录里的 schema 数量必须与 embedded 一一对应：新增一份没同步的 schema 同样是漂移。
func TestCanonicalDirHasExactlyOneSchema(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join(docsDir, "*.schema.json"))
	if err != nil {
		t.Fatalf("枚举 canonical schema 失败: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("canonical schema 数量=%d，SIP v1 只应有一份", len(matches))
	}
}

// schema 必须递归地把每一层对象都关成 additionalProperties=false。
//
// 这是 SIP 最重要的结构不变量：只要有一层被放开，任意键（包括查询、凭据、prompt）就能
// 合法地出现在 package 里。测试直接遍历 schema 文档本身，因此新增 $defs 时忘记收紧会立刻失败。
func TestEverySchemaObjectIsClosed(t *testing.T) {
	raw, err := EmbeddedSchema()
	if err != nil {
		t.Fatalf("读取 embedded schema 失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}

	var open []string
	var walk func(node any, path string)
	walk = func(node any, path string) {
		object, ok := node.(map[string]any)
		if !ok {
			if list, isList := node.([]any); isList {
				for i, item := range list {
					walk(item, path+"/"+itoa(i))
				}
			}
			return
		}
		// 只检查真正描述对象的 schema 节点：有 properties 的一定是对象 schema。
		if _, hasProps := object["properties"]; hasProps {
			additional, present := object["additionalProperties"]
			closed := present && additional == false
			if !closed {
				open = append(open, path)
			}
		}
		for key, value := range object {
			walk(value, path+"/"+key)
		}
	}
	walk(doc, "")
	if len(open) > 0 {
		t.Fatalf("以下 schema 对象没有 additionalProperties=false: %v", open)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}
