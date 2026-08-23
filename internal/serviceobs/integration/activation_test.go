package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"alert-system/internal/serviceobs/model"
)

// ---- 激活身份的三方交叉核对 ----
//
// manifest、CompiledIntegration 与实际 Profile 三处重复记录同一批激活身份字段。这份冗余
// 唯一的用处就是让"只改了其中一处"暴露出来，所以每个字段都要有一条对抗测试证明它真的
// 被核对了——否则冗余只是三份可以各说各话的副本。

// 编译产出的激活单元必须自洽通过校验，作为下面所有篡改用例的对照基线。
func TestCompiledBundlePassesActivationIdentity(t *testing.T) {
	if err := ValidateBundle(compileBundle(t)); err != nil {
		t.Fatalf("正常编译的激活单元必须通过校验: %v", err)
	}
}

// 只改 manifest 把 draft 提权成 published 必须失败。
//
// 这是最要紧的一条：compiler 有 AllowPublished 门，但那道门只在**编译时**生效。激活时
// 如果只信 manifest 自称的 package_status，改一个字段就能让 draft package 以 published
// 身份进生产，而且两份产物的摘要都还对得上——因为产物本身一个字节都没动。抓住它的办法是
// 拿实际 Profile 文档里的 status 反过来核对 manifest。
func TestManifestOnlyStatusEscalationIsRejected(t *testing.T) {
	bundle := compileBundle(t)
	if bundle.Manifest.PackageStatus != StatusDraft {
		t.Fatalf("前置条件：基线应为 draft，实际 %q", bundle.Manifest.PackageStatus)
	}

	bundle.Manifest.PackageStatus = StatusPublished
	err := ValidateBundle(bundle)
	if !errors.Is(err, ErrBundleIdentityMismatch) {
		t.Fatalf("err=%v，want ErrBundleIdentityMismatch", err)
	}

	// 连 CompiledIntegration 一起改也不行：实际 Profile 产物仍然是 draft。
	bundle = compileBundle(t)
	bundle.Manifest.PackageStatus = StatusPublished
	bundle.Integration = remarshalIntegration(t, &bundle, func(c *CompiledIntegration) {
		c.PackageStatus = StatusPublished
	})
	if err := ValidateBundle(bundle); !errors.Is(err, ErrBundleIdentityMismatch) {
		t.Fatalf("两处一起改也必须被实际 Profile status 抓住，err=%v", err)
	}
}

// 逐个字段篡改 manifest：每一项都必须被抓住，且错误里点名的是那个字段。
func TestManifestIdentityFieldTamperingIsRejected(t *testing.T) {
	cases := map[string]struct {
		tamper func(*CompileManifest)
		field  string
	}{
		"compiler_version": {
			func(m *CompileManifest) { m.CompilerVersion += 99 }, "compiler_version",
		},
		"package_key": {
			func(m *CompileManifest) { m.PackageKey = "other.service" }, "package_key",
		},
		"package_version": {
			func(m *CompileManifest) { m.PackageVersion++ }, "package_version",
		},
		"environment": {
			func(m *CompileManifest) { m.Environment = "staging" }, "environment",
		},
		"service_id": {
			func(m *CompileManifest) { m.ServiceID = strings.Repeat("f", 36) }, "service_id",
		},
		"service_entity_id": {
			func(m *CompileManifest) { m.ServiceEntityID = strings.Repeat("e", 36) },
			"service_entity_id",
		},
		"root_entity_ids 被替换": {
			func(m *CompileManifest) { m.RootEntityIDs = []string{strings.Repeat("d", 36)} },
			"root_entity_ids",
		},
		"root_entity_ids 被截断": {
			func(m *CompileManifest) { m.RootEntityIDs = nil }, "root_entity_ids",
		},
		"profile_key": {
			func(m *CompileManifest) { m.ProfileKey = "svc.other" }, "profile_key",
		},
		"profile_version": {
			func(m *CompileManifest) { m.ProfileVersion += 7 }, "profile_version",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := compileBundle(t)
			tc.tamper(&bundle.Manifest)
			err := ValidateBundle(bundle)
			if !errors.Is(err, ErrBundleIdentityMismatch) {
				t.Fatalf("err=%v，want ErrBundleIdentityMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("错误应点名字段 %q，实际 %v", tc.field, err)
			}
		})
	}
}

// 篡改 CompiledIntegration 后重算摘要：摘要重新对上了，只能靠身份交叉核对抓。
//
// 这是比"摘要错配"更危险的形状：能改产物的人当然也能重算摘要，所以摘要链只能证明
// 传输完整，证明不了内容没被换。
func TestTamperedIntegrationWithRecomputedDigestIsRejected(t *testing.T) {
	cases := map[string]struct {
		tamper func(*CompiledIntegration)
		field  string
	}{
		"service_id": {
			func(c *CompiledIntegration) { c.ServiceID = strings.Repeat("f", 36) }, "service_id",
		},
		"service_entity_id": {
			func(c *CompiledIntegration) { c.ServiceEntityID = strings.Repeat("e", 36) },
			"service_entity_id",
		},
		"root_entity_ids": {
			func(c *CompiledIntegration) { c.RootEntityIDs = []string{strings.Repeat("d", 36)} },
			"root_entity_ids",
		},
		"compiler_version": {
			func(c *CompiledIntegration) { c.CompilerVersion -= 1 }, "compiler_version",
		},
		"profile_key": {
			func(c *CompiledIntegration) { c.ProfileKey = "svc.other" }, "profile_key",
		},
		"profile_digest": {
			func(c *CompiledIntegration) { c.ProfileDigest = "sha256:" + strings.Repeat("a", 64) },
			"profile_digest",
		},
		"schema_version": {
			func(c *CompiledIntegration) { c.SchemaVersion = "sip-compiled/v99" },
			"schema_version",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := compileBundle(t)
			bundle.Integration = remarshalIntegration(t, &bundle, tc.tamper)
			err := ValidateBundle(bundle)
			if !errors.Is(err, ErrBundleIdentityMismatch) {
				t.Fatalf("err=%v，want ErrBundleIdentityMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("错误应点名字段 %q，实际 %v", tc.field, err)
			}
		})
	}
}

// 换掉实际 Profile 文档的身份字段（并重算摘要）也必须失败。
//
// Profile 是 Registry 真正会加载的那份文档；如果只核对 manifest 与 CompiledIntegration，
// 就等于让两份 server 侧记录互相背书，而页面加载的是第三份没人核对过的文档。
func TestTamperedProfileDocumentIsRejected(t *testing.T) {
	cases := map[string]struct {
		tamper func(map[string]any)
		field  string
	}{
		"profile_key": {
			func(doc map[string]any) { doc["profile_key"] = "svc.other" }, "profile.profile_key",
		},
		"profile_version": {
			func(doc map[string]any) { doc["profile_version"] = float64(99) },
			"profile.profile_version",
		},
		"status 被提权": {
			func(doc map[string]any) { doc["status"] = string(model.ProfileStatusPublished) },
			"profile.status",
		},
		"service_id": {
			func(doc map[string]any) {
				doc["service_ref"].(map[string]any)["service_id"] = strings.Repeat("f", 36)
			},
			"profile.service_ref.service_id",
		},
		"root_entity_ids": {
			func(doc map[string]any) {
				doc["service_ref"].(map[string]any)["root_entity_ids"] =
					[]any{strings.Repeat("d", 36)}
			},
			"profile.service_ref.root_entity_ids",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := compileBundle(t)
			var doc map[string]any
			if err := json.Unmarshal(bundle.Profile, &doc); err != nil {
				t.Fatalf("解析 Profile 失败: %v", err)
			}
			tc.tamper(doc)
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}
			bundle.Profile = raw
			bundle.Manifest.ProfileDigest = digest(raw)
			bundle.Integration = remarshalIntegration(t, &bundle, func(c *CompiledIntegration) {
				c.ProfileDigest = bundle.Manifest.ProfileDigest
			})

			err = ValidateBundle(bundle)
			if !errors.Is(err, ErrBundleIdentityMismatch) {
				t.Fatalf("err=%v，want ErrBundleIdentityMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("错误应点名字段 %q，实际 %v", tc.field, err)
			}
		})
	}
}

// ---- 严格解析 ----

// 未知字段与尾随 JSON 都必须拒绝，且错误不泄露产物内容。
func TestStrictDecodeRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	const marker = "s3cret-marker-value"
	cases := map[string]func(raw []byte) []byte{
		"CompiledIntegration 未知字段": func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("{"),
				[]byte(`{"smuggled_note":"`+marker+`",`), 1)
		},
		"CompiledIntegration 尾随 JSON": func(raw []byte) []byte {
			return append(append([]byte(nil), raw...),
				[]byte(`{"smuggled_note":"`+marker+`"}`)...)
		},
		"CompiledIntegration 尾随裸 token": func(raw []byte) []byte {
			return append(append([]byte(nil), raw...), []byte(` 42`)...)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := compileBundle(t)
			bundle.Integration = mutate(bundle.Integration)
			bundle.Manifest.IntegrationDigest = digest(bundle.Integration)

			err := ValidateBundle(bundle)
			if !errors.Is(err, ErrBundleInvalidArtifact) {
				t.Fatalf("err=%v，want ErrBundleInvalidArtifact", err)
			}
			// 错误会进启动日志与 quarantine 记录，不能把产物内容原样带出来。
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("错误泄露了产物内容: %v", err)
			}
		})
	}
}

// Profile 侧的尾随 JSON 同样必须拒绝。
func TestStrictDecodeRejectsTrailingJSONInProfile(t *testing.T) {
	bundle := compileBundle(t)
	bundle.Profile = append(append([]byte(nil), bundle.Profile...), []byte(`{"x":1}`)...)
	bundle.Manifest.ProfileDigest = digest(bundle.Profile)
	bundle.Integration = remarshalIntegration(t, &bundle, func(c *CompiledIntegration) {
		c.ProfileDigest = bundle.Manifest.ProfileDigest
	})
	if err := ValidateBundle(bundle); !errors.Is(err, ErrBundleInvalidArtifact) {
		t.Fatalf("err=%v，want ErrBundleInvalidArtifact", err)
	}
}

// ---- 回滚引用成对 ----

// 激活时也要挡半对回滚引用，不能只靠编译期。
//
// 编译期的检查只覆盖"这一份 manifest 是我们自己生成的"，而激活入口还会收到从库里读出来、
// 可能被别的代码路径写过的 manifest。
func TestValidateBundleRejectsHalfPairedPreviousDigests(t *testing.T) {
	cases := map[string]func(*CompileManifest){
		"只有 PreviousProfileDigest": func(m *CompileManifest) {
			m.PreviousProfileDigest = "sha256:" + strings.Repeat("a", 64)
		},
		"只有 PreviousIntegrationDigest": func(m *CompileManifest) {
			m.PreviousIntegrationDigest = "sha256:" + strings.Repeat("b", 64)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := compileBundle(t)
			tamper(&bundle.Manifest)
			if err := ValidateBundle(bundle); !errors.Is(err, ErrBundlePreviousDigestUnpaired) {
				t.Fatalf("err=%v，want ErrBundlePreviousDigestUnpaired", err)
			}
		})
	}

	// 成对存在是合法的回滚引用形态。
	bundle := compileBundle(t)
	bundle.Manifest.PreviousProfileDigest = "sha256:" + strings.Repeat("a", 64)
	bundle.Manifest.PreviousIntegrationDigest = "sha256:" + strings.Repeat("b", 64)
	if err := ValidateBundle(bundle); err != nil {
		t.Fatalf("成对回滚引用应通过: %v", err)
	}
}

// ---- 测试辅助 ----

func compileBundle(t *testing.T) Bundle {
	t.Helper()
	compiled, err := newCompiler(t, e2bResolver()).Compile(
		context.Background(), CompileRequest{Raw: readPackage(t, "e2b.package.json")})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	return compiled.Bundle()
}

// remarshalIntegration 篡改 CompiledIntegration 后重新序列化并同步 manifest 摘要，
// 用来构造"摘要对得上但内容被换"的攻击形状。
func remarshalIntegration(
	t *testing.T, bundle *Bundle, tamper func(*CompiledIntegration),
) []byte {
	t.Helper()
	var compiled CompiledIntegration
	if err := json.Unmarshal(bundle.Integration, &compiled); err != nil {
		t.Fatalf("解析 CompiledIntegration 失败: %v", err)
	}
	tamper(&compiled)
	raw, err := json.Marshal(compiled)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	bundle.Manifest.IntegrationDigest = digest(raw)
	return raw
}
