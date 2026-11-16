// Package pipeline 负责从队列消费事件并依序执行各处理 stage：
// 去重 -> 告警映射 -> 路由 -> 分组 -> 静默 -> 抑制。
package pipeline
