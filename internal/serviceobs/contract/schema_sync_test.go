// 双份定义防漂移：internal/serviceobs/contract/schemas/ 与 tools/catalog.v1.json 只是
// docs/contracts/service-observability/v1/ 的生成镜像。一旦有人只改一侧，本文件立刻失败，
// 避免"Go 校验通过但 python verifier 不认"这种最难查的分裂。
package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const canonicalDir = "../../../docs/contracts/service-observability/v1"

func TestEmbeddedSchemasAreByteIdenticalToCanonical(t *testing.T) {
	for _, kind := range SchemaKinds() {
		name, ok := EmbeddedSchemaFileName(kind)
		if !ok {
			t.Fatalf("kind %s 没有对应文件名", kind)
		}
		embedded, err := EmbeddedSchema(kind)
		if err != nil {
			t.Fatalf("读取 embedded %s 失败: %v", name, err)
		}
		canonical, err := os.ReadFile(filepath.Join(canonicalDir, name))
		if err != nil {
			t.Fatalf("读取 canonical %s 失败: %v", name, err)
		}
		if !bytes.Equal(embedded, canonical) {
			t.Fatalf("%s 与 canonical 不一致：镜像必须逐字节相同，请重新复制而不是手改", name)
		}
	}
}

// 逐字节相同已经覆盖内容，这里额外确认规范化 JSON 也一致，这样即使未来允许格式差异
// （例如换行风格），语义漂移仍会被抓住。
func TestEmbeddedSchemasAreSemanticallyIdentical(t *testing.T) {
	for _, kind := range SchemaKinds() {
		name, _ := EmbeddedSchemaFileName(kind)
		embedded, err := EmbeddedSchema(kind)
		if err != nil {
			t.Fatalf("读取 embedded %s 失败: %v", name, err)
		}
		canonical, err := os.ReadFile(filepath.Join(canonicalDir, name))
		if err != nil {
			t.Fatalf("读取 canonical %s 失败: %v", name, err)
		}
		var embeddedDoc, canonicalDoc any
		if err := json.Unmarshal(embedded, &embeddedDoc); err != nil {
			t.Fatalf("解析 embedded %s 失败: %v", name, err)
		}
		if err := json.Unmarshal(canonical, &canonicalDoc); err != nil {
			t.Fatalf("解析 canonical %s 失败: %v", name, err)
		}
		if !reflect.DeepEqual(embeddedDoc, canonicalDoc) {
			t.Fatalf("%s 规范化 JSON 不一致", name)
		}
	}
}

// canonical 目录里新增一份 schema 但没有同步进 embedded 镜像同样是漂移。
func TestEveryCanonicalSchemaIsEmbedded(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join(canonicalDir, "*.schema.json"))
	if err != nil {
		t.Fatalf("枚举 canonical schema 失败: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("canonical 目录里没有 schema，路径大概写错了: %s", canonicalDir)
	}
	embedded := map[string]struct{}{}
	for _, kind := range SchemaKinds() {
		name, _ := EmbeddedSchemaFileName(kind)
		embedded[name] = struct{}{}
	}
	if len(embedded) != len(matches) {
		t.Fatalf("embedded schema 数量=%d canonical=%d，两侧必须一一对应", len(embedded), len(matches))
	}
	for _, match := range matches {
		name := filepath.Base(match)
		if _, ok := embedded[name]; !ok {
			t.Fatalf("canonical %s 没有对应的 embedded 镜像", name)
		}
	}
}
