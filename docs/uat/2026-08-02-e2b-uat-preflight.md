# E2B UAT 实机验收预检（2026-08-02）

## 结论

状态：**BLOCKED — 当前没有可证明隔离的 E2B UAT 环境，禁止执行会创建 Sandbox 的生命周期验收。**

本次只做只读预检，没有创建 Sandbox、没有修改 Nomad/Consul、没有调整节点 eligibility、没有变更 DNS/证书/对象存储，也没有触碰生产租户。

## 已确认事实

- 当前进程可见的 `E2B_API_URL` 是生产入口 `https://api.agi.exc.show`；它不能用于自动化 UAT。
- 没有专用的 `E2B_UAT_TEMPLATE_ID`、`E2B_TEMPLATE_ID` 或 `E2B_TEST_TEMPLATE_ID` 配置。
- Nomad/Consul 只有 `dc1`：三个 server 正常，`api`、`client-proxy`、`orchestrator`、`template-manager` 四个作业都运行在生产命名空间/节点池。
- 候选节点 `10.60.255.92`、`10.60.255.93` 仍是 eligible/ready 的生产 `default` pool 节点，并承载生产 orchestrator allocation，不能直接拿来做演练靶机。
- 远端不存在 `/home/cloud/20260614/e2b-self-hosting/ansible/inventories/uat`。
- `api.uat.agi.exc.show` 当前解析到 `10.60.255.86`，这是现有生产 ingress 节点；其 `/health` 还返回自签 TLS 链。域名含 `.uat` 不能证明环境隔离。
- 没有确认独立的 UAT PostgreSQL、Redis、ClickHouse、S3 bucket、TLS 证书、模板和 API 凭据。名称类似 `uat-e2b-templates` 的 bucket 不能替代独立状态层证明。

## 已完成的代码侧安全能力

服务默认仍使用 `DisabledExecutor`。只有同时提供启动参数 `-enable-e2b-uat-runbooks` 和以下专用环境变量，生命周期动作才可能进入执行器：

- `E2B_UAT_API_URL`：必须为 HTTPS，host 必须包含独立的 `uat` DNS label，且不能是已知生产 API。
- `E2B_UAT_API_ALLOWED_IPS`：逗号分隔的精确 UAT ingress IP；每次拨号重新解析 DNS并只连接白名单 IP，不使用进程代理。
- `E2B_UAT_API_KEY`：专用 UAT key；不回退读取生产 `E2B_API_KEY`，不进入浏览器、请求参数、日志或审计结果。
- `E2B_UAT_ENVIRONMENT_ID`：隔离环境标识。
- `E2B_UAT_TEMPLATE_IDS`：服务端模板白名单。
- `E2B_UAT_AUTHORIZATION_REF`：单次变更授权引用。
- `E2B_UAT_AUTHORIZATION_EXPIRES_AT`：RFC3339 格式的授权过期时间。

执行请求还必须由当前 Incident 的 Commander 或 Technical Lead 发起，并逐项匹配环境、授权和模板，显式确认自动清理。执行器只调用类型化 API：

`create → ready → pause → resume → verify → delete`

Sandbox 创建成功后，正常完成、步骤失败或调用 context 取消都会用独立短超时尝试 delete。审计仅保存 Sandbox ID 的不可逆短哈希；清理失败会把整次验收判为失败。HTTP 重定向、非白名单 host/IP、明文 HTTP、生产 API 和请求中的 `api_key`/`command`/`keep` 均被拒绝。

基于本次预检，`api.uat.agi.exc.show` 与生产 `api.agi.exc.show` 均已加入代码级拒绝列表。未来若重建后仍计划使用前一个域名，必须先用新的 inventory、状态层、DNS 解析、证书和专用凭据重新证明隔离，再通过受审代码变更解除该安全锁，不能只修改启动参数绕过。

## 要解除阻塞必须补齐

1. 选择并批准 UAT 资源方案：
   - A：从现有生产容量 drain 两个节点后改建 UAT；有生产容量影响，需要单独变更与回退计划。
   - B：从现有生产容量 drain 三个节点后改建 UAT；隔离余量更好，但生产影响更大。
   - C：申请新 VM 建立 UAT；对生产零扰动，等待资源交付。
2. 建立独立 inventory、Nomad/Consul 边界或等价的强隔离边界。
3. 建立独立 PostgreSQL、Redis、ClickHouse 和对象存储 bucket，禁止与生产共享状态。
4. 配置独立 DNS/TLS、精确 ingress IP 白名单和专用 API key。
5. 构建仅供演练的模板，将其 ID 加入服务端白名单。
6. 创建有时效的变更授权，明确执行窗口、观察指标、停止条件、清理负责人和回退方式。
7. 预检再次通过后，在 Incident Room 执行一次生命周期验收，并核对 Runbook 结果、统一时间线、资源清理和 UAT 状态通报。

## 下一决策

在 A/B/C 中选择一种，或提供已经存在但本次预检未发现的隔离 UAT endpoint、精确 IP、模板与授权。选择前，阶段 7 保持阻塞，不对生产执行替代性验收。
