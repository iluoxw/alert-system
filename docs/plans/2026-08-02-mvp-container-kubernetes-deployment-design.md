# SRE Control v0.1.0 容器与 Kubernetes 部署设计

## 目标

交付首个可重复部署的 MVP 基线：开发者可用一条 Make 命令复现 CI，使用同一份根
Dockerfile 构建 API、Web 和数据库迁移镜像，并在本地 Kubernetes 集群得到包含
PostgreSQL 的完整 SRE Control。Alertmanager、飞书和 SMTP 等外部系统仍通过 Secret
配置，不把任何明文凭据写入镜像或通用 manifests。

## 架构选择

根 Dockerfile 提供三个独立 target：`api` 是非 root Go 运行时，同时携带一次性
`seed-admin` 工具；`web` 是非 root nginx 静态站点；`migrate` 携带版本化 SQL 和
golang-migrate。所有镜像统一通过 Docker Buildx 构建：本地单平台构建使用 `--load`
进入当前 Docker image store，仓库发布使用 `--push` 直接生成多架构 manifest list。
Kubernetes 将 Web 与 API 放入同一 Pod，nginx 在 `:8080` 提供 SPA，
并把 `/api`、`/ingest`、`/callbacks` 和健康检查代理到 Pod 内 `:8081` 的 API。这保留了
浏览器同源 cookie 和 webhook URL 语义，也让 MVP 只有一个对外 Service。

每个应用 Pod 在启动前由 migrate initContainer 执行 `up`。迁移失败时业务容器不会
启动；重复执行由 schema migration ledger 保证幂等。正常恢复和升级都不依赖
PostgreSQL 官方镜像的“一次性 initdb”行为。管理员初始化不是启动依赖，使用单独 Job
显式执行，避免每次滚动发布都重置密码。

## 部署边界

`deploy/k8s/base` 是与数据库实现无关的应用基线；`overlays/local` 增加单节点
PostgreSQL、开发 Secret 和本地镜像名，用于 kind、Docker Desktop 或 OrbStack。
本地 Secret 明确只供开发，生产部署必须换成外部 Secret 管理与受管 PostgreSQL。

应用 Deployment 默认单副本，配置 startup/liveness/readiness probe、资源 request/
limit、只读根文件系统、drop all capabilities 和 seccomp。Service 使用 ClusterIP；
本地通过 `make k8s-port-forward` 暴露到 `localhost:5173`，不预设任何特定 Ingress
Controller。数据库使用 StatefulSet 和 PVC，数据卷不会随 Pod 重建而丢失。

## 验收

发布检查由 `make release-check` 串联：Go format/build/vet/完整 integration+E2E race
测试、npm ci/build/lint、生产依赖 audit、三个镜像构建与入口 smoke test、Kustomize
渲染和 kubectl 客户端校验。镜像构建命令必须是 `docker buildx build`，本地验收必须
显式 `--load`，避免只留在 BuildKit cache 而导致 smoke/kind 阶段找不到镜像。当前 npm
audit 的两个 high 均来自 React Router RSC Mode
的服务端 Action CSRF；本项目是纯 Vite SPA，没有 RSC/Server Action 入口，因此
v0.1.0 记录风险但不做破坏性降级，release-check 仍对 critical 漏洞硬阻断。集群验收
要求迁移 initContainer 成功、PostgreSQL 与 Deployment rollout 完成、`/healthz` 和
`/readyz` 返回 200，并能通过 seed-admin 登录 Web 控制台。
