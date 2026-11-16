---
name: iac-go-ops
description: >
  用 iacctl-go 对 PRO 上的 Go 版 auth-center 做只读巡检与下钻。双源取数：
  thanos-inner（CCloud 抓取的 iac_* 指标，按 app_id 选环境）+ OpenObserve
  （mysql 指标、日志/trace SQL、流统计）。当用户说：Go 版巡检、PRO 巡检、
  iacctl-go、golang 版 IAC、看 PRO 的 auth-center 正不正常、Go 版 5xx/延迟/
  Redis 超时下钻时使用。不用于 Node 版 PRO（Node 版下线前用
  iac-service-inspection / iac-risk-analysis）。
metadata:
  requires:
    bins: ["iacctl-go"]
  cliHelp: "iacctl-go --help"
---

# IAC-Go PRO 巡检（iacctl-go）

## 调用方式

**前置：** `iacctl-go` 在 PATH（或用绝对路径）。OpenObserve 凭据是交互式
shell 里的 `OBS_AUTH`（`user:pass`），用 `zsh -ic` 带入；缺失时退出码 2、
不出报告。thanos 无需认证。

```bash
zsh -ic 'iacctl-go inspect --window 1h --format both'      # 巡检（summary + JSON）
zsh -ic 'iacctl-go inspect --window 24h --format json'
zsh -ic 'iacctl-go sources --probe'                         # 双源连通性
zsh -ic 'iacctl-go query prom --source thanos --expr "..."'  # 任意只读 PromQL
zsh -ic 'iacctl-go query sql --sql "SELECT ..." --window 1h' # O2 日志/trace SQL
iacctl-go tools                                             # 工具面（无需凭据）
```

历史窗口用 `--start/--end`（CST ISO 时间，与 `--window` 互斥）。窗口至少 5 分钟。

## 数据源路由（实测 2026-11-11）

| 指标族 | 取数 | 环境选择器 |
|---|---|---|
| `iac_http_*` / `iac_admission_*` / `iac_dependency_errors_total` / `iac_cache_untrusted` / fallback 计数 / `process_cpu_seconds_total` | **thanos** | `app_id="80beff42…"`（thanos **无** `service_name` 标签） |
| `mysql_threads` | **O2** | `deployment_environment_name="pro"` |
| 日志/trace SQL、流统计 | **O2** | 同上（SQL 字符串用单引号） |

选择器已内置在 `packs/iac-go/resources.yaml`，不要在查询里手写环境过滤——
混用 fat/pro 会把两套流量算在一起（见 known-items 第 1 条）。

## 检查与判定线（L0–L8，PRO 临时线）

| 层 | 检查 | 线 |
|---|---|---|
| L0 | sample_age / stats_gap | 活窗口样本 >180s 黄；stats 同实例连续缺 ≥2min 黄 |
| L1 | 5xx / 401 / admission | 5xx ≥0.1% 黄 ≥1% 红；准入 >0 黄 |
| L2 | latency_a / latency_b / p99 | ≤25ms <99% 黄；≤100ms <99.5% 黄 |
| L3 | redis_timeout | 窗口 >0.5‰ 或任一 5 分钟 ≥2‰ 黄 |
| L3 | mysql_fallback / cache_untrusted / mysql_running | 被拒 >0 黄；untrusted >0 黄；running ≥20 红 |
| L4 | replicas / restarts / cpu / version | ready<5 红；重启 >0 黄；cpu/version 信息 |
| L7 | 5xx 日志明细 | 有指标 5xx 但日志 0 行=未知（日志有采样） |
| L8 | 流存量 | 信息项 |

总体 = 计入检查的最严重项（红>黄>未知>绿）。**任何查询失败或无数据都是
unknown，绝不判绿。** LP（iac-probe）/L5（压测）/L6（org 级告警 SLO）对
PRO 不适用，不计入总体。

## 下钻（异常时）

- 5xx：`inspect --format json` 的 `l7.5xx` 分组（route/err_code/path/redis_error）
  + `query sql` 按 `iac_err_code` 过滤日志；
- Redis 超时：`query prom --source thanos --expr 'sum by (operation) (iac_dependency_errors_total{app_id="80beff42ab0b41819f9c1c3c3ac95413308e70eb",dependency="redis",error_class="timeout"})' --range --window 3h`；
- 延迟：桶差分已在 `l2`；细看用 `query prom` 取 `iac_http_request_duration_seconds_bucket`。

## 硬性约束

- 只读：不改服务、不改告警、不写 OpenObserve；
- 不打印 `OBS_AUTH`、token 或 webhook；报告附录只有查询文本；
- 阈值是 PRO 临时线（上线首夜定的），连续观测后按 known-items 第 2 条重定。

## 参考

- `references/known-items.md` — 已知项与阈值来源
