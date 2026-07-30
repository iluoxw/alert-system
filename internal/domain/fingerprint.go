package domain

import (
	"fmt"
	"hash"
	"hash/fnv"
	"sort"
)

// sep 是哈希输入中字段之间的分隔符。每写入一个字段（RuleID、标签 key、标签 value）后追加一个
// sep 字节，避免相邻字段拼接产生歧义——例如不加分隔符时 {"a":"bc"} 与 {"ab":"c"} 会得到相同的
// 拼接结果 "abc"，从而撞出相同指纹。0x1e 是 ASCII record separator，普通标签内容不会包含它。
const sep = '\x1e'

// Fingerprint 按架构决策 D5 计算告警去重键：
//   - metric 类：hash(rule_id + 全标签集 labels)
//   - log/trace 类：hash(rule_id + 资源标识 resource_ref)，日志/trace 内容不参与哈希
//
// 未声明 SignalKind（零值）时按 metric 策略处理。标签按 key 排序后逐对写入，
// 保证同一标签集的 fingerprint 不受来源 map 遍历顺序影响。
func Fingerprint(e AlertEvent) string {
	h := fnv.New64a()
	writeField(h, e.RuleID)

	switch e.SignalKind {
	case SignalKindLog, SignalKindTrace:
		writeLabels(h, e.ResourceRef)
	default:
		writeLabels(h, e.Labels)
	}

	return fmt.Sprintf("%016x", h.Sum64())
}

// writeLabels 将 labels 按 key 升序逐对写入 h。
func writeLabels(h hash.Hash64, labels map[string]string) {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(h, k)
		writeField(h, labels[k])
	}
}

// writeField 写入单个字段并追加分隔符。
func writeField(h hash.Hash64, s string) {
	h.Write([]byte(s))
	h.Write([]byte{sep})
}
