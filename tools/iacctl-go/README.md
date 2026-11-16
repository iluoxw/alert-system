# iacctl-go

IAC Go 版（auth-center PRO）只读可观测 CLI。thanos-inner 取服务自身指标，
OpenObserve 取 mysql 指标与日志/trace SQL。计数一律本地差分（不用
`increase()`、不用子查询；计数查询多取一个 step；晚于取数时刻的样本丢弃）——
规则与 [skills/iac-go-inspection](../skills/iac-go-inspection) 的
`references/queries.md` 完全一致。

## 构建

```bash
CGO_ENABLED=0 go build -ldflags "-X github.com/luoxw/iacctl-go/cmd.version=0.1.0" -o bin/iacctl-go .
go test ./...
```

本机 Go 工具链在启用 cgo 时链接出的 Mach-O 缺 `LC_UUID`，dyld 会 abort，
统一 `CGO_ENABLED=0`。

## 运行

```bash
iacctl-go inspect --window 1h --format both          # 巡检垂直切片
iacctl-go sources --probe                            # 后端连通性
iacctl-go tools                                      # Agent 工具面（JSON）
iacctl-go query prom --source thanos --expr '...'    # 任意只读 PromQL
iacctl-go query prom --source obs --expr '...' --range --window 1h
iacctl-go query sql --sql 'SELECT ...' --window 1h   # O2 日志/trace SQL
```

凭据：thanos 无认证；OpenObserve 读环境变量 `OBS_AUTH`（`user:pass`，
用 `zsh -ic` 运行）。缺失时退出码 2，不出报告。配置可用 `--config` /
`IACCTL_GO_CONFIG` 覆盖端点（默认 PRO 内置值）。

## 巡检覆盖（v0.1 垂直切片）

| 检查 | 数据源 | 语义 |
|---|---|---|
| `l0.sample_age` | thanos 即时 | 最新样本年龄，活窗口 >180s 黄 |
| `l0.stats_gap` | O2 SQL（30 分钟切片） | service.stats 每实例每分钟连续性，连续缺 ≥2 分钟黄 |
| `l1.qps` | thanos 差分 | 校验量/均值/5 分钟峰谷（信息项） |
| `l1.5xx` | thanos 差分 | 按路由 system_error 占比，≥0.1% 黄 ≥1% 红 |
| `l1.401` | thanos 差分 | 校验 business_reject 占比（信息项） |
| `l1.admission` | thanos 差分 | 准入拒绝 >0 黄 |
| `l2.latency_a/b` | thanos 桶差分 | ≤25ms <99% 黄；≤100ms <99.5% 黄 |
| `l2.p99` | 桶差分本地插值 | p99/p99.9（信息项） |
| `l3.redis_timeout` | thanos 差分 | 应用侧超时/校验，>0.5‰ 或任一 5 分钟 ≥2‰ 黄（临时线） |
| `l3.mysql_fallback` | thanos 差分 | 兜底被拒 >0 黄；过载跳过只报告 |
| `l3.cache_untrusted` | thanos 仪表 | >0 黄 |
| `l3.mysql_running` | O2 仪表 | Threads_running ≥20 红（临时线） |
| `l4.replicas` | ccloud-cli 快照 | ready < 期望副本 红 |
| `l4.restarts` | ccloud-cli 快照 | >0 黄 |
| `l4.cpu` | thanos rate | 单 Pod 最大核数（信息项） |
| `l4.version` | thanos 差分 | 窗内有流量的 app_version_num（信息项） |
| `l7.5xx` | O2 SQL | 5xx 日志分组（route/err_code/path/redis_error）；指标有 5xx 但日志 0 行=未知 |
| `l8.streams` | O2 streams | 日志/trace 流存量（信息项） |

另有 `skills list/read/sync`：嵌入的 `iac-go-ops` SKILL 同步到
`~/.agents/skills`（`sync --check` 校验一致性）。未实现层（LP/L5/L6）由
Python skill `iac-go-inspection` 覆盖。与 Python 同窗口交叉验证：均值 qps
与分位数一致（378 vs 379、p99 19.27 vs 18.81ms）；窗口总量因两管道边界
采样可差 ~一个 step，看均值不看总量（known-items 第 8 条）。

## 目录

```text
cmd/                 cobra 命令（inspect/tools/sources/query）
internal/prom/       双源客户端（thanos 无认证 / O2 Basic+SQL），Auth/Query 错误分离
internal/series/     本地差分管线（DeltaMap/CoverageBounds/DropAfter/RateExtremes…）
internal/runtimecfg/ 运行时配置（--config / IACCTL_GO_CONFIG）
internal/inspect/    判定引擎与输出（summary/JSON）
packs/iac-go/        PRO 资源 YAML（app_id 选择器、mysql、流名）
```

## 数据源路由（2026-11-11 实测）

| 指标族 | 取数 | 环境选择器 |
|---|---|---|
| `iac_http_*` / `iac_admission_*` / `iac_dependency_errors` / `iac_cache_untrusted` / fallback / `process_cpu` | **thanos** | `app_id="80beff42…"`（无 `service_name` 标签） |
| `mysql_threads` / redis / probe 指标 | **O2** | `deployment_environment_name="pro"` |
| 日志 / trace SQL | **O2** | 同上（SQL 单引号） |

阈值沿用 Python skill 的判定线；PRO 噪声底待观测后重定（known-items 第 22 条）。
