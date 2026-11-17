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

安装器校验受信归档 SHA、清单完整性、`package.json` 与 `BUNDLE.json` 的 name/version、每个文件内容、Node >=18.15 及安全路径；解包至 `/tmp/oma-ccloud`，在 bin 放固定 argv guard。安装后再次回读所有文件，确认 guard 内容、可执行权限及空参数拒绝行为。子进程只运行宿主 `node --version` 与无参数 guard 拒绝自检，使用最小环境，不传递凭据、HOME 或 NODE_OPTIONS；不执行官方 CLI 的 JavaScript 入口，不联网。保留 `installed=true` / `file_hashes_verified=true` 输出，并增加 `guard_verified=true`。guard 不是安全边界，代理在服务端再次强制范围。

离线安装验收已整合进原安装器，不需要增加 OMA resource。`scripts/verify_runtime.py` 提供可选的**只读复验**，复用同目录安装器函数；默认读取原四文件布局的受信归档与 guard，检查 `/tmp/oma-ccloud`。将两脚本放在同一目录时可运行：

```bash
python3 scripts/verify_runtime.py \
  --archive /mnt/session/uploads/ccloud-cli-2.2.1-runtime.tgz \
  --sha256 df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87 \
  --guard /mnt/session/uploads/ccloud-cli-guard
```

以上参数均有对应挂载默认值，目标固定为 `/tmp/oma-ccloud`。任何版本、文件或 guard 不符、缺少 Node 均失败退出；不能忽略失败继续。归档更新后须同时更新可信 SHA；guard 变更须审核并更新安装器中的 guard 摘要。

**本地安装、metadata 连通与认证、L4 取证是三个独立验收阶段。** 官方 `node .../dist/ccloud-cli.js --version` 也可能初始化动态 metadata，在无凭据的受限网络环境中可能返回 403，单凭状态码不能区分网络策略和上游鉴权拒绝；这不等于离线包安装失败，因此不得将其作为安装判据。仅 `node --version` 检查 Node 运行时本身。metadata 连通与认证通过后，还必须以冷缓存核对真实三请求链与 L4 返回值，才能认定本次只读取证完成。专用只读身份未提供、代理未启用时，不能声称 L4 已完成；缺 token 的 L4 返回 unknown，desired_replicas 为 null。禁止自动配置凭据、使用管理员 key 或新增 Kubernetes Secret。

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
python3 -m unittest discover -s scripts -p 'test_*.py' -v
# 可选：指定仅含公开 npm 包的目录，执行真实离线包验收；不读取用户认证目录
CCLOUD_TEST_PACKAGE_DIR=/path/to/public/@cvte/ccloud-cli python3 scripts/test_verify_runtime.py -v
IAC_CCLOUD_CLI_PATH=/tmp/oma-ccloud/ccloud-cli/dist/ccloud-cli.js GOCACHE=/private/tmp/alert-iac-gocache go test -race ./internal/iacccloudproxy -count=1 -v
GOCACHE=/private/tmp/alert-iac-gocache go vet ./internal/iacccloudproxy
```

Go 命令在仓库根目录执行；Python 命令在本目录执行。未提供 `CCLOUD_TEST_PACKAGE_DIR` 时真实包用例会跳过，不能把跳过当作真实包已验收。未设置 CLI path 时真实 CLI 测试会跳过，不能把该跳过当作冷缓存链已验收。
