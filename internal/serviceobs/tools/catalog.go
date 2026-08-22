// Package tools 是只读 Tool descriptor catalog 的边界。
//
// 本 Phase 刻意只提供 Get/Has/List：没有 Execute，也没有 query template。Profile 与
// 浏览器只能引用语义 key，真正的查询模板留在服务端 adapter 里；执行器、Evidence broker
// 与限额裁剪属于后续 Phase，需要各自的授权与审计设计。
//
// catalog 是不可变注册表：Get/List 返回深拷贝，调用方改不到共享配置。
package tools

import (
	"embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
)

//go:embed catalog.v1.json
var catalogFS embed.FS

const canonicalCatalogFile = "catalog.v1.json"

// toolKeyPattern 与 tool-descriptor.schema.json 的 key pattern 一致；这里额外把末尾
// 的语义主版本单独捕获出来，用于与 semantic_version 交叉校验。
var toolKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+\.v([1-9][0-9]*)$`)

var semanticVersionPattern = regexp.MustCompile(`^([0-9]+)\.[0-9]+\.[0-9]+$`)

// registeredSourceAdapters 是 Tool Registry 持有的 adapter allowlist。契约 schema 保持
// provider-neutral（只约束 key 形状），受控集合在服务端这一侧，因此换 Prometheus 后端或
// 换证据存储只需要调整这里与对应 adapter，不动 schema。
var registeredSourceAdapters = map[string]struct{}{
	"alert_system":          {},
	"topology":              {},
	"prometheus_compatible": {},
	"openobserve":           {},
}

var (
	// ErrToolNotFound 表示 catalog 里没有这个 key。Profile 引用不存在的 tool 会导致该
	// Profile 被 quarantine，而不是在请求时静默降级。
	ErrToolNotFound = errors.New("tools: tool descriptor 不存在")
	// ErrDuplicateToolKey 表示同一个 key + 语义主版本被注册了两次。
	ErrDuplicateToolKey = errors.New("tools: tool key 重复")
	// ErrUnregisteredSourceAdapter 表示 descriptor 指向未登记的 adapter。
	ErrUnregisteredSourceAdapter = errors.New("tools: source_adapter 未登记")
	// ErrVersionMismatch 表示 key 末尾的语义主版本与 semantic_version 的 major 不一致。
	ErrVersionMismatch = errors.New("tools: key 语义主版本与 semantic_version 不一致")
)

// Catalog 是已发布的只读 tool descriptor 集合。
type Catalog struct {
	byKey   map[string]model.ToolDescriptor
	ordered []string
}

var (
	canonicalOnce sync.Once
	canonical     *Catalog
	canonicalErr  error
)

// Canonical 返回进程内共享的 canonical catalog，只加载并校验一次。
func Canonical() (*Catalog, error) {
	canonicalOnce.Do(func() { canonical, canonicalErr = LoadCanonical() })
	return canonical, canonicalErr
}

// LoadCanonical 从 embedded catalog.v1.json 构造 catalog，并让每个 descriptor 都过一遍
// 真实 ToolDescriptor v1 schema。发布物自身不合契约时必须启动失败，不能只在测试里发现。
func LoadCanonical() (*Catalog, error) {
	raw, err := catalogFS.ReadFile(canonicalCatalogFile)
	if err != nil {
		return nil, fmt.Errorf("tools: 读取 %s 失败: %w", canonicalCatalogFile, err)
	}
	return LoadJSON(raw)
}

// CanonicalJSON 返回嵌入的 catalog 原始字节，供 schema_sync 测试与 docs 下的 canonical
// 文件逐字节比对。
func CanonicalJSON() ([]byte, error) {
	return catalogFS.ReadFile(canonicalCatalogFile)
}

// LoadJSON 解析一份 descriptor 数组并构造 catalog。
func LoadJSON(raw []byte) (*Catalog, error) {
	validator, err := contract.Default()
	if err != nil {
		return nil, err
	}
	var descriptors []model.ToolDescriptor
	decoder := strictDecoder(raw)
	if err := decoder.Decode(&descriptors); err != nil {
		return nil, fmt.Errorf("tools: 解析 tool catalog 失败: %w", err)
	}
	for i := range descriptors {
		if err := validator.ValidateValue(contract.KindToolDescriptor, descriptors[i]); err != nil {
			return nil, fmt.Errorf("tools: catalog[%d] key=%q %w", i, descriptors[i].Key, err)
		}
	}
	return New(descriptors)
}

// New 从已解析的 descriptor 构造 catalog，并执行 registry 级不变量：key 唯一、
// source_adapter 已登记、key 的语义主版本与 semantic_version 一致、read_only 必须为真。
func New(descriptors []model.ToolDescriptor) (*Catalog, error) {
	catalog := &Catalog{byKey: make(map[string]model.ToolDescriptor, len(descriptors))}
	for i, descriptor := range descriptors {
		descriptor.Normalize()
		if !descriptor.ReadOnly {
			return nil, fmt.Errorf("tools: catalog[%d] key=%q 必须是 read_only", i, descriptor.Key)
		}
		major, err := toolKeyMajor(descriptor.Key)
		if err != nil {
			return nil, fmt.Errorf("tools: catalog[%d] %w", i, err)
		}
		semanticMajor, err := semanticVersionMajor(descriptor.SemanticVersion)
		if err != nil {
			return nil, fmt.Errorf("tools: catalog[%d] key=%q %w", i, descriptor.Key, err)
		}
		if major != semanticMajor {
			return nil, fmt.Errorf("tools: catalog[%d] key=%q v%d vs semantic_version %s: %w",
				i, descriptor.Key, major, descriptor.SemanticVersion, ErrVersionMismatch)
		}
		if _, ok := registeredSourceAdapters[descriptor.SourceAdapter]; !ok {
			return nil, fmt.Errorf("tools: catalog[%d] key=%q adapter=%q: %w",
				i, descriptor.Key, descriptor.SourceAdapter, ErrUnregisteredSourceAdapter)
		}
		if _, exists := catalog.byKey[descriptor.Key]; exists {
			return nil, fmt.Errorf("tools: catalog[%d] key=%q: %w", i, descriptor.Key, ErrDuplicateToolKey)
		}
		catalog.byKey[descriptor.Key] = descriptor.Clone()
		catalog.ordered = append(catalog.ordered, descriptor.Key)
	}
	sort.Strings(catalog.ordered)
	return catalog, nil
}

// Get 返回 descriptor 的深拷贝。没有 Execute：本 Phase 的 catalog 只是元数据边界。
func (c *Catalog) Get(key string) (model.ToolDescriptor, error) {
	descriptor, ok := c.byKey[key]
	if !ok {
		return model.ToolDescriptor{}, fmt.Errorf("%w: %s", ErrToolNotFound, key)
	}
	return descriptor.Clone(), nil
}

// Has 只回答存在性，供 Profile 引用完整性校验使用。
func (c *Catalog) Has(key string) bool {
	_, ok := c.byKey[key]
	return ok
}

// List 按 key 稳定排序返回全部 descriptor 的深拷贝。
func (c *Catalog) List() []model.ToolDescriptor {
	out := make([]model.ToolDescriptor, 0, len(c.ordered))
	for _, key := range c.ordered {
		out = append(out, c.byKey[key].Clone())
	}
	return out
}

// Keys 返回稳定排序的 key 列表，供 DiagnosticContext 的 allowed_tool_keys 使用。
func (c *Catalog) Keys() []string {
	out := make([]string, len(c.ordered))
	copy(out, c.ordered)
	return out
}

// Len 返回已发布 descriptor 数量。
func (c *Catalog) Len() int { return len(c.byKey) }

// RegisteredSourceAdapters 返回排序后的 adapter allowlist，供装配自检与测试断言。
func RegisteredSourceAdapters() []string {
	out := make([]string, 0, len(registeredSourceAdapters))
	for adapter := range registeredSourceAdapters {
		out = append(out, adapter)
	}
	sort.Strings(out)
	return out
}

func toolKeyMajor(key string) (int, error) {
	match := toolKeyPattern.FindStringSubmatch(key)
	if match == nil {
		return 0, fmt.Errorf("tools: tool key %q 不符合 <domain>.<action>.vN 形状", key)
	}
	return strconv.Atoi(match[1])
}

func semanticVersionMajor(version string) (int, error) {
	match := semanticVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return 0, fmt.Errorf("tools: semantic_version %q 不是 major.minor.patch", version)
	}
	return strconv.Atoi(match[1])
}

func strictDecoder(raw []byte) *jsonDecoder {
	return newStrictDecoder(strings.NewReader(string(raw)))
}
