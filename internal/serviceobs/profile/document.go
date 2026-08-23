package profile

import (
	"context"
	"errors"

	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
)

// 本文件把 Registry 的校验链暴露成可复用入口。
//
// 动机很具体：Phase 3 的 Service Integration Package compiler 必须让编译产物走**完全相同**
// 的校验，而不是自己重写一份"应该等价"的规则。两份规则一旦漂移，就会出现"compiler 说合法、
// Registry 拒绝加载"或者更糟的反向情况。
//
// 这里没有新增任何校验语义：DocumentValidator 调用的就是 Registry 内部用的
// decodeProfile / validateStructure / validateToolRefs / resolveReferences。

// ToolCatalog 是 Profile 引用完整性需要的 catalog 能力子集。
// *tools.Catalog 直接满足它，测试可以给一个最小 fake。
type ToolCatalog interface {
	Has(key string) bool
}

// DocumentValidator 对一份完整 Profile 文档执行 Registry 的校验链。
//
// 它是无状态的：构造后可并发使用。
type DocumentValidator struct {
	validator *contract.Validator
	tools     ToolCatalog
}

// NewDocumentValidator 构造校验器。两个依赖都必填：缺 validator 会让 schema 校验被跳过，
// 缺 catalog 会让 Profile 引用到未发布的 tool。
func NewDocumentValidator(validator *contract.Validator, catalog ToolCatalog) (*DocumentValidator, error) {
	if validator == nil {
		return nil, ErrValidatorRequired
	}
	if catalog == nil {
		return nil, ErrToolsRequired
	}
	return &DocumentValidator{validator: validator, tools: catalog}, nil
}

// ValidateDocument 执行不依赖数据库的全部校验：unknown-field、v1 schema、递归禁止键、
// 结构与引用完整性、golden 投影，以及 tool_refs 必须已发布。
//
// 它刻意不解析 service/root 引用：那一步需要 resolver 与数据库，由 ValidateReferences
// 单独提供，好让"文档本身是否合法"与"这个环境里的 ID 是否存在"保持可分辨。
func (v *DocumentValidator) ValidateDocument(raw []byte) error {
	parsed, err := decodeProfile(v.validator, raw)
	if err != nil {
		return err
	}
	if err := validateStructure(parsed); err != nil {
		return err
	}
	return validateToolRefs(parsed, v.tools)
}

// ValidateToolKeys 只校验一组 tool key 是否全部已发布。
//
// SIP compiler 在生成 Profile **之前**就需要这个判断：一份引用了未发布 tool 的 package
// 应该在编译阶段就被拒绝，而不是先编出一个必然被 quarantine 的 Profile 再报错。
func (v *DocumentValidator) ValidateToolKeys(keys []string) error {
	for _, key := range keys {
		if !v.tools.Has(key) {
			return failf(ReasonToolNotPublished, "tool_refs 引用未发布的 %s", key)
		}
	}
	return nil
}

// ValidateReferences 执行 Registry 的外部引用解析：service ID 存在且唯一、root entity
// 存在、同环境（或 global）、且从 service entity 经 declared 关系有界可达。
//
// 返回值语义与 Registry 内部一致且必须被区别对待：
//   - configErr 非 nil：文档配置错误（该 Profile/package 不可发布）；
//   - fatal 非 nil：基础设施错误（resolver/DB 不可用），调用方不能把它当成"文档非法"。
//
// 两者互斥。maxDepth <= 0 时使用 DefaultMaxReachableDepth。
func ValidateReferences(
	ctx context.Context, resolver Resolver, parsed model.Profile, maxDepth int,
) (configErr error, fatal error) {
	if resolver == nil {
		return nil, ErrResolverRequired
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxReachableDepth
	}
	return resolveReferences(ctx, resolver, parsed, maxDepth)
}

// ReasonCodeOf 提取 typed reason code，便于调用方与测试按稳定 code 断言而不是匹配中文。
func ReasonCodeOf(err error) string {
	var failure *validationFailure
	if errors.As(err, &failure) {
		return failure.ReasonCode
	}
	return ""
}
