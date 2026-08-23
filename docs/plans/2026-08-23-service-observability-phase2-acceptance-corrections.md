# Service Observability Phase 2 Acceptance Corrections Plan

> **Owner:** Codex 负责计划、调度与本地验收；Cursor Cloud Fable 5 High 只审计划；Cursor Cloud Claude Opus 5 High 编码；Grok Bot `alert-system` 在最终 exact SHA 上做对抗性 code review。
>
> **固定 Cursor 入口：** 所有 Fable 计划审查与 Opus 编码 follow-up 都必须在同一个 Cursor Cloud Agent 会话 `https://cursor.com/agents/bc-5a993b86-e20a-46b1-b033-028118eced93` 内完成；不得新建 Cursor 会话，也不得使用本地 Cursor 客户端。

## 1. 目标

在 `6e4eddddd8e13ee505639ddadd6619d7afa1ecee` 已通过全量测试的基础上，关闭 Codex 本地验收发现的查询真实性、安全边界与首个 E2B Demo 可用性缺口，再进入 Grok exact-SHA review。

本轮不是扩展 Phase 2 产品范围。它只修正已经承诺但尚未严格满足的出口条件：

1. URL 是目录筛选、总览窗口与工作区 focus 的可分享事实源；非法输入必须显式 422，不能被前端吞掉后展示另一个成功结果；
2. fixture 模式仍是一等 E2B Demo，重新执行 focus 后，输入、URL、Journey、bounded path 与诊断结论必须一致；
3. opaque cursor、timezone 与页面链接必须 fail closed；
4. 1536px 请求路径/依赖面板达到画稿要求的可读层级和内容密度，同时保持 1180/900/390px 既有响应式能力；
5. 缺失、失败、Forbidden 或过期数据继续只在最小对象范围降级为 Unknown。

## 2. 不变约束

- 生产主落地是 `alert-system`；E2B 是首个 Service Profile，不是核心特例。
- fixtures 优先且只允许 `import.meta.env.DEV && VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE=true`；API 失败不得回退 fixture。
- Agent 暂缓；不增加 Agent、Assistant、prompt、session、memory 或 stream UI/API。
- 不连接 Prometheus、OpenObserve 或其它生产遥测源。
- 不修改 Kubernetes/OTel 配置，也不触碰以下 hold paths：
  - `deploy/observability/`
  - `docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md`
  - `docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md`
- 不伪造 sparkline、节点运行数字、事件或 observed relation；视觉修正只能重排/强化合同已经提供的事实。
- 当前分支串行修改；不与其它编码 Agent 并发写共享文件。
- Cursor 的模型可以按回合在固定 Cloud 会话内切换，但任务上下文、目标分支与审查记录不得拆到其它会话。

## 3. 已验证问题与处置

### C1：前端静默改写非法 URL 查询

**证据**

- `ServiceDirectory.tsx` 只把合法 `sort` 放入 API query；非法 `sort` 被省略。非法 `direction` 被归一为 `asc` 或省略。
- `ServiceOverview.tsx` 把 `window_seconds=0`、负数或非数字转换成 `undefined`，因此 API 收到的是缺省窗口并返回成功页。
- 这与 Phase 2 计划中“URL 是事实源”和后端稳定 422 reason code 相冲突。

**修正**

1. 把 URL 原始 `sort`、`direction`、`window_seconds` 传到 live API；客户端类型不得用过窄 union 迫使调用方先吞掉非法值。
2. UI 的排序图标/窗口展示可以只解析合法值，但不得改变送往服务器的原始请求。
3. fixture provider 实现与后端同口径的轻量 query validation，并抛出 `ApiError(422, ..., reason_code)`，使 fixture/live 的错误页面一致。
4. 补回归：非法 sort、direction、window、health/data enum 都显示明确错误；合法 URL 与前进/后退恢复不退化。

### C2：fixture focus 重新查询后出现 URL 与视图矛盾

**证据**

- fixture provider 当前完全忽略 `ServiceWorkspaceQuery`。
- Empty scenario 中手工输入新 focus 后，URL/Input 可以是 `sbx_missing`，但 Journey 卡仍显示 patch 中固定的 `sbx_notfound1`；保留 `scenario` 时重新执行诊断也不会改变结果。
- live region 在 fixture 主态错误宣称“已加载服务端聚合视图”。

**修正**

1. `runDiagnosis` 在显式 fixture 模式清除 QA `scenario`；live 模式保持只 refetch/query，不引入 scenario 概念。
2. fixture provider 根据通用 base view 的 primary focus 判断：请求值与 base focus 相同返回 main；合法但不同的值应用既有 Empty scenario，并把返回 view 的 `focus.value` 与可见 Journey identity 同步为请求值。不得在 renderer 中写 E2B ID 分支。
3. 显式 `scenario=` 仍用于确定性的五态视觉 QA；进入场景时 URL 应规范化为该 scenario view 的 focus，禁止保留互相矛盾的 focus 参数。
4. fixture live region 明确“fixture 场景/fixture 主视图”；不得冒充服务端 live 数据。
5. 覆盖 main → Empty → main、浏览器前进/后退与 URL reload；保留既有显式 QA 恢复语义：Partial/Stale 的恢复动作可回到 main，Forbidden 不出现全局重试，而重新执行 focus 必须离开旧 scenario。

### C3：游标解码未拒绝尾随 JSON

**证据**

- `decodeCursor` 使用一次 `decoder.Decode` 和 `DisallowUnknownFields`，但没有第二次读取 EOF；合法 payload 后拼接第二个 JSON 值可能被接受。

**修正**

1. 第一次 decode 后必须再 decode 到 `io.EOF`；任何尾随 token、空白以外字节或第二个 JSON 值均返回 `ErrInvalidCursor`。
2. 增加合法尾随空白、拼接对象、拼接标量、未知字段、超长/base64/version/fingerprint 的表驱动测试。

### C4：timezone 只校验形状，不校验可用时区

**证据**

- API regex 接受形似 `Mars/Olympus` 的字符串；前端 `Intl.DateTimeFormat` 失败后退回原始 UTC 文本。
- API 已定义 `invalid_timezone`，因此“格式合法但不可加载”不应成为成功响应。

**修正**

1. 在 API 边界对非空 timezone 调用 `time.LoadLocation`；未知 zone 返回 422 `invalid_timezone`。
2. `UTC` 与有效 IANA timezone 保持通过；timezone 仍只影响显示，不改变 from/to 查询边界。
3. `WorkspaceContext` 的 Evidence 时间必须使用 `view.scope.timezone`，消除局部仍固定 Asia/Shanghai 的显示漂移。

### C5：登记 URL 可进入不安全 href

**证据**

- Service Registration 只 trim `dashboard_url/runbook_url`，Page View projector 原样生成 `<a href>`/`<Link>`。
- `javascript:`、`data:` 或 scheme-relative URL 不应进入运营页面的可点击入口。

**修正**

1. 新增小而明确的链接策略：允许 alert-system 内部单斜杠相对路径，以及绝对 `http://`/`https://`；拒绝 `javascript:`、`data:`、`vbscript:`、控制字符和 `//host` scheme-relative 形式。
2. Service create/update 对非法 dashboard/runbook 返回稳定 422 reason code；空值继续合法。
3. projector 对历史遗留非法值 fail closed：不输出该链接，不能仅依赖新写入校验。
4. 前端 EvidenceRef deep link 同样通过共享的安全 href 展示 helper；不安全 locator 显示摘要与不可打开状态，不生成可点击 URL。
5. 覆盖大小写/前后空白/编码边界与合法内部、HTTP(S) URL。

### C6：live 目录团队筛选只看已加载页

**证据**

- `teamOptions` 从 `loadedItems` 构造；尚未出现在已加载分页中的团队无法通过 UI 选择，虽然手工 URL 可以查询。

**修正**

1. live 模式复用现有 member-readable `listTeamsLite` 查询构造完整团队选项；fixture 模式继续从 fixture rows 构造，避免引入 live 依赖。
2. teams 查询失败只降级团队下拉，不阻断目录 Page View；当前 URL 中已选 team 必须仍能显示稳定 fallback label。
3. 不把团队目录并入 service-observability 后端聚合，也不改变目录 summary 分母。

### C7：请求路径/依赖面板的可读密度低于画稿

**证据**

- 1536px 当前实现语义完整：数据与证据依赖、主请求路径、运行资源依赖、8 节点/7 关系、RED/USE 与右侧上下文均存在。
- 但中心画布的关键名称/关系/信号使用 7–9px 字号，节点与关系被压缩；右栏首屏被内部滚动截断。相较源画稿，用户需要放大或滚动才能完成同一次扫描。
- sparklines、额外节点数字与第四条跨窗口事件缺少合同事实，仍禁止补画。

**修正**

1. 1536px 保持三列，但提高主路径节点、关系标签和 RED 值的可读尺寸；允许中心 band 自身横向浏览，不能靠把关键文字缩到 7px 塞入。
2. 视觉优先级固定为：当前关系与主请求路径 > 运行资源依赖 > declared 数据/证据依赖；不得减少现有 8/7 焦点信息或删除全量 10/10 入口。
3. 右侧 summary 首屏优先展示当前选择、定位结果、最多两条 supporting facts、仍需验证与主动作；来源明细允许在 tab 内继续滚动，但关键下一步不能被无提示地挤出。
4. 1180/900 继续使用 Context Drawer；390px 页面无横向溢出，`.topology-graph` 保持内部横向浏览且关系列表入口始终可见。
5. 对 Directory、Overview、Complete、Partial、Forbidden 五张画稿重新做 1536×1024 同输入比较；Empty/Stale 走合同场景验收。

## 4. 明确不在本轮仓促修改的已知限制

### D1：96-byte cursor 文本前缀

当前 cursor 为满足 512 字符上限，对 name/owner 使用 96-byte 前缀。对于超长且共享前缀的极端数据，分页可能重复/跳过；acceptance 已如实记录。

本轮不直接把前缀改长、加 hash 或改成 server-side cursor：这些方案会分别破坏 512 上限、可见排序语义或无状态边界。Fable 与 Grok 应评估风险等级；若要关闭，必须先明确以下三者中的产品取舍再另开设计修订：

- 对 Page View identity 字段施加并迁移可证明的写入长度上限；
- 放宽 512 cursor 上限并更新契约；
- 引入有生命周期的服务端 snapshot/cursor store。

### D2：生产 E2B Profile 与真实遥测

真实 metric names、labels、retention、阈值与 OpenObserve Evidence 仍属于 Phase 3。fixture Profile 不得被复制成生产 Profile。

## 5. 实施切片

### Slice R1：查询真实性与 fixture 交互

**主要文件**

- `web/src/api/client.ts`
- `web/src/serviceObservability/data.ts`
- `web/src/serviceObservability/fixtures.ts`
- `web/src/pages/ServiceDirectory.tsx`
- `web/src/pages/ServiceOverview.tsx`
- `web/src/pages/ServiceObservabilityWorkspace.tsx`
- `web/scripts/verify-service-observability.mjs`

**出口**

- C1、C2、C6 自动化通过；fixture 五态无退化；非法 URL 显式 422。

### Slice R2：后端与链接 fail-closed

**主要文件**

- `internal/serviceobs/facade/cursor.go`
- `internal/serviceobs/facade/*_test.go`
- `internal/api/service_observability.go`
- `internal/api/service_observability_test.go`
- `internal/api/service_catalog.go`
- `internal/api/service_catalog_test.go`
- `internal/serviceobs/view/projector.go`
- `internal/serviceobs/view/*_test.go`
- `web/src/components/serviceObservability/WorkspaceContext.tsx`
- 新增的前端安全链接 helper/test（位置由实现选择，但不得重复策略）

**出口**

- C3、C4、C5 自动化通过；旧非法登记链接不会进入 Page View。

### Slice R3：视觉可读性与跨层复验

**主要文件**

- `web/src/serviceObservability/serviceObservability.css`
- 必要时小改 `TopologyExplorer.tsx` / `WorkspaceContext.tsx` 的语义容器，不改 Page View 合同
- `design-qa.md` 只追加本轮证据，不改写 Phase 1 历史记录
- `docs/plans/2026-08-23-service-observability-phase2-acceptance.md` 追加 correction SHA、命令与已知限制

**出口**

- C7 的五张 1536 比较与 1180/900/390 响应式证据完成；console 0 warning/0 error；所有后端/前端门禁全绿。

三个 correction slice 串行执行。Fable review 与 R1–R3 的所有 Opus follow-up 均复用本计划顶部记录的固定 Cursor Cloud 会话。每个 slice 完成后 Cursor Cloud 必须提交并 push 当前远端分支，Codex 在本地 fast-forward、验收、记录 exact SHA 后才派下一 slice。

## 6. 测试与验收

### 自动化

```bash
python3 docs/contracts/service-observability/v1/verify_examples.py
go test -race -count=1 ./internal/serviceobs/... ./internal/api ./internal/app ./internal/store ./internal/config ./cmd/server
go vet ./...
go vet -tags=integration ./internal/store
go test -tags=integration ./... -race -count=1
go test -count=1 ./...
cd web
npm run lint
npm run test:service-observability
npm run build
```

### 浏览器

- fixture：Directory、Overview、Complete、Partial、Forbidden、Empty、Stale；focus main ↔ Empty；前进/后退；局部重试；table/canvas；stage/node/relation/evidence/timeline；1536/1180/900/390。
- live：Unconfigured、Partial/Unknown、404、409、422、503；非法 sort/direction/window/timezone 不得显示成功默认页；网络失败不得出现 fixture 数字。
- 安全：不安全 registration/evidence href 不可点击；production bundle 不含 fixture ID、scenario key 或 fixture 数据。

### Git 守护

```bash
git diff --check
git diff --name-only 6e4eddddd8e13ee505639ddadd6619d7afa1ecee...HEAD
git diff --name-only 6e4eddddd8e13ee505639ddadd6619d7afa1ecee...HEAD -- \
  deploy/observability \
  docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md \
  docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md
```

最后一条必须零输出。不得用已有测试全绿替代浏览器实测，也不得用 fixture 截图声称 live API 已验收。

## 7. 退出条件

- Fable 5 High 对本计划给出 `APPROVE`，或所有 blocking comment 已由 Codex 修订并再次确认。
- Opus 5 High 完成 R1–R3，所有 slice 均有 exact SHA、push 与本地门禁证据。
- Grok Bot `alert-system` 对最终 exact SHA review；真实问题由 Grok 提窄 PR，Codex/Opus 复验后合入。
- Kubernetes/OTel hold paths 零改动；Agent 仍未实现；Phase 3 未提前开始。
