# alert-system

当前可部署基线：**SRE Control v0.1.0**（版本源见 [`VERSION`](VERSION)）。

Go 编写的告警管理系统（MVP）：接入已触发的告警事件（metric/log/trace 归一为统一
事件模型），经过 去重 -> 路由 -> 分组 -> 静默 -> 抑制 五段管道处理后，按值班排班与
升级策略通知责任人（飞书卡片为主要通知渠道），支持处置（Ack/Resolve）与自愈预留。
架构大量借鉴 Alertmanager 与 PagerTree 的实践。

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

## 测试

```bash
go build ./... && go vet ./...          # 必须零错误
go test ./... -race -count=1            # 全量单测（store 用 testcontainers 需 Docker）
cd test/e2e && docker compose up -d --wait && cd - && go test ./test/e2e/...   # 真实 PG 场景化 E2E
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
| API | `sre-control-api:0.1.0` | Go API、后台 worker，并携带一次性 `seed-admin` 工具 |
| Web | `sre-control-web:0.1.0` | nginx SPA 与同源反向代理 |
| Migrate | `sre-control-migrate:0.1.0` | golang-migrate + 当前版本 SQL |

```bash
make docker-build
make docker-smoke
```

镜像统一由 Docker Buildx 构建。本地目标使用 `--load` 将单平台结果写入当前 Docker
image store，保证后续 smoke test 和 `kind load` 能直接读取；如需指定本地架构可设置
`DOCKER_LOCAL_PLATFORM=linux/amd64`，如需指定 builder 可设置
`DOCKER_BUILDX_BUILDER=<name>`。

推送到镜像仓库时显式指定仓库前缀；该目标直接通过 Buildx `--push` 发布
`linux/amd64,linux/arm64` manifest list：

```bash
IMAGE_REGISTRY=registry.example.com/team make docker-push
# 只发布 amd64：DOCKER_PUSH_PLATFORMS=linux/amd64 IMAGE_REGISTRY=... make docker-push
```

镜像不包含数据库、飞书或 SMTP 凭据；所有敏感配置只在运行时通过环境变量或
Kubernetes Secret 注入。

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
make k8s-port-forward              # http://localhost:5173
```

本地 Secret 中的固定口令只能用于个人开发集群。生产部署使用
`deploy/k8s/overlays/production`，先将镜像名改为真实仓库，并通过 External Secrets、
Sealed Secrets 或平台 Secret 管理能力创建 `sre-control-secrets`。示例字段见
`secret.example.yaml`，该文件不参与 kustomization。

应用 Pod 包含 Web 与 API 两个容器；数据库迁移由 initContainer 在业务启动前执行。
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
