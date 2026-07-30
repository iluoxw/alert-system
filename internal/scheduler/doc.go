// Package scheduler 是统一持久化定时器：承接 TTL、人员升级、group_wait/group_interval、
// 渠道升级的全部定时逻辑，进程重启不丢失待触发的定时器。
package scheduler
