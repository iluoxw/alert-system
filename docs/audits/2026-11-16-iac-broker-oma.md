# IAC broker / OMA 适配取证与验收

日期：2026-11-16。Issue #25，Refs #22。基线 fb505a2，独立 worktree `/private/tmp/alert-iac-broker`。范围：只读 broker、固定版本 Go CLI 快照、worker 镜像 recipe 和 OMA API 只读能力核验。没有修改 IAC 仓库、现有日报 Agent、Grafana 或线上配置，没有发送 OMA 写请求。

## 源能力核对

应用 iac-go-ops、iac-shared 和 oma-agentize 技能，区分其历史默认与当前源码。实际 CLI 位于 `iacctl/golang/iacctl`，来源 revision `643ddf7aaec314a438f052a719b3741f2eef71ff`，该子目录取样时 Git clean。源码加载运行时 OBS_AUTH，不把旧 Node 的「凭据编译内置」当作当前事实。Go pack 的 expect_replicas=5 不代表当前目标副本数。

选择24个纯 Go/模块/pack/skill文件打包到 tools/iacctl-go，逐文件哈希见 SOURCE.json。没有复制 .env、git、lock、已构建二进制、shell配置或旧 Node pack；复制后逐文件 hash 与原快照全部匹配。只输出布尔结果的凭据字面量扫描未发现候选字符串；这不是对任意外部文件的完整秘密审计。原库未提供随选定源码的独立 license 文件，快照来源保留，发布方应沿本组织授权使用。

broker 对 model 暴露固定 query ID，代码生成固定 argv，经绝对路径直接运行CLI；不使用 shell、不接受 env/app/cluster、PromQL、SQL、URL 或命令片段。环境最小化，OMA key/任意配置路径不传给子进程。响应只保留有界数值与固定连通性字段，未采集、失败和缺样本为 unknown。7条 Go + 3条共享 + canary 均有固定 profile；共享五地/VIP仅给 core 观测和明确 unknown 覆盖缺口，不从Go业务指标推导其正常。

## OMA 真实能力与边界

本机 ant1.34.0，Go buildinfo 显示 anthropic-sdk-go v1.74.0。SDK源码不在本机可用module cache；没有假设已读到SDK实现。实际实例为 `https://oma.test.seewo.com` / default，模型回读 `default/deepseek-flash`。

历史已验证 Demo 是程序代查、同一 OMA 会话两轮 user.message。原生 read 工具曾503，不能作为首期必需依赖；本轮不修改现有 Go daily brief (`agent_cmJ2nCWWiB8yNKuSgThWlkYc` v1、环境 `env_pwlE8ULfRTexjMcqEAL1zI3Z`，ID由主Agent回执)。专用告警Agent由主Agent创建，tools=[]，不挂日报记忆/飞书目标。

当前只读实测安全元数据见 [oma-read-schema.json](evidence/2026-11-16-iac-broker/oma-read-schema.json)：

| 请求 | 实测 |
|---|---|
| GET /v1/sessions?limit=1 | 200；data、next_page；对象含 id/title/metadata/status/created_at/environment_id/agent |
| GET /v1/sessions/{历史DemoID} | 200，idle，模型对象 id=default/deepseek-flash |
| GET /v1/sessions/{id}/events?limit=1 | 200；data、next_page；事件含 id/type/processed_at/session_thread_id |
| GET /v1/sessions/{id}/events/stream | 200；Content-Type text/event-stream；首帧 `: connected` |
| 同 SSE 带 Last-Event-ID | 200；connected。没有验证历史回放，不能据此依赖该头恢复 |

只记录响应字段名、HTTP状态和安全事件元数据，没有读取/打印 API key值、会话消息文本、vault内容或memory秘密。环境变量 OMA_API_KEY/OBS_AUTH 仅检查存在布尔值；CLI执行本来需要转发OBS_AUTH，值不入日志/报告。

依据历史Demo代码及本地CLI，写接口契约为：

```text
Headers: x-api-key=<runtime secret>
         anthropic-version: 2023-06-01
         anthropic-beta: managed-agents-2026-04-01
POST /v1/sessions
  {agent:{type:"agent_with_overrides",id:<专用agent>,system:<受控prompt>,tools:[]},
   environment_id:<受控环境>,title:<稳定run标题>,metadata:{run_id:<持久run>},
   initial_events:[{type:"user.message",content:[{type:"text",text:<JSON字符串>}]}]}
  -> {id,status,...}
POST /v1/sessions/{id}/events
  {events:[{type:"user.message",content:[{type:"text",text:<带submission_id的证据JSON>}]}]}
```

本轮只读验证不证明上述 POST 已在当前实例执行；主Agent负责专用对象及真实完整上线验收。当前列表响应已证实 title/metadata 存在，但没有已验证服务端幂等保证，也没有metadata过滤参数。Create结果不确定后应按agent/创建窗口分页查回、客户端匹配run ID，不直接重发。Event POST结果不确定后按持久submission_id从user.message查回。

官方主源确认 [会话列表](https://platform.claude.com/docs/en/api/beta/sessions/list) 使用 next_page/page，列表有title/metadata；[会话操作](https://platform.claude.com/docs/en/managed-agents/session-operations) 明确idle不等于工作完成。[事件流接口](https://platform.claude.com/docs/en/api/beta/sessions/events/stream) 是 GET `/events/stream`，不是猜测的 `?stream=true`。[事件列表](https://platform.claude.com/docs/en/api/beta/sessions/events/list) 可分页补漏。官方最新schema不自动等同内网部署，所以上表保留内网实际回执。

worker应真实订阅SSE并持久 event ID，断连后GET分页/重叠时间窗口补漏，按session/event唯一键去重。只有固定JSON报告与证据引用校验通过并持久化才算完成；idle/end_turn只是轮次边界。原始SSE仅观察到连接帧，本子任务没有创建新会话去制造消息或复现断连；事件实际到达及恢复需由runtime/主Agent验收。

## 已完成验证

```bash
GOCACHE=/private/tmp/alert-iac-gocache go test -race ./internal/iacbroker -count=1
GOCACHE=/private/tmp/alert-iac-gocache go vet ./internal/iacbroker
# 在 tools/iacctl-go 下：
GOCACHE=/private/tmp/alert-iac-gocache go test ./... -count=1
GOCACHE=/private/tmp/alert-iac-gocache CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false \
  -ldflags='-s -w -X github.com/luoxw/iacctl-go/cmd.version=source-643ddf7aaec314a438f052a719b3741f2eef71ff' \
  -o /private/tmp/iacctl-go-linux-amd64 .
```

上述命令退出0。broker race覆盖完整请求预校验、固定scope/argv、最小环境/标签剥离、无数据/陈旧/坏JSON/退出失败、超时、输出上限与等待槽取消、共享缺口和缺OBS_AUTH不影响Thanos。上游prom/runtimecfg/series测试实际执行通过；其余无test文件不算覆盖。

另外构建本机审计CLI，使用loopback httptest等价受控HTTP server与临时非秘密配置，在完全无OBS_AUTH环境执行真实 `query prom --source thanos`，成功返回数值0；`sources --probe` 同时明确报O2缺凭据。只连接127.0.0.1，不访问线上。证据见 [cli-smoke.json](evidence/2026-11-16-iac-broker/cli-smoke.json)，含linux/amd64和本机二进制SHA。没有把数值0本身判为应用健康。

## 未完成与交接

- Dockerfile.iac-worker 与 recipe 已提供；cmd/oma-worker 属runtime子任务，本分支独立没有该cmd，因此尚未声称完整worker镜像已成功build或部署。与runtime集成后由root按内网镜像/代理buildx构建与发布。
- OMA SSE真实消息、断连补漏、Create/Event结果未知恢复、PG崩溃恢复与report API由runtime/主Agent验收；本子任务没有以轮询代替“事件订阅”。
- 没有做当前真实业务查询、native工具可靠性或生产故障归因验收。固定观测是调用时刻/5分钟窗口，不重建告警历史现场。
- 五地/VIP覆盖项明确unknown，不导入旧Node错误关系图，不把OpenObserve部署归属当作IAC业务依赖。
- 旧Go日报的tools/secret/memory/cron保持原样，不因创建告警agent而替换或暂停日报。

用户已授权完整实施；本子任务不增加额外审批。线上凭据、agent创建、镜像发布和完整验收由主Agent负责，不在本工作树直接执行。

追加：`BUILDX_CONFIG=/private/tmp/alert-iac-buildx docker buildx build -f deploy/docker/Dockerfile.iac-worker --call=targets .` 退出0，列出cli-build、worker-build、iac-worker三个target；只证明recipe可解析，不是完整镜像构建通过。broker最终race退出0（5.901s），focused vet与diff --check均退出0。
