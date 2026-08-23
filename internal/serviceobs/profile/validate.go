package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"alert-system/internal/serviceobs/contract"
	"alert-system/internal/serviceobs/model"
)

// 稳定 reason code。它们进 quarantine 记录、启动日志与总览的配置错误提示，所以只能
// 增加不能改写；前端按 code 本地化文案，不解析中文说明。
const (
	ReasonReadFailed              = "profile_read_failed"
	ReasonTooLarge                = "profile_too_large"
	ReasonNotRegularJSONFile      = "profile_not_regular_json_file"
	ReasonInvalidJSON             = "profile_invalid_json"
	ReasonUnknownField            = "profile_unknown_field"
	ReasonSchemaInvalid           = "profile_schema_invalid"
	ReasonForbiddenKey            = "profile_forbidden_key"
	ReasonInvalidFocusPattern     = "profile_invalid_focus_pattern"
	ReasonPrimaryFocusNotUnique   = "profile_primary_focus_not_unique"
	ReasonDuplicateKey            = "profile_duplicate_key"
	ReasonDanglingRef             = "profile_dangling_ref"
	ReasonInvalidThreshold        = "profile_invalid_threshold"
	ReasonProxyReasonMissing      = "profile_proxy_reason_missing"
	ReasonGoldenBindingInvalid    = "profile_golden_binding_invalid"
	ReasonToolNotPublished        = "profile_tool_not_published"
	ReasonRefNotFound             = "profile_ref_not_found"
	ReasonRefAmbiguous            = "profile_ref_ambiguous"
	ReasonRootUnreachable         = "profile_root_unreachable"
	ReasonRootEnvironmentMismatch = "profile_root_environment_mismatch"
	ReasonDuplicatePublished      = "profile_duplicate_published"
)

// forbiddenKeys 是 Profile 文档里绝对不允许出现的键名。Profile 是声明式配置，
// 携带查询、凭据、URL header、prompt 或可执行代码就等于把它变成脚本插件。
//
// 这是纵深防御：v1 的 Profile schema 每一层都是 additionalProperties=false，所以
// 正常情况下这些键已经被 schema 拒绝。保留递归扫描是为了将来某个 schema 分支被放开
// （或新增未收紧的对象）时仍然 fail closed。
//
// 只匹配键名，不对合法说明文字做 substring 猜测——proxy_reason 里出现"查询"两个字
// 是完全正常的。
var forbiddenKeys = map[string]struct{}{
	"query": {}, "promql": {}, "sql": {}, "logql": {}, "shell": {}, "secret": {},
	"token": {}, "headers": {}, "prompt": {}, "html": {}, "executable": {},
}

// validationFailure 是单个 Profile 文档的结构性校验失败。它带稳定 reason code 与
// 只含定位信息的说明，绝不带文档正文。
type validationFailure struct {
	ReasonCode string
	Detail     string
}

func (e *validationFailure) Error() string {
	return fmt.Sprintf("%s: %s", e.ReasonCode, e.Detail)
}

func failf(reasonCode, format string, args ...any) *validationFailure {
	return &validationFailure{ReasonCode: reasonCode, Detail: fmt.Sprintf(format, args...)}
}

// decodeProfile 执行"unknown-field + schema"双重拒绝，并在 schema 通过后递归扫描
// forbidden key。返回的 Profile 已经是结构合法的文档，但 service/root/tool 引用尚未解析。
func decodeProfile(validator *contract.Validator, raw []byte) (model.Profile, error) {
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return model.Profile{}, failf(ReasonInvalidJSON, "文档不是合法 JSON")
	}

	// unknown-field 拒绝：Go model 是 v1 的镜像，出现它不认识的字段说明文档超出契约。
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var profile model.Profile
	if err := decoder.Decode(&profile); err != nil {
		return model.Profile{}, failf(ReasonUnknownField, "文档字段超出 v1 契约: %s", unknownFieldDetail(err))
	}

	if err := validator.ValidateJSON(contract.KindProfile, raw); err != nil {
		var validationErr *contract.ValidationError
		if errors.As(err, &validationErr) {
			return model.Profile{}, failf(ReasonSchemaInvalid, "违规位置 %s", strings.Join(validationErr.Paths(), ","))
		}
		return model.Profile{}, failf(ReasonSchemaInvalid, "schema 校验失败")
	}

	if path, found := scanForbiddenKeys(generic, ""); found {
		return model.Profile{}, failf(ReasonForbiddenKey, "禁止的键 %s", path)
	}
	profile.Normalize()
	return profile, nil
}

// unknownFieldDetail 只保留 encoding/json 报出的字段名，不回显整段文档。
func unknownFieldDetail(err error) string {
	const marker = "unknown field "
	message := err.Error()
	if index := strings.Index(message, marker); index >= 0 {
		return message[index+len(marker):]
	}
	return "解码失败"
}

// scanForbiddenKeys 递归遍历已解码的 JSON，返回第一个命中的 JSON Pointer 路径。
func scanForbiddenKeys(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			if _, forbidden := forbiddenKeys[strings.ToLower(key)]; forbidden {
				return childPath, true
			}
			if found, ok := scanForbiddenKeys(typed[key], childPath); ok {
				return found, true
			}
		}
	case []any:
		for i, item := range typed {
			if found, ok := scanForbiddenKeys(item, fmt.Sprintf("%s/%d", path, i)); ok {
				return found, true
			}
		}
	}
	return "", false
}

// toolCatalog 是 Registry 需要的 catalog 能力子集，等同于导出的 ToolCatalog。
// 用别名而不是重复声明：两处方法集不可能漂移。
type toolCatalog = ToolCatalog

// validateStructure 执行 Task 3 校验顺序里的第 5、6 步：focus/journey/view/signal 的
// 键唯一性、引用完整性、阈值合法性、proxy 自证，以及 golden binding 与 golden_overview
// view 的一一对应。
//
// 它是纯函数：不查数据库、不读文件、不发网络请求。
func validateStructure(profile model.Profile) error {
	focusKeys, err := uniqueKeys("focus_kinds", collect(profile.FocusKinds, func(f model.FocusKind) string { return f.Key }))
	if err != nil {
		return err
	}
	primaryCount := 0
	for _, focus := range profile.FocusKinds {
		if _, compileErr := regexp.Compile(focus.ValuePattern); compileErr != nil {
			// 无法用 Go RE2 编译的 pattern 必须在启动时就拒绝，不能等到工作区请求才炸。
			return failf(ReasonInvalidFocusPattern, "focus_kinds[%s].value_pattern 无法被 Go RE2 编译", focus.Key)
		}
		if focus.Primary {
			primaryCount++
		}
	}
	if primaryCount != 1 {
		return failf(ReasonPrimaryFocusNotUnique, "primary focus 数量=%d，必须恰好 1 个", primaryCount)
	}

	journeyKeys, err := uniqueKeys("journeys", collect(profile.Journeys, func(j model.Journey) string { return j.Key }))
	if err != nil {
		return err
	}
	for _, journey := range profile.Journeys {
		if _, ok := focusKeys[journey.FocusKind]; !ok {
			return failf(ReasonDanglingRef, "journeys[%s].focus_kind=%s 未声明", journey.Key, journey.FocusKind)
		}
		if _, err := uniqueKeys("journeys["+journey.Key+"].stages",
			collect(journey.Stages, func(s model.JourneyStageDef) string { return s.Key })); err != nil {
			return err
		}
	}

	signalKeys, err := uniqueKeys("signal_bindings", collect(profile.SignalBindings, func(b model.SignalBinding) string { return b.Key }))
	if err != nil {
		return err
	}
	declaredTools, err := uniqueKeys("tool_refs", profile.ToolRefs)
	if err != nil {
		return err
	}

	for _, binding := range profile.SignalBindings {
		if _, ok := declaredTools[binding.ToolKey]; !ok {
			return failf(ReasonDanglingRef, "signal_bindings[%s].tool_key=%s 未在 tool_refs 中声明",
				binding.Key, binding.ToolKey)
		}
		// 这里刻意不校验 scope.refs 是否为空：canonical profile schema 里 refs 是
		// optional 且没有 minItems，设计交接稿、契约 README 与 Phase 2 计划都没有要求
		// 非 service 作用域必须声明 refs。Registry 不能自行缩小合法 v1 Profile 集合，
		// 否则一份完全符合契约的 Profile 会被无授权地 quarantine。
		if binding.MeasurementRole == model.MeasurementRoleProxy && strings.TrimSpace(binding.ProxyReason) == "" {
			return failf(ReasonProxyReasonMissing, "signal_bindings[%s] 是 proxy，必须给出 proxy_reason", binding.Key)
		}
		if err := validateThresholds(binding); err != nil {
			return err
		}
	}

	viewKeys := map[string]struct{}{}
	viewOrders := map[int]struct{}{}
	goldenOverviewViews := 0
	var goldenOverviewRefs []string
	for _, view := range profile.Views {
		if _, exists := viewKeys[view.Key]; exists {
			return failf(ReasonDuplicateKey, "views[%s] 重复", view.Key)
		}
		viewKeys[view.Key] = struct{}{}
		if _, exists := viewOrders[view.Order]; exists {
			return failf(ReasonDuplicateKey, "views[%s].order=%d 与其它 view 冲突，展示顺序不确定",
				view.Key, view.Order)
		}
		viewOrders[view.Order] = struct{}{}
		if view.JourneyRef != "" {
			if _, ok := journeyKeys[view.JourneyRef]; !ok {
				return failf(ReasonDanglingRef, "views[%s].journey_ref=%s 未声明", view.Key, view.JourneyRef)
			}
		}
		for _, ref := range view.SignalRefs {
			if _, ok := signalKeys[ref]; !ok {
				return failf(ReasonDanglingRef, "views[%s].signal_refs 引用未声明的 %s", view.Key, ref)
			}
		}
		for _, ref := range view.ToolRefs {
			if _, ok := declaredTools[ref]; !ok {
				return failf(ReasonDanglingRef, "views[%s].tool_refs 引用未在 tool_refs 中声明的 %s", view.Key, ref)
			}
		}
		if view.Renderer == model.RendererGoldenOverview {
			goldenOverviewViews++
			goldenOverviewRefs = view.SignalRefs
		}
	}

	if err := validateGoldenProjection(profile, goldenOverviewViews, goldenOverviewRefs); err != nil {
		return err
	}
	return nil
}

// validateThresholds 拒绝方向与 degraded/critical 顺序矛盾的阈值。above 必须
// degraded < critical，below 必须 degraded > critical；相等无法区分两级，同样拒绝。
func validateThresholds(binding model.SignalBinding) error {
	if binding.Thresholds == nil {
		return nil
	}
	thresholds := *binding.Thresholds
	switch thresholds.Direction {
	case model.ThresholdDirectionAbove:
		if !(thresholds.Degraded < thresholds.Critical) {
			return failf(ReasonInvalidThreshold,
				"signal_bindings[%s] direction=above 要求 degraded < critical", binding.Key)
		}
	case model.ThresholdDirectionBelow:
		if !(thresholds.Degraded > thresholds.Critical) {
			return failf(ReasonInvalidThreshold,
				"signal_bindings[%s] direction=below 要求 degraded > critical", binding.Key)
		}
	default:
		return failf(ReasonInvalidThreshold, "signal_bindings[%s] direction=%q 非法",
			binding.Key, thresholds.Direction)
	}
	return nil
}

// validateGoldenProjection 保证三种 Page View 的 golden_signals 恒为四项：
// family=golden 的 binding 全局恰好四个、latency/traffic/errors/saturation 各一个，
// 并且恰好一个 renderer=golden_overview 的 view 的 signal_refs 与它们一一对应。
//
// 缺一项会让页面少一张黄金卡，多一项会撞上 Page View schema 的 maxItems=4；只投影一部分
// 又会出现"配了但永远看不到"的 binding，因此不允许存在未被该 view 引用的 golden binding。
func validateGoldenProjection(profile model.Profile, goldenOverviewViews int, goldenOverviewRefs []string) error {
	goldenBySemantic := map[model.SignalSemantic][]string{}
	goldenKeys := map[string]struct{}{}
	for _, binding := range profile.SignalBindings {
		if binding.Family != model.SignalFamilyGolden {
			continue
		}
		goldenBySemantic[binding.Semantic] = append(goldenBySemantic[binding.Semantic], binding.Key)
		goldenKeys[binding.Key] = struct{}{}
	}
	if len(goldenKeys) != 4 {
		return failf(ReasonGoldenBindingInvalid, "family=golden 的 binding 数量=%d，必须恰好 4 个", len(goldenKeys))
	}
	for _, semantic := range model.GoldenSemantics() {
		switch len(goldenBySemantic[semantic]) {
		case 1:
		case 0:
			return failf(ReasonGoldenBindingInvalid, "缺少 semantic=%s 的 golden binding", semantic)
		default:
			return failf(ReasonGoldenBindingInvalid, "semantic=%s 的 golden binding 数量=%d，必须恰好 1 个",
				semantic, len(goldenBySemantic[semantic]))
		}
	}

	if goldenOverviewViews != 1 {
		return failf(ReasonGoldenBindingInvalid, "renderer=golden_overview 的 view 数量=%d，必须恰好 1 个",
			goldenOverviewViews)
	}
	referenced := map[string]struct{}{}
	for _, ref := range goldenOverviewRefs {
		if _, exists := referenced[ref]; exists {
			return failf(ReasonGoldenBindingInvalid, "golden_overview view 重复引用 %s", ref)
		}
		referenced[ref] = struct{}{}
		if _, ok := goldenKeys[ref]; !ok {
			return failf(ReasonGoldenBindingInvalid, "golden_overview view 引用了非 golden binding %s", ref)
		}
	}
	if len(referenced) != len(goldenKeys) {
		missing := make([]string, 0, len(goldenKeys))
		for key := range goldenKeys {
			if _, ok := referenced[key]; !ok {
				missing = append(missing, key)
			}
		}
		sort.Strings(missing)
		return failf(ReasonGoldenBindingInvalid,
			"golden_overview view 未投影全部 golden binding，遗漏 %s", strings.Join(missing, ","))
	}
	return nil
}

// validateToolRefs 是校验顺序的第 8 步：Profile 只能引用已发布并获授权的 tool key。
func validateToolRefs(profile model.Profile, catalog toolCatalog) error {
	for _, key := range profile.ToolRefs {
		if !catalog.Has(key) {
			return failf(ReasonToolNotPublished, "tool_refs 引用未发布的 %s", key)
		}
	}
	return nil
}

func collect[T any](items []T, key func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, key(item))
	}
	return out
}

func uniqueKeys(field string, keys []string) (map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			return nil, failf(ReasonDuplicateKey, "%s 中 %s 重复", field, key)
		}
		seen[key] = struct{}{}
	}
	return seen, nil
}
