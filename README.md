# alert-system

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

4. 启动前端（dev 模式下 `/api` 会被 Vite 代理到 `:8080`）：

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
POST /ingest/{integration_id}
Authorization: Bearer <integration token>
```

`integration_id` 与 token 通过管理 API 创建（token 明文只在创建响应中返回一次，
库内只存 `hex(sha256(token))`）。当前支持三种 adapter kind：`alertmanager`、
`grafana`、`generic`（自定义 JSON 事件格式）。

## 测试

```bash
go build ./... && go vet ./...          # 必须零错误
go test ./... -race -count=1            # 全量单测（store 用 testcontainers 需 Docker）
cd test/e2e && docker compose up -d --wait && cd - && go test ./test/e2e/...   # 真实 PG 场景化 E2E
cd web && npm run build && npx oxlint   # 前端构建 + lint
```

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
| `test/e2e/` | docker-compose + 真实 PG 的场景化 E2E |
| `web/` | 前端（Vite + React + AntD） |
