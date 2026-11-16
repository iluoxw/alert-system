# OMA Go 日报的 CCloud 只读取证包

本目录为 Issue #27 的部署材料。使用官方 `@cvte/ccloud-cli` 2.2.1 离线包，经 SRE 固定只读代理读取 IAC PRO pods；不把 CCloud 管理员 key 交给 OMA。这里只提供包和渲染工具，不自动更新线上资源。

## 服务端接入

`internal/iacccloudproxy.New(Options{ProxyToken: ..., CloudAPIKey: ...})` 返回 `http.Handler`。服务端从 `IAC_CCLOUD_PROXY_TOKEN` 和 `IAC_CCLOUD_UPSTREAM_KEY` 读取两个独立值。前者至少 32 字节，仅作为固定代理令牌；后者是授权给服务端的 CCloud key（不带 Bearer）。`IAC_CCLOUD_PROXY_ENABLED` 默认关闭；只有明确启用且两个配置都存在时才挂载代理。不得自行读取或转存本机管理员 key，不新增 Kubernetes Secret，也不能改换环境变量或 Vault 绕过未获授权的凭据转存。当前线上代理尚未启用。

在现有 SRE API 注册以下三个 GET 到同一个 handler（例如 Gin 的 MountUnauthenticated 方法）；此处的“未经过用户登录”不绕过 handler 自身的 Bearer 校验。其他 method 不注册，框架 404 即拒绝，不需要 Any 路由。

- `/api/v1/iac-ccloud/cloud/api/v1/agent/meta/resources`
- `/api/v1/iac-ccloud/cloud/api/v1/agent/meta/resources/app/actions`
- `/api/v1/iac-ccloud/cloud/api/v1/agent/app/pods`

只 pods 路由产生上游 IO，固定 `appId=80beff42ab0b41819f9c1c3c3ac95413308e70eb&clusterId=60`。拒绝其他 app、cluster、appVersionId、重复或未知 query、写方法。官方CLI实际给 pods GET 带精确 `{}` body，因此仅这个路由允许这2字节；不从 body 读取任何参数。上游 key 不转发给 CLI；响应只保留 podName、phase、ready、restartCount、nodeName，不暴露 IP、环境变量、annotations。超时、并发、体积有界，错误不包含上游原文。

## 离线文件与安装

运行 `python3 scripts/pack_runtime.py --help` 查看打包参数。打包输入是本机安装的真实 npm 包，而非 IAC 仓库或用户配置目录。运行时需要 Node >=18.15；本轮 OMA 原生工具已验证 Node v24.18.0/x86_64，因此无需另打 Node 包。

运行时归档的 SHA-256 是 `df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87`，2,618,568 字节，3447个文件。归档未提交源码仓库；上传 OMA 后用参数引用 File ID，文件到期后重新上传并渲染，不能把临时 File ID 当稳定模板。

将四个文件上传并挂载：

| 本目录文件 | OMA resource.mount_path | 会话路径 |
| --- | --- | --- |
| 打包生成的 ccloud-cli-2.2.1-oma.tgz | /uploads/ccloud-cli-2.2.1-runtime.tgz | /mnt/session/uploads/ccloud-cli-2.2.1-runtime.tgz |
| ccloud-cli | /uploads/ccloud-cli-guard | /mnt/session/uploads/ccloud-cli-guard |
| scripts/install_runtime.py | /uploads/install_ccloud_runtime.py | /mnt/session/uploads/install_ccloud_runtime.py |
| scripts/l4_validate.py | /uploads/l4_validate.py | /mnt/session/uploads/l4_validate.py |

安装器校验归档 SHA、每个文件 SHA、Node 版本及安全路径；解包至 `/tmp/oma-ccloud`，在 bin 放固定 argv guard。guard 不是安全边界，代理在服务端再次强制范围。保持 CLI metadata 冷缓存可验证真实三请求链。

## 更新现有 Go 日报

只追加现有资源，保留 config RO memory、state RW memory、iacctl-go file、原 OBS_AUTH Vault、原 cron `30 8 * * 1-5 / Asia/Shanghai`、原初始事件、metadata 和预算。不要覆盖环境 packages/init_script/resources；读取完整现有环境后，仅在 `config.networking.allowed_hosts` 追加 `sre-control.test.seewo.com`。无需允许 OMA 直连 CCloud。

新建只包含代理窄令牌的 Vault：暴露变量名 `CCLOUD_API_KEY`，仅允许 `sre-control.test.seewo.com`；若使用 HTTP header 替换，Authorization 为 `Bearer ${CCLOUD_API_KEY}`。不得将服务端 CCloud key 放入该 Vault。系统片段见 [system-append.md](system-append.md)，在旧 system 后追加并发布新 Agent version；旧 Agent 与现有其他设置保留。

```bash
python3 scripts/render_additions.py \
  --deployment /secure/daily-deployment-before.json --output /secure/daily-deployment-update.json \
  --agent-version 2 --runtime-file-id file_RUNTIME --guard-file-id file_GUARD \
  --installer-file-id file_INSTALLER --validator-file-id file_VALIDATOR --proxy-vault-id vlt_PROXY
```

渲染工具只输出保留情况，文件权限0600，不 apply。输出需操作员按当前 OMA API schema 复核再提交；schedule 只传 type/expression/timezone，剔除响应中的运行时间字段。更新后 GET 回读比较旧资源、Vault、cron、预算和初始事件，确认仅新增资源/窄Vault及版本变化。

## 手工验收

同一个新版本 Agent、原环境和新增资源启动一次手工会话，初始 `user.message` text 为 JSON：

```json
{"mode":"l4_validation_only","instruction":"仅固定L4取证，不发送飞书，不读取webhook，不写memory或ledger"}
```

该模式仅安装并执行固定 `l4_validate.py`，最终给出只读 JSON；原 cron 事件不带此 mode，继续日常报告。读取工具输出与最终报告核对一致，确认实际调用 ccloud-cli，source 是只读代理；失败必须 unknown，不能靠旧结果通过。人工回查 memory/ledger 未新增验证记录、未发送飞书。随后在正常日报执行中核对 L4 来源已切到 ccloud-cli，并保留原业务报告能力。

pods 列表只证明当前观测数量、Ready 与重启。没有当前 desired 副本证据，因此 desired_replicas 必须 null/unknown；旧 Go pack 的 expect_replicas=5 不证明当前应有5个，也不能把列表4/4 Ready等同于当前 desired 已满足。

## 本地验证

```bash
python3 scripts/test_package.py -v
IAC_CCLOUD_CLI_PATH=/tmp/oma-ccloud/ccloud-cli/dist/ccloud-cli.js GOCACHE=/private/tmp/alert-iac-gocache go test -race ./internal/iacccloudproxy -count=1 -v
GOCACHE=/private/tmp/alert-iac-gocache go vet ./internal/iacccloudproxy
```

第二、三条在仓库根目录执行。未设置 CLI path 时真实 CLI 测试会跳过，不能把该跳过当作冷缓存链已验收。
