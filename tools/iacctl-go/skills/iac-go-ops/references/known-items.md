# 已知项（iac-go-ops / iacctl-go）

## 1. fat 与 pro 同 org 混流，环境选择器必须内置（2026-11-11）

PRO 上线当晚发现：同一个 OpenObserve org、同名流、同 `service_name`，只有
`deployment_environment_name` 区分 fat/pro；thanos 侧则完全没有 `service_name`，
只能用 `app_id` / `namespace`。不带选择器的查询会把 PRO 流量算进 FAT（实测混入
240 万次校验中的 1/6）。iacctl-go 的选择器统一来自 `packs/iac-go/resources.yaml`，
改环境标识只改那一处；手写查询要带上选择器再跑。

## 2. PRO 阈值是临时线（2026-11-11 定）

判定线沿用 FAT 时代的数值：5xx ≥0.1%/1%、Redis 超时 >0.5‰（5 分钟 ≥2‰）、
延迟 A <99% / B <99.5%、mysql running ≥20。FAT 的线从 FAT 路径噪声底推出
（跨网段丢包 + 共享 Redis 191 的 BGSAVE）；PRO 到 redis002 VIP 丢包接近 0，
上线首夜实测：Redis 超时 0.14–0.19‰、p99 3–24ms（窗口相关）、5xx≈0、
延迟 A 99.98%。连续观测数日后按 PRO 自己的噪声底重定；重定前不要因数值与
FAT 不同就改线。

## 3. 两条管道数值同源但有抓取时点差

thanos（CCloud `psdcloud-app-monitor` 抓取）与 O2（OTel push）是同一批计数器
的两条管道：计数器抽样差 0.01–0.03%，延迟分位数差 ~2ms 量级。与 Python skill
（走 O2）交叉验证时，数字有小差异是预期的；量级一致即可。

## 4. 计数规则（与 Python skill 一致）

不用 `increase()`、不用子查询（O2 对稀疏序列少计、子查询算错）；一律
`query_range` 原始值本地差分：相邻点相减，负差视为复位改加新值；计数查询
结束多取一个 step（O2 在网格端点会丢样本）；晚于取数时刻的样本丢弃（O2 会用
最后一次抓取填充未来刻度）；均值分母是首尾样本实际覆盖的秒数。

## 5. PRO 不适用的层

- LP：无 iac-probe；外部探测=五地探测（域名级，不区分 node/golang）；
- L5：无 soak/loadgen；
- L3 redis_191 / bgsave：无 redis 侧 exporter 指标（redis002 的观测在 Node 版
  监控栈，Node 下线后另行评估）；
- L6：O2 的告警/SLO 列表是 org 级、未按环境区分，PRO 专属 SLO 建立前不评估。

## 6. l7 的日志有采样

指标 5xx > 0 但日志聚合 0 行 → unknown，不是 green（采样可能漏）。日志分组
条数不必等于指标计数。

## 7. 版本标识

thanos 上版本标签是 `app_version_num`（如 14），没有 `service_version` 指标；
O2 侧对应 `service_version`（如 f0f82fd）。`l4.version` 报的是前者。

## 8. 跨管道对比看均值，不看窗口总量

thanos 与 O2 的采样网格不对齐，窗口两端的边界桶落到各自网格的位置不同，
覆盖跨度可差接近一个 step（300s）。同窗口下两边的「窗口总量」可差 ~8%
（1h 窗口 1.36M vs 1.48M），小计数值（如 Redis 超时几百次）受单个边界桶
影响更明显（189 vs 245）。**可比的是均值 qps（378 vs 379，0.3%）与分位数
（p99 19.27 vs 18.81ms）**——两者都除以各自实际覆盖的秒数。与 Python skill
（走 O2）交叉验证时按此判读。
