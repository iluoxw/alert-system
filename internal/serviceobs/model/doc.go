// Package model 是 Service Observability contracts v1 的 Go 镜像，且是 serviceobs
// 的叶子包：它只依赖标准库，不 import store、adapter 或任何 E2B 私有包。
//
// 唯一事实源是 docs/contracts/service-observability/v1/*.schema.json。本包的职责是
// 逐字段镜像那些 schema 的线上形状，不额外发明字段、不做业务判断：
//
//   - 契约里 required 的数组字段在 Go 里是 slice + 无 omitempty。nil slice 会被
//     encoding/json 写成 null，而契约要求空集合必须是 []，所以每个根类型提供
//     Normalize()，序列化前把 nil 补成空 slice。
//   - 契约里 nullable 的对象/标量（focus、profile、next_cursor、signal value）用
//     pointer，nil 就是 JSON null，与 schema 的 type: [..., "null"] 对齐。
//   - optional 字段带 omitempty，保证不传的字段不会凭空出现在输出里，从而通过
//     additionalProperties=false 之外的语义断言（例如"没有阈值就不要写
//     threshold_source"）。
//
// 时间字段用 Timestamp（string 别名）而不是 time.Time：model 的职责是 round-trip
// 线上格式，time.Time 会把时区偏移和小数秒归一化，导致镜像失真。需要计算时用
// Timestamp.Time() 解析。
package model
