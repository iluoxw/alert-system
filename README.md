# alert-system

当前发布基线：**SRE Control v0.3.2**（版本源见 [`VERSION`](VERSION)，发布记录见 [`CHANGELOG.md`](CHANGELOG.md)）。v0.3.2 仅发布 Web，API 与迁移镜像沿用 v0.3.1；发布后的修复列在 CHANGELOG 的 Unreleased 节，不代表已部署。

Go 编写的 Infrastructure-first 事故响应系统：从 Alertmanager / Grafana 同步已触发的 Alert，绑定实体后形成 Fault、Impact 与 Incident，支撑故障认领、通知、协作处置与恢复。Alertmanager / Grafana 是各自 Alert 生命周期的权威来源；本地路由、分组、静默、抑制仅对 generic 来源生效，相关能力已冻结。

## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.25、gin v1.12、pgx v5.7（不用 ORM）、PostgreSQL 16 |
| 队列/定时器 | 无外部 MQ，统一用 PG `timers` 表 |
| 前端 | Vite + React 19 + TypeScript + AntD 5.29.3 + React Query 5 + react-router-dom 7 |
| 测试 | go test + testcontainers-go（store 集成测试）、`test/e2e` 真实 PG 场景化 E2E |

## 本地快速开始

1. 启动本地 Postgres（命名卷持久化，端口 5433，避免与其他项目占用的 5432 冲突）：

   ```bash
   docker compose -f deploy/docker-compose.yml up -d --wait
   ```

2. 引导一个管理员账号（系统没有注册入口，密码用 bcrypt 存储，无法手写 SQL 建账号；
   可重复执行，用于重置密码）：

   ```bash
   go run ./cmd/seed-admin -password '你的密码'
   ```

3. 加载环境变量并启动后端（服务端不读取 `.env` 文件本身，需先 source 到 shell）：

   ```bash
   cp .env.example .env   # 首次执行，之后按需修改
   set -a; source .env; set +a
   SESSION_COOKIE_SECURE=false go run ./cmd/server
   ```

4. 启动前端（dev 模式下 `/api` 与 `/ingest` 都会被 Vite 代理到 `:8080`）：

   ```bash
   cd web && npm install && npm run dev
   ```

## 飞书配置

飞书卡片是主要通知渠道，同时承接处置回调，需要以下四个环境变量（均在
`.env.example` 中留了空位）：

| 变量 | 用途 |
|---|---|
| `LARK_APP_ID` / `LARK_APP_SECRET` | 飞书自建应用凭据，用于调用开放平台 API 发送卡片消息 |
| `LARK_VERIFICATION_TOKEN` | 校验飞书回调请求的真实性（事件订阅/卡片交互回调） |
| `LARK_ENCRYPT_KEY` | 若飞书后台开启了加密，用于解密回调 payload |

飞书卡片交互回调地址是 `/callbacks/lark/card`，需要飞书开放平台能访问到（本地联调
需要内网穿透工具把该地址暴露为公网可达）。

卡片动作只接受已验签且已绑定 Firebreak 用户的请求。管理员在“设置 → 用户”中维护
飞书 `open_id`；当前支持 Alert 的认领/解决、Fault 的逐级推进、Incident 的进入处置。
每次点击使用 `request_id` 幂等，并在 `lark_card_actions` 中记录操作和结果快照。详细
边界与后续 Agent 扩展点见
[`docs/plans/2026-08-13-feishu-card-action-design.md`](docs/plans/2026-08-13-feishu-card-action-design.md)。

## 告警接入（webhook）

外部系统（Alertmanager / Grafana / 自定义脚本）通过 webhook 推送已触发的告警事件：

```
POST /ingest/v1/hook/{hook_uuid}
```

`hook_uuid` 由 SRE Control 在登记 Source 时生成，URL 本身同时完成来源识别与鉴权；
库内只存 `hex(sha256(hook_uuid))`，明文 URL 仅在创建或轮换时显示一次。轮换不会改变
稳定的 `source_id`，但旧 URL 会立即失效。该路径是唯一 Alert webhook 入口；不支持
`source_id` 路径或 Authorization/X-Ingest-Token Header 兼容模式。

Alertmanager receiver 示例：

```yaml
webhook_configs:
  - url: 'https://sre-control.example/ingest/v1/hook/00000000-0000-4000-8000-000000000000'
    send_resolved: true
```

当前支持 `alertmanager`、`grafana`、`generic` 三种 adapter。入站 Source URL 与飞书群
机器人等出站通知 URL 是两个独立凭据域，禁止互相复用或写入对方配置。
本地 route / group / silence / inhibit 仅对 `generic` 来源生效；Alertmanager 与
Grafana 是 Alert 生命周期权威来源，不经过这套本地编排。

## 测试

```bash
go build ./... && go vet ./...          # 必须零错误
go test -tags=integration ./... -race -count=1  # 含 store 集成测试与 E2E，需 Docker
go test ./test/e2e/...                 # 自动管理测试专用 PG；同一时刻只运行一轮
cd web && npm run build && npx oxlint   # 前端构建 + lint
```

也可以直接复现仓库 CI：

```bash
make ci
```

发布前完整检查会在 CI 基础上继续构建三个镜像、检查镜像入口并校验 Kubernetes
manifests：

```bash
make release-check
```

## 容器镜像

根目录 `Dockerfile` 提供三个独立 target，均以非 root 用户运行：

| 镜像 | 默认标签 | 用途 |
|---|---|---|
| API | `sre-control-api:$(VERSION)` | Go API、后台 worker，并携带一次性 `seed-admin` 工具 |
| Web | `sre-control-web:$(VERSION)` | 静态 SPA 与运行时 `API_BASE_URL` 配置 |
| Migrate | `sre-control-migrate:$(VERSION)` | golang-migrate + 当前版本 SQL |

上表是 Makefile 的本地构建默认值，`VERSION` 取根目录版本文件；已发布镜像的版本组合以 CHANGELOG 为准。仓库 Kubernetes 示例仍固定为 `0.1.0`，使用前须显式设置镜像仓库与版本，不能直接视为当前发布配置。

```bash
make docker-build
make docker-smoke
```

镜像统一由 Docker Buildx 构建。本地目标使用 `--load` 将单平台结果写入当前 Docker
image store，保证后续 smoke test 和 `kind load` 能直接读取；如需指定本地架构可设置
`DOCKER_LOCAL_PLATFORM=linux/amd64`，如需指定 builder 可设置
`DOCKER_BUILDX_BUILDER=<name>`。

默认构建使用 `registry.gz.cvte.cn/library` 下的 Go、Alpine 和 Node 基础镜像，Go/NPM
依赖分别通过 `goproxy.cn`、`registry.npmmirror.com` 获取；所有地址都可通过 Makefile
同名变量覆盖。Web 运行时和迁移镜像都从内网基础镜像构建，不依赖 Docker Hub。

推送到镜像仓库时显式指定仓库前缀；该目标直接通过 Buildx `--push` 发布。当前内网
基础镜像基线默认只发布 `linux/amd64`，待全部基础镜像具备多架构 manifest 后再显式
覆盖 `DOCKER_PUSH_PLATFORMS`：

```bash
IMAGE_REGISTRY=registry.gz.cvte.cn/ai-tools make docker-push
# 失败后可只重试一个 target：make docker-push-web IMAGE_REGISTRY=...
# 基础镜像就绪后发布多架构：DOCKER_PUSH_PLATFORMS=linux/amd64,linux/arm64 IMAGE_REGISTRY=... make docker-push
```

镜像不包含数据库、飞书或 SMTP 凭据；所有敏感配置只在运行时通过环境变量或
Kubernetes Secret 注入。

Web 镜像必须在运行时设置 `API_BASE_URL`。前端只采用一种链路：浏览器直接请求该
地址，容器内 Nginx 只提供静态文件，不代理 API。变量可使用同源路径（推荐 `/api`）
或带协议的公开地址（例如 `https://sre-control.example.com/api`）；前端会在其后拼接
`/v1`。缺失、裸域名或含非法字符时容器会直接退出。

使用 `/api` 时，外部 Ingress/Gateway 或平台负载均衡必须将 `/api` 路由到 API
Service 的 `8081` 端口，并将 `/` 路由到 Web 的 `80` 端口；`/ingest` 与
`/callbacks` 也必须直接路由到 API。使用不同 origin 的绝对地址时，API 或网关还要
通过 `CORS_ALLOWED_ORIGINS` 配置允许凭据的精确 Web origin（逗号分隔），不能使用
`*`；跨站点部署还必须同时评估 session cookie 的 SameSite 策略，推荐保持同站点。
集群内部地址（如
`firebreak-server:8081`）无法被用户浏览器解析，不能作为 `API_BASE_URL`。

## Kubernetes 部署

`deploy/k8s/base` 是应用基线；`overlays/local` 自带单节点 PostgreSQL 和仅供本地开发
的 Secret，适用于 kind、Docker Desktop 或 OrbStack。先构建镜像；kind 还需要把本地
镜像加载进集群：

```bash
make docker-build
make kind-load                     # 非 kind 集群跳过
make k8s-deploy
make k8s-wait
make k8s-bootstrap-admin           # 本地默认 admin/admin123
make k8s-port-forward              # Web http://localhost:5173；API http://localhost:8080
```

local overlay 将 Web 的 `API_BASE_URL` 设为 `http://localhost:8080/api`，并仅允许
`http://localhost:5173` 跨 origin 携带登录 cookie；这两个端口必须同时转发。

本地 Secret 中的固定口令只能用于个人开发集群。生产部署使用
`deploy/k8s/overlays/production`，先将镜像名改为真实仓库，并通过 External Secrets、
Sealed Secrets 或平台 Secret 管理能力创建 `sre-control-secrets`。示例字段见
`secret.example.yaml`，该文件不参与 kustomization。

若平台托管的 Deployment 没有迁移 initContainer，使用 [`deploy/k8s/jobs/migrate.example.yaml`](deploy/k8s/jobs/migrate.example.yaml) 准备一次性迁移 Job。先复制到仓库外，填写 namespace、镜像、拉取凭据和数据库 Secret，再人工执行并核对结果；该模板不参与 overlay，不会随应用自动部署。环境专用的 `uat-migrate-0-3-1.yaml` 保留为本地运维文件，不作为通用部署基线。

应用 Pod 包含 Web 与 API 两个容器，Service 分别暴露 `80`（Web）与 `8081`（API），
路径路由由集群入口层完成；数据库迁移由 initContainer 在业务启动前执行。
新数据库会自动创建 `schema_migrations`。若目标数据库过去通过 `deploy/initdb` 手工应用
过 SQL、但没有 migration ledger，必须先人工核对并建立正确 baseline，不能直接执行
`migrate up` 猜测版本。

## 目录结构

| 目录 | 说明 |
|---|---|
| `cmd/server/` | 服务入口 |
| `cmd/seed-admin/` | 初始管理员引导 CLI |
| `internal/app/` | 依赖装配（wiring），`cmd/server` 与 `test/e2e` 共用 |
| `internal/domain/` | 纯函数领域层：状态机、matcher、指纹 |
| `internal/store/` | 仓储层（pgx，事务由调用方开） |
| `internal/pipeline/` | 管道 stage：去重/路由/分组/静默/抑制 |
| `internal/ingest/` | webhook 接入与 Alertmanager/Grafana 适配器 |
| `internal/scheduler/` | 统一定时器（PG `timers` 表驱动） |
| `internal/oncall/` | 值班排班与人员升级链 |
| `internal/notify/` | 通知策略、渠道与生命周期（重复提醒等） |
| `internal/api/` | HTTP 层：鉴权、RBAC、REST 端点 |
| `migrations/` | golang-migrate 格式迁移文件 |
| `deploy/` | 本地开发用 docker-compose（PG，端口 5433） |
| `deploy/k8s/` | Kubernetes base、local/production overlay 与管理员引导 Job |
| `test/e2e/` | docker-compose + 真实 PG 的场景化 E2E |
| `web/` | 前端（Vite + React + AntD） |

## 外部分析客户端（M1）

如需按角色把一组 Alertmanager 告警交给本地分析程序，在服务端设置 `AGENT_DELIVERY_CONFIG`，指向[分发配置](docs/examples/agent-delivery.yaml)。先应用数据库迁移，再填写已登记来源的稳定 source ID、匹配规则、每角色的 TTL 和客户端身份。配置在服务启动时加载。未设置此变量的实例按原有方式接入告警。

为每个客户端分配独立的随机 bearer token。服务端配置只保存 token 的 SHA-256 十六进制摘要，客户端从配置引用的环境变量读取明文。轮换时可为同一客户端和角色短暂配置两个不同摘要，更新客户端后移除旧摘要并重启服务实例。生产部署通过 TLS 入口暴露 `/api/v1/agent/`。

```bash
go build -o ./agent-client ./cmd/agent-client
./agent-client -config /etc/sre-control/agent-client.yaml
```

[客户端配置示例](cmd/agent-client/example.yaml)指定并发、匹配条件、固定可执行文件及超时。上游组数据和投递意图一起入库，客户端按空闲槽位领取，确认 ACK 后将脱敏的 Envelope JSON 通过 stdin 交给 handler。ACK 前断线会在租约到期后重新分配任务，ACK 后崩溃或 handler 超时可能漏掉该次分析。critical 告警仍通过独立通知渠道送达值班人。协议与验收见 [M1 设计](.spec-workflow/specs/agent-delivery/design.md)。

客户端需要支持进程组的 Unix 环境。每次领取至多 8 条，每条租约最长 30 秒。handler 输入包含来源 ID、事件类型、接收器和告警组的标识摘要，以及各成员的标识摘要、告警名、状态、级别、起止时间和截断数量。同一来源的 firing 与 resolved 可通过 `group_ref` 对照。上游缺少 `groupKey` 时，组标识按已收到的成员计算。自由文本 annotations、任意 labels 和内部 URL 留在服务端。默认日志只记录消息 ID、退出码、耗时和输出字节数。需要查看输出时，配置仅运行用户可访问的 `result_dir`，每次执行分别保留 stdout 和 stderr 的前 8 KiB。

排查积压或未执行的任务时，可使用只读数据库账号执行以下查询。终态任务及其审计事件保留 7 天。

```sql
SELECT role, status, count(*) AS messages, min(created_at) AS oldest_enqueued_at
FROM agent_messages GROUP BY role, status ORDER BY role, status;

SELECT status, count(*) AS batches, max(failures) AS most_failures
FROM agent_batches GROUP BY status;

-- $1 为客户端日志中的 message_id
SELECT at, kind, from_status, to_status, actor, detail
FROM agent_message_events WHERE message_id = $1 ORDER BY id;
```
