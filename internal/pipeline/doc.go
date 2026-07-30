// Package pipeline 负责从队列消费事件并依序执行各处理 stage：
// 去重 -> 分组 -> 静默 -> 抑制（stub）-> 路由。
package pipeline
