package model

// nonNilSlice 把 nil slice 换成长度 0 的 slice。契约里 required 的集合字段即使为空也
// 必须序列化成 []：返回 null 会让页面在解构时崩溃。
func nonNilSlice[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func cloneStrings(items []string) []string {
	if items == nil {
		return nil
	}
	out := make([]string, len(items))
	copy(out, items)
	return out
}

func cloneSlice[T any](items []T) []T {
	if items == nil {
		return nil
	}
	out := make([]T, len(items))
	copy(out, items)
	return out
}

// cloneJSONValue 深拷贝一个已解码的 JSON 值。tool descriptor 的 input/output schema 是
// 任意嵌套 map/slice，浅拷贝会让调用方改到共享注册表。
func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case ToolJSONSchema:
		out := make(ToolJSONSchema, len(typed))
		for key, item := range typed {
			out[key] = cloneJSONValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneJSONValue(item)
		}
		return out
	default:
		return value
	}
}
