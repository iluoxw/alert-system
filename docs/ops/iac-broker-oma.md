# IAC 只读取证 broker 与 OMA 接入

本包服务于 Issue #25 / #22：Grafana 告警经 SRE Control 持久投递后，由独立常驻 worker 请求固定证据，再把证据回传到同一 OMA 会话。它不修改 IAC、Grafana、OpenObserve、Fault/Impact/Incident 状态，不发送业务通知。worker 的持久执行、事件订阅、报告与 API 由对应 runtime 模块负责。

## 来源与边界

| 能力 | 固定实现 | 凭据/可变状态 |
|---|---|---|
| Go PRO 指标 | 打包 iacctl-go 的 query prom | Thanos 不需凭据；不接受模型传入表达式 |
| O2 MySQL running | 固定 service_name + pro 选择器 | 运行时 OBS_AUTH，绝不嵌入二进制 |
| 来源连通性 | sources --probe | 仅输出 source 名、reachable、credential_configured；不输出 URL/error 原文 |
| OMA 判断 | 专用 agent 的两轮 JSON，tools=[] | API key 仅供 worker，不传给 CLI/模型 |
| 执行/报告恢复 | 常驻 worker PostgreSQL 账本 | ACK 只代表持久接收，不代表分析完成 |
| 五地入口/Redis VIP | 明确未采集的覆盖项 | 返回 unknown，不把 Go 应用正常当作入口/VIP正常 |

`tools/iacctl-go` 是从真实 Go CLI `golang/iacctl` 选择的纯源码快照，来源 revision `643ddf7aaec314a438f052a719b3741f2eef71ff`，该子目录取样时无未提交修改；每个文件的 SHA256 见 `SOURCE.json`。没有复制本机二进制、dotenv、lock、凭据或 Node ServicePack。上游部署日报不在本任务修改范围。

Go pack 的 expect_replicas=5 是旧资源默认，不能推出当前故障。broker 不开放 inspect（含历史副本预期及 ccloud-cli 依赖）、任意 SQL、任意 PromQL、URL、shell、Node 运维路径。固定 PromQL 与当前 Go Grafana 生产规则口径对应；rate/increase 仅用于 Thanos，不把旧 O2 的计数求值限制误套到 Thanos。

## 可调用接口

```go
broker, err := iacbroker.New(iacbroker.Options{
    Binary: "/app/iacctl-go",
    SourceRevision: "643ddf7aaec314a438f052a719b3741f2eef71ff",
    Timeout: 45 * time.Second,
})
// worker 按受控规则 profile 生成候选集；模型仅能请求这些 ID，不带参数。
bundle, err := broker.Collect(ctx, iacbroker.Profile(ruleID))
```

Collect 一次接受 1–6 个不重复 ID，先校验完整请求再执行；未知/重复/超限请求不会执行任何命令。固定 scope 为 `pro`、app_id `80beff42ab0b41819f9c1c3c3ac95413308e70eb`、cluster `prd60`、namespace `iac-pro-pro`。每个 Broker 同时只运行一个进程，等待执行槽可取消。执行默认 45 秒，上限 90 秒；外层 worker 还应给整个取证轮次总 deadline，避免六个查询累计等待拖住任务。

完整 ID：`sources`、`freshness`、`traffic`、`errors`、`latency`、`commit_unknown`、`audit_dead`、`audit_lag`、`redis_timeouts`、`mysql_running`、`entry_coverage`、`vip_coverage`。

| 规则 | Profile |
|---|---|
| iac-go-system-errors | sources, freshness, traffic, errors, redis_timeouts, mysql_running |
| iac-go-latency-p99 | sources, freshness, traffic, latency, redis_timeouts, mysql_running |
| iac-go-traffic-zero | sources, freshness, traffic, errors |
| iac-go-metrics-stale | sources, freshness, traffic |
| iac-go-commit-unknown | sources, freshness, commit_unknown, mysql_running |
| iac-go-outbox-dead / iac-go-outbox-lag | sources, freshness, audit_dead, audit_lag |
| iac-api-probe-not-all-200 / iac-api-probe-delay-high | sources, freshness, traffic, errors, entry_coverage |
| iac-redis002-vip-ras-down | sources, freshness, traffic, redis_timeouts, vip_coverage |
| iac-sre-oma-canary | sources, freshness, traffic, errors |

Evidence 使用 `iac_evidence_v1`，包含 query_id、固定 scope、observed_at、status、reason_code、数值 data 和 provenance（实际 CLI binary SHA256、来源 revision、固定查询、窗口说明）。status=ok 只代表取到合法观测，不是服务健康结论。空结果、无新样本、超时、凭据失败、解析失败、输出超限都为 unknown；缺 OBS_AUTH 不阻塞独立 Thanos 查询。Bundle.partial 表示有证据缺口。

CLI stdout 上限 256KiB、stderr 上限 4KiB，series 上限400样本；只接受有效数值与180秒内样本，不向模型传递标签、原始日志/SQL、错误原文或凭据。`sources` 只验证连通性。固定 gap ID 返回 unknown，明确应用指标不覆盖五地探针或 VIP。凭据只从运行时 OBS_AUTH 转发给 O2/sources；子进程不继承 OMA key、任意 IACCTL_GO_CONFIG、HOME、云凭据或可变 PATH。

## 打包与部署

镜像 recipe：`deploy/docker/Dockerfile.iac-worker`。同一次 buildx 编译被审计的 Go CLI 和本仓库 `cmd/oma-worker`，以用户10001运行，不把凭据加入镜像层。runtime 子任务集成前，当前分支没有该 cmd，不能因此宣称完整 worker 镜像已构建成功。

从仓库根目录运行：

```bash
make -f deploy/iac-worker/Makefile image \
  GO_BASE_IMAGE='<内网Go基础镜像>' \
  ALPINE_BASE_IMAGE='<内网Alpine基础镜像>' \
  GOPROXY='<内网Go代理>' GOSUMDB='<内网校验配置>' \
  VCS_REF='<发布提交>' IMAGE='<发布镜像:固定标签>'
# 发布用 push target；本地使用 --load，发布使用 --push，均为 buildx。
```

这些 placeholder 不是实际可运行的镜像名。内网构建必须显式提供镜像及代理，禁止隐式访问 Docker Hub/proxy.golang.org。SOURCE.json 逐文件审计，CLI 二进制运行时 SHA256 随每份证据输出；发布方同时记录镜像 digest 和本仓库 revision。

运行配置通过 worker 自身配置适配 `Binary=/app/iacctl-go` 与固定来源 revision。仅 worker 进程持有 OMA API key/PG授权；CLI 只转发需要的 OBS_AUTH。部署应使用只读根文件系统、独立 tmp emptyDir、禁用 service account token 自动挂载、最小 RBAC，网络仅放行 PG、OMA、Thanos 与 O2 所需主机/端口；不挂载本机 kubeconfig、IAC 源仓库、SSH/JumpServer 或用户 HOME。

## OMA 实证与会话恢复

详见 `docs/audits/2026-11-16-iac-broker-oma.md` 与脱敏 `oma-read-schema.json`。当前只读验证过的实例为 `https://oma.test.seewo.com` / default，旧 Go 日报 agent/env 仅作参考，不复用其可写记忆、飞书或 bash/read 工具。root 为本次创建专用告警 agent；tools=[]，模型仅交换固定 JSON，OBS_AUTH 不进入 OMA prompt/vault/会话。

已验证路径是告警初始 user.message → 模型请求固定 query_ids → broker 代查并持久证据 → 同会话 user.message 回传 → 结构化报告。历史 native read 工具报过503，不能作为首期必需能力。

订阅真实 `GET /v1/sessions/{id}/events/stream` SSE；持久化 event ID 与报告状态，断线后用 `GET /events` 分页补漏。当前仅证实 Last-Event-ID 请求被接受，未证实历史重放，因此恢复不能只依赖该头。idle/end_turn 只是轮次结束，结构化报告内容及证据引用匹配后才能判定分析完成。Create 响应不确定时按持久 run ID/title/metadata 分页查回；没有已验证的服务端幂等保证，不能直接无界重发 Create。

## 运维验收与回退

1. 用部署内固定CLI版本与来源SHA回读比对 SOURCE.json；确认运行时凭据存在仅输出布尔值。
2. 缺 OBS_AUTH、无数据/无新样本、来源不可达必须输出 unknown；Thanos 独立查询仍可执行。不要为未知设默认绿。
3. 隔离 canary 证明同一会话提出固定ID、broker证据入账、SSE事件持久化、二轮报告入账、重投不重复创建报告；实际模型/查询调用由发布主 Agent 受控执行。
4. 用非业务目标验证 SSE断线补漏、服务重启恢复、Create/Event POST 响应不确定；关键业务通知仍走原确定性通知路径。
5. 回退先停止新 worker 领取；保留 PG 任务/证据/报告账本以便恢复。不自动删除 OMA session 或修改既有日报。不要因O2是E2B部署就推导IAC业务依赖E2B。

本子任务不创建/应用 OMA agent、环境或 Deployment，不写线上。root 已获用户完整实施授权，最终配置和上线验收由主 Agent 负责；本文件不引入额外用户确认步骤。
