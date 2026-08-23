import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import os from 'node:os'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const scriptDir = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDir, '../..')
const webRoot = path.resolve(scriptDir, '..')
const examplesDir = path.join(repoRoot, 'docs/contracts/service-observability/v1/examples')
const require = createRequire(import.meta.url)

const readJSON = async (filename) => JSON.parse(await readFile(path.join(examplesDir, filename), 'utf8'))

function pointerSegments(pointer) {
  assert.ok(pointer.startsWith('/'), `invalid pointer: ${pointer}`)
  return pointer.slice(1).split('/').map((part) => part.replaceAll('~1', '/').replaceAll('~0', '~'))
}

function parentAtPath(document, pointer) {
  const segments = pointerSegments(pointer)
  const key = segments.pop()
  let cursor = document
  for (const segment of segments) cursor = Array.isArray(cursor) ? cursor[Number(segment)] : cursor[segment]
  return { parent: cursor, key }
}

function applyPatch(source, operations) {
  const document = structuredClone(source)
  for (const operation of operations) {
    const { parent, key } = parentAtPath(document, operation.path)
    if (Array.isArray(parent)) {
      const index = Number(key)
      if (operation.op === 'remove') parent.splice(index, 1)
      else if (operation.op === 'add') parent.splice(index, 0, structuredClone(operation.value))
      else parent[index] = structuredClone(operation.value)
    } else if (operation.op === 'remove') {
      delete parent[key]
    } else {
      parent[key] = structuredClone(operation.value)
    }
  }
  return document
}

// stripComments 去掉行注释与块注释，让"代码里不得出现查询模板/凭据"这类断言不会被
// 解释性文档误伤。
function stripComments(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
}

function valueAtPath(document, pointer) {
  return pointerSegments(pointer).reduce((value, segment) => Array.isArray(value) ? value[Number(segment)] : value[segment], document)
}

const [directory, overview, workspace, scenarios] = await Promise.all([
  readJSON('e2b.directory-view.json'),
  readJSON('e2b.overview-view.json'),
  readJSON('e2b.workspace-view.json'),
  readJSON('e2b.workspace-scenarios.json'),
])

assert.equal(directory.view_kind, 'service_directory')
assert.equal(overview.view_kind, 'service_overview')
assert.equal(workspace.view_kind, 'observability_workspace')
assert.equal(directory.items[0].service.service_id, overview.service.service_id)
assert.equal(overview.service.service_id, workspace.service.service_id)
assert.equal(directory.items.length, 7, 'Phase 1 一等 E2B demo 应包含主服务与 6 条关联 registration rows')
assert.equal(directory.items.filter((item) => item.profile?.status === 'published').length, 1, 'E2B 必须仍是唯一已发布 Service Profile')
assert.deepEqual(new Set(overview.dependencies.map((dependency) => dependency.group_key)), new Set(['request_entry', 'orchestration_runtime', 'data_evidence']))
assert.ok(workspace.topology.nodes.length >= 9, 'workspace 必须投影 E2B canonical journey 与 declared dependency nodes')
assert.equal(workspace.golden_signals.find((signal) => signal.semantic === 'traffic').health, 'neutral')
assert.equal(workspace.golden_signals.find((signal) => signal.semantic === 'saturation').measurement_role, 'proxy')
assert.equal(workspace.topology.nodes.flatMap((node) => node.signals).find((signal) => signal.semantic === 'saturation').health, 'unknown')

const expectedGoldenDeltas = new Map([
  ['latency', 37],
  ['traffic', 12],
  ['errors', 6.1],
  ['saturation', 15],
])
for (const [viewName, signals] of [
  ['directory', directory.items[0].golden_signals],
  ['overview', overview.golden_signals],
  ['workspace', workspace.golden_signals],
]) {
  assert.equal(signals.length, expectedGoldenDeltas.size, `${viewName} must expose all four Golden Signals`)
  for (const signal of signals) {
    assert.equal(
      signal.baseline_delta_percent,
      expectedGoldenDeltas.get(signal.semantic),
      `${viewName} ${signal.semantic} baseline delta must remain fixture-driven`,
    )
  }
}

const renderedScenarios = new Map()
for (const scenario of scenarios.scenarios) {
  const rendered = applyPatch(workspace, scenario.patch)
  renderedScenarios.set(scenario.key, rendered)
  for (const expectation of scenario.assertions) {
    const actual = valueAtPath(rendered, expectation.path)
    if ('equals' in expectation) assert.deepEqual(actual, expectation.equals, `${scenario.key} ${expectation.path}`)
    if ('length' in expectation) assert.equal(actual.length, expectation.length, `${scenario.key} ${expectation.path}`)
  }
}

const partial = renderedScenarios.get('telemetry_bundle_timeout_partial_unknown')
assert.ok(partial.golden_signals.every((signal) => signal.value === null && signal.health === 'unknown'))
assert.deepEqual(partial.journey.map((stage) => stage.state), ['ok', 'unknown', 'pending', 'pending', 'pending'])
assert.deepEqual(partial.timeline.map((event) => event.kind), ['telemetry_gap'], 'Partial timeline 不得泄漏当前不可验证的动态值')
assert.equal(partial.topology.relations[0].provenance, 'declared')

const forbidden = renderedScenarios.get('logs_forbidden_traces_allowed')
assert.equal(forbidden.capabilities.view_logs, false)
assert.equal(forbidden.capabilities.view_traces, true)
assert.ok(forbidden.evidence_refs.every((evidence) => evidence.kind !== 'log'))
assert.equal(forbidden.health.status, workspace.health.status, '局部 Forbidden 不得改写服务健康')
assert.ok(!forbidden.data_status.sources.some((source) => source.state === 'unavailable' && source.retryable), 'Forbidden-only 场景不得提供失败数据重试')

const empty = renderedScenarios.get('focus_empty_valid_query')
assert.equal(empty.data_status.state, 'complete')
assert.ok(empty.journey.every((stage) => stage.state === 'unknown'))
assert.equal(empty.evidence_refs.length, 0)
assert.equal(empty.topology.relations[0].provenance, 'declared')

const stale = renderedScenarios.get('observed_relation_stale')
assert.equal(stale.data_status.state, 'stale')
assert.equal(stale.topology.relations[0].freshness, 'stale')
assert.equal(stale.health.status, 'critical', 'stale observed path 与当前 metrics health 正交')

const appSource = await readFile(path.join(repoRoot, 'web/src/App.tsx'), 'utf8')
for (const route of ['services/:serviceId', 'services/:serviceId/observability']) {
  assert.ok(appSource.includes(`path="${route}"`), `missing React route ${route}`)
}
for (const token of ['lazy(', '<Suspense', 'ServiceRouteFallback', 'aria-busy="true"']) {
  assert.ok(appSource.includes(token), `service route loading boundary is missing ${token}`)
}
// live 模式下 App shell 不得 import fixture 去猜服务 identity：真实服务名只能来自
// Page View（fixture 模式走同一条发布路径）。
assert.ok(
  !appSource.includes("serviceObservability/fixtures"),
  'App shell must not import contract fixtures to guess service identity',
)
assert.ok(
  appSource.includes('useServiceLabel'),
  'App shell must read the service label published by the page view',
)

const fixtureSource = await readFile(path.join(repoRoot, 'web/src/serviceObservability/fixtures.ts'), 'utf8')
for (const filename of ['e2b.directory-view.json?raw', 'e2b.overview-view.json?raw', 'e2b.workspace-view.json?raw', 'e2b.workspace-scenarios.json?raw']) {
  assert.ok(fixtureSource.includes(filename), `UI is not driven by ${filename}`)
}

// ---- 数据源选择点：唯一、双重门控、失败不回落 ----

const dataSource = await readFile(path.join(repoRoot, 'web/src/serviceObservability/data.ts'), 'utf8')
assert.ok(
  dataSource.includes('import.meta.env.DEV')
    && dataSource.includes("import.meta.env.VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE === 'true'"),
  'fixture mode must stay double-gated on import.meta.env.DEV and the explicit env flag',
)
assert.ok(
  /await fixtureModule\(\)/.test(dataSource) && /import\('\.\/fixtures'\)/.test(dataSource),
  'fixtures must be reachable only through the dynamic fixture-mode branch',
)
for (const api of ['getServiceDirectory', 'getServiceOverview', 'getServiceWorkspace']) {
  assert.ok(dataSource.includes(api), `data source selector must call ${api}`)
}
// API 失败绝不回落 fixture：选择点里不允许出现任何 catch。
assert.ok(!/catch\s*[({]/.test(dataSource), 'the data source selector must not swallow API errors')

const pageSources = Object.fromEntries(await Promise.all([
  'web/src/pages/ServiceDirectory.tsx',
  'web/src/pages/ServiceOverview.tsx',
  'web/src/pages/ServiceObservabilityWorkspace.tsx',
].map(async (filename) => [filename, await readFile(path.join(repoRoot, filename), 'utf8')])))

// 三个页面都只经过唯一选择点，不自己 import fixture、也不自己判断环境变量。
for (const [filename, source] of Object.entries(pageSources)) {
  assert.ok(
    !source.includes("serviceObservability/fixtures"),
    `${filename} must not import contract fixtures directly`,
  )
  assert.ok(
    !source.includes('import.meta.env.VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE'),
    `${filename} must not re-implement the fixture-mode decision`,
  )
  assert.ok(
    source.includes("from '../serviceObservability/data'"),
    `${filename} must consume the single data source selector`,
  )
}

// 每个页面都消费对应的聚合 API hook，并有显式的失败状态与重试。
assert.ok(pageSources['web/src/pages/ServiceDirectory.tsx'].includes('useServiceDirectoryPages'))
assert.ok(pageSources['web/src/pages/ServiceOverview.tsx'].includes('useServiceOverview'))
assert.ok(pageSources['web/src/pages/ServiceObservabilityWorkspace.tsx'].includes('useServiceWorkspace'))
// 目录必须显式消费 next_cursor（服务端分页），不自己构造游标。
const directoryPage = pageSources['web/src/pages/ServiceDirectory.tsx']
for (const token of ['hasNextPage', 'fetchNextPage', 'isFetchingNextPage']) {
  assert.ok(directoryPage.includes(token), `service directory must consume server pagination via ${token}`)
}
// 工作区的"重试失败数据"在 live 模式必须是 refetch，而不是改 scenario。
const workspacePage = pageSources['web/src/pages/ServiceObservabilityWorkspace.tsx']
assert.ok(workspacePage.includes('onRetry'), 'workspace retry must call the query refetch')
assert.ok(
  /if \(serviceObservabilityFixtureMode\) \{[\s\S]{0,320}?scenario: null/.test(workspacePage),
  'scenario may only be reset inside the fixture-mode retry branch',
)
for (const status of ['409', '404', '422']) {
  assert.ok(workspacePage.includes(status), `workspace must render an explicit ${status} state`)
}
assert.ok(
  workspacePage.includes('profile_unconfigured') || workspacePage.includes('Service Profile'),
  'workspace must explain the 409 unconfigured state instead of showing an empty page',
)

// 客户端只声明三条 typed API，且不在浏览器里出现查询模板或凭据。
const clientSource = await readFile(path.join(repoRoot, 'web/src/api/client.ts'), 'utf8')
for (const token of [
  '/service-observability',
  'ServiceDirectoryQuery',
  'ServiceOverviewQuery',
  'ServiceWorkspaceQuery',
  'reasonCode',
]) {
  assert.ok(clientSource.includes(token), `api client is missing ${token}`)
}
// 只检查 Service Observability 这一段的真实代码：注释里的"不拼 PromQL/SQL"是正确的
// 文档，而查询模板、凭据 header 或数据源语法出现在代码里才是缺陷。范围限定也避免误伤
// client.ts 里与本 Phase 无关的既有类型（例如 webhook 的 bearer_token_env 字段名）。
const observabilityClientSection = clientSource.slice(
  clientSource.indexOf('Service Observability 三层 Page View'),
)
assert.ok(observabilityClientSection.length > 0, 'api client must keep the page view section marker')
const observabilityClientCode = stripComments(observabilityClientSection).toLowerCase()
for (const forbidden of [
  'histogram_quantile', 'rate(', 'select ', 'promql', 'logql', 'authorization', 'api_key', 'token',
]) {
  assert.ok(
    !observabilityClientCode.includes(forbidden),
    `service observability api client must not carry ${forbidden}`,
  )
}

const workspaceSources = await Promise.all([
  'web/src/pages/ServiceObservabilityWorkspace.tsx',
  'web/src/components/serviceObservability/JourneyRail.tsx',
  'web/src/components/serviceObservability/TopologyExplorer.tsx',
  'web/src/components/serviceObservability/WorkspaceContext.tsx',
  'web/src/components/serviceObservability/DomainTimeline.tsx',
  'web/src/serviceObservability/workspaceModel.ts',
  'web/src/serviceObservability/workspaceState.ts',
].map((filename) => readFile(path.join(repoRoot, filename), 'utf8')))
const workspaceSource = workspaceSources.join('\n')
assert.ok(!workspaceSource.includes('AI 诊断'))
assert.ok(!workspaceSource.includes('Agent 暂缓'))
assert.ok(workspaceSource.includes("source.state === 'unavailable' && source.retryable"), '全局失败 banner 必须由可重试 unavailable 来源驱动')
assert.ok(workspaceSource.includes("signals.some((signal) => signal.availability === 'available')"), '资源组只应在所有 USE 信号都缺失时整体 Unknown')

const directorySource = await readFile(path.join(repoRoot, 'web/src/pages/ServiceDirectory.tsx'), 'utf8')
for (const token of ['aria-sort', "searchParams.get('sort')", "searchParams.get('direction')", 'onKeyDown']) {
  assert.ok(directorySource.includes(token), `service directory interaction is missing ${token}`)
}

const overviewSource = await readFile(path.join(repoRoot, 'web/src/pages/ServiceOverview.tsx'), 'utf8')
// windowSeconds 取代了 Phase 1 的 workspaceWindowSeconds：总览不再从 workspace fixture
// 借时间窗，而是用 URL 参数或服务端兜底窗口渲染诊断启动器。
for (const token of ['aria-invalid', 'service-focus-validation', 'windowSeconds']) {
  assert.ok(overviewSource.includes(token), `service overview diagnosis launcher is missing ${token}`)
}

for (const forbiddenLiteral of [
  'openobserve_e2b_logs',
  'prometheus_e2b_uat',
  'FLT-5F437252',
  'rel_api_orchestrator',
  '02:00:00',
  'Sandbox 创建阶段',
  'Sandbox ID',
]) {
  assert.ok(!workspaceSource.includes(forbiddenLiteral), `generic workspace renderer contains E2B-only literal: ${forbiddenLiteral}`)
}

for (const queryKey of ['stage', 'selected', 'tab', 'topology', 'scope', 'cursor']) {
  assert.ok(workspaceSource.includes(`'${queryKey}'`), `workspace URL state is missing ${queryKey}`)
}

for (const topologyInteraction of ['projectTopology(view, scope', '仅显示焦点路径', '查看全部依赖', '主请求与运行路径', '数据与证据依赖', '运行资源依赖']) {
  assert.ok(workspaceSource.includes(topologyInteraction), `workspace topology interaction is missing ${topologyInteraction}`)
}

assert.ok(workspaceSource.includes('<Drawer'), 'workspace responsive context must use an overlay Drawer')


// ============================================================
// C1/C2/C6：查询真实性、fixture query-aware 与团队筛选
// ============================================================

// ---- C1：live 请求必须原样携带 URL 值 ----

// 页面不得在送往服务端之前把 sort/direction/window_seconds 归一化或吞掉：URL 是筛选与
// 窗口的可分享事实源，非法值必须换回服务端的稳定 422，而不是一个成功的默认页面。
assert.ok(
  directoryPage.includes("sort: searchParams.get('sort') ?? undefined")
    && directoryPage.includes("direction: searchParams.get('direction') ?? undefined"),
  'service directory must forward the raw URL sort/direction to the live API',
)
assert.ok(
  !/sort: activeSort\b/.test(directoryPage) && !/direction: activeSort \?/.test(directoryPage),
  'service directory must not normalise sort/direction before the request',
)
assert.ok(
  overviewSource.includes("const rawWindowSeconds = searchParams.get('window_seconds')")
    && overviewSource.includes('windowSeconds: rawWindowSeconds'),
  'service overview must forward the raw window_seconds to the live API',
)
assert.ok(
  !/windowSeconds: windowSeconds\b/.test(overviewSource),
  'service overview must not send a normalised window to the live API',
)
// 客户端类型必须足够宽，否则调用方被迫先吞掉非法值。
assert.ok(
  /direction\?: string/.test(clientSource) && /windowSeconds\?: number \| string/.test(clientSource),
  'api client query types must accept raw URL values so the server can answer 422',
)

// ---- allowlist 单一来源 ----

const directoryModuleSource = await readFile(
  path.join(repoRoot, 'web/src/serviceObservability/directory.ts'), 'utf8',
)
for (const constant of [
  'directorySortKeys', 'sortDirections', 'directoryHealthFilters',
  'directoryDataStatusFilters', 'serviceTierFilters',
]) {
  assert.ok(
    directoryModuleSource.includes(`export const ${constant} =`),
    `directory module must export the single-source allowlist ${constant}`,
  )
}
// 页面与 fixture provider 都必须从常量派生，不得再各写一份字面量数组。
for (const [name, source] of [
  ['service directory page', directoryPage],
  ['query contract', await readFile(path.join(repoRoot, 'web/src/serviceObservability/queryContract.ts'), 'utf8')],
]) {
  for (const duplicated of [
    "['critical', 'degraded', 'unknown', 'healthy']",
    "['complete', 'partial', 'stale', 'unconfigured']",
    "['tier-0', 'tier-1', 'tier-2', 'tier-3']",
    "['service', 'owner', 'tier', 'health', 'fault', 'data', 'observed']",
  ]) {
    assert.ok(
      !source.includes(duplicated),
      `${name} must derive allowlists from the shared constants, found duplicated ${duplicated}`,
    )
  }
}
// fixture provider 不得重抄一份校验实现。
const fixtureProviderSource = await readFile(
  path.join(repoRoot, 'web/src/serviceObservability/fixtures.ts'), 'utf8',
)
assert.ok(
  fixtureProviderSource.includes("from './queryContract'"),
  'fixture provider must reuse the shared query contract instead of copying validation',
)

// ---- 跨语言漂移守护：TS allowlist 必须与 Go 后端白名单一致 ----

const goDirectorySource = await readFile(
  path.join(repoRoot, 'internal/serviceobs/view/directory.go'), 'utf8',
)
const goSortKeys = [...goDirectorySource.matchAll(/SortKey\w+\s+DirectorySortKey = "([a-z]+)"/g)].map((m) => m[1])
const goAPISource = await readFile(path.join(repoRoot, 'internal/api/service_observability.go'), 'utf8')

// ---- 可执行回归：直接运行真实的纯校验模块（不是它的副本） ----

const tmpDir = mkdtempSync(path.join(os.tmpdir(), 'serviceobs-verify-'))
let queryContract
let directoryModule
try {
  execFileSync('npx', [
    'tsc', '--ignoreConfig', 'src/serviceObservability/queryContract.ts',
    '--outDir', tmpDir, '--module', 'commonjs', '--target', 'es2022', '--skipLibCheck',
  ], { cwd: webRoot, stdio: 'pipe' })
  queryContract = require(path.join(tmpDir, 'queryContract.js'))
  directoryModule = require(path.join(tmpDir, 'directory.js'))
} finally {
  // 模块已被 require 进内存，临时产物可以立刻清掉。
  process.on('exit', () => rmSync(tmpDir, { recursive: true, force: true }))
}

const tsSortKeys = [...directoryModule.directorySortKeys]
assert.deepEqual(
  tsSortKeys, goSortKeys,
  `TS sort allowlist ${tsSortKeys} must match the Go allowlist ${goSortKeys}`,
)
for (const value of [...directoryModule.directoryHealthFilters, ...directoryModule.directoryDataStatusFilters]) {
  assert.ok(
    goAPISource.includes(value),
    `Go API allowlist is missing the enum value ${value} used by the frontend`,
  )
}

function expectReason(run, reasonCode, label) {
  let thrown
  try {
    run()
  } catch (error) {
    thrown = error
  }
  assert.ok(thrown, `${label} must be rejected`)
  assert.equal(thrown.reasonCode, reasonCode, `${label} reason_code`)
}

// 目录：每个合法排序字段都必须被接受；非法 sort/direction/enum/limit/cursor 全部 422。
for (const sort of tsSortKeys) {
  queryContract.validateDirectoryQuery({ sort, direction: 'desc' })
}
expectReason(() => queryContract.validateDirectoryQuery({ sort: 'cpu' }), 'invalid_filter', 'unknown sort')
expectReason(() => queryContract.validateDirectoryQuery({ direction: 'sideways' }), 'invalid_filter', 'unknown direction')
expectReason(() => queryContract.validateDirectoryQuery({ health: ['exploded'] }), 'invalid_filter', 'unknown health')
expectReason(() => queryContract.validateDirectoryQuery({ dataStatus: ['fresh'] }), 'invalid_filter', 'unknown data_status')
expectReason(() => queryContract.validateDirectoryQuery({ tier: 'tier-9' }), 'invalid_filter', 'unknown tier')
expectReason(() => queryContract.validateDirectoryQuery({ limit: '0' }), 'invalid_limit', 'zero limit')
expectReason(() => queryContract.validateDirectoryQuery({ limit: '201' }), 'invalid_limit', 'oversized limit')
expectReason(() => queryContract.validateDirectoryQuery({ limit: 'abc' }), 'invalid_limit', 'non-numeric limit')
expectReason(
  () => queryContract.validateDirectoryQuery({ cursor: 'a'.repeat(513) }),
  'invalid_cursor', 'oversized cursor',
)
queryContract.validateDirectoryQuery({})
queryContract.validateDirectoryQuery({ health: ['critical'], dataStatus: ['partial'], limit: 50 })

// 总览：非法 window_seconds 必须 422，而不是缺省窗口。
for (const windowSeconds of ['0', '-5', 'abc', '1.5']) {
  expectReason(
    () => queryContract.validateOverviewQuery({ windowSeconds }),
    'invalid_window', `window_seconds=${windowSeconds}`,
  )
}
queryContract.validateOverviewQuery({})
queryContract.validateOverviewQuery({ windowSeconds: '300' })

// 工作区：focus 配对、pattern 与时间窗配对。focus_kinds 来自契约 fixture，不是硬编码。
const focusKinds = overview.focus_kinds.map(({ key, value_pattern }) => ({ key, value_pattern }))
const primaryFocus = overview.focus_kinds.find((focus) => focus.primary)
expectReason(
  () => queryContract.validateWorkspaceQuery(focusKinds, { focusKind: primaryFocus.key }),
  'invalid_focus', 'focus_kind without value',
)
expectReason(
  () => queryContract.validateWorkspaceQuery(focusKinds, { focusValue: workspace.focus.value }),
  'invalid_focus', 'focus_value without kind',
)
expectReason(
  () => queryContract.validateWorkspaceQuery(focusKinds, { focusKind: 'not_declared', focusValue: 'x' }),
  'invalid_focus', 'undeclared focus kind',
)
expectReason(
  () => queryContract.validateWorkspaceQuery(focusKinds, { focusKind: primaryFocus.key, focusValue: 'not-a-sandbox' }),
  'invalid_focus', 'focus value violating the profile pattern',
)
expectReason(
  () => queryContract.validateWorkspaceQuery(focusKinds, { from: workspace.scope.from }),
  'invalid_time_range', 'from without to',
)
expectReason(
  () => queryContract.validateWorkspaceQuery(focusKinds, { from: 'yesterday', to: workspace.scope.to }),
  'invalid_time_range', 'non absolute from',
)
queryContract.validateWorkspaceQuery(focusKinds, {
  focusKind: workspace.focus.kind,
  focusValue: workspace.focus.value,
  from: workspace.scope.from,
  to: workspace.scope.to,
  timezone: workspace.scope.timezone,
})

// ---- C2：fixture 的 focus 身份对齐 ----

// Empty 场景里表示"这一次聚焦的是哪个请求"的身份值必须**唯一**，这样通用的身份对齐
// 不需要任何服务专属分支就能让 URL/输入框/Journey 完全一致。
const emptyScenarioView = renderedScenarios.get('focus_empty_valid_query')
const emptyFocusValue = emptyScenarioView.focus.value
assert.notEqual(emptyFocusValue, workspace.focus.value, 'Empty scenario must use a distinct focus identity')
const emptyOccurrences = JSON.stringify(emptyScenarioView).split(`"${emptyFocusValue}"`).length - 1
assert.equal(
  emptyOccurrences, 1,
  'Empty scenario must carry its focus identity exactly once so generic alignment stays lossless',
)

const requestedFocus = 'sbx_r1regression'
const aligned = queryContract.alignFocusIdentity(emptyScenarioView, emptyFocusValue, requestedFocus)
assert.equal(aligned.focus.value, requestedFocus, 'aligned view must carry the requested focus identity')
assert.ok(
  !JSON.stringify(aligned).includes(emptyFocusValue),
  'aligned view must not keep the scenario focus identity anywhere',
)
// 对齐只改身份，不改任何观测状态：Empty 仍是 Empty，不得变成 Healthy。
assert.equal(aligned.data_status.state, emptyScenarioView.data_status.state)
assert.deepEqual(aligned.journey.map((stage) => stage.state), emptyScenarioView.journey.map((stage) => stage.state))
assert.deepEqual(aligned.evidence_refs, emptyScenarioView.evidence_refs)
assert.equal(aligned.health.status, emptyScenarioView.health.status)
// 身份相同时必须原样返回，不做无谓的深拷贝改写。
assert.equal(
  queryContract.alignFocusIdentity(emptyScenarioView, emptyFocusValue, emptyFocusValue),
  emptyScenarioView,
)

// fixture provider 必须是 query-aware，并且在 focus 不同时给出 bounded Empty。
assert.ok(
  /export function getWorkspaceFixture\([\s\S]{0,400}?query: ServiceWorkspaceQuery/.test(fixtureProviderSource),
  'fixture workspace provider must accept the workspace query',
)
assert.ok(
  fixtureProviderSource.includes('alignFocusIdentity(emptyView'),
  'fixture workspace provider must align the Empty view focus identity with the request',
)
for (const forbiddenLiteral of ['sbx_hp16c3', 'sbx_notfound1', 'svc_e2b_fixture']) {
  const providerCode = stripComments(fixtureProviderSource)
  assert.ok(
    !providerCode.includes(forbiddenLiteral),
    `fixture provider must not branch on the service-specific literal ${forbiddenLiteral}`,
  )
}
// 重新执行 focus 必须离开旧 QA 场景，否则 URL 与视图会再次矛盾。
// 只看 runDiagnosis 自己的函数体：宽松的"附近出现过 scenario: null"会被 retryFailed
// 里同名清理误判成通过。
function functionBody(source, declaration) {
  const start = source.indexOf(declaration)
  assert.notEqual(start, -1, `missing ${declaration}`)
  const end = source.indexOf('\n  }', start)
  assert.notEqual(end, -1, `unterminated ${declaration}`)
  return source.slice(start, end)
}
const runDiagnosisBody = functionBody(workspacePage, 'const runDiagnosis = () => {')
assert.ok(
  runDiagnosisBody.includes('scenario: null'),
  'runDiagnosis must clear the QA scenario so the new focus is actually re-queried',
)
assert.ok(
  runDiagnosisBody.includes('focus_value: value'),
  'runDiagnosis must publish the requested focus value to the URL',
)
// retryFailed 只在 fixture 分支里碰 scenario；live 模式必须是 refetch。
const retryFailedBody = functionBody(workspacePage, 'const retryFailed = () => {')
assert.ok(
  /if \(serviceObservabilityFixtureMode\) \{[\s\S]*?scenario: null/.test(retryFailedBody)
    && retryFailedBody.includes('onRetry()'),
  'retryFailed must refetch in live mode and only reset scenario inside the fixture branch',
)
// 显式场景下 URL 的 focus 以场景视图为准。
assert.ok(
  workspacePage.includes('canonicalWorkspaceSearchParams(searchParams, view, viewState, Boolean(scenario))'),
  'explicit scenarios must canonicalise the URL focus to the scenario view',
)
const workspaceStateSource = await readFile(
  path.join(repoRoot, 'web/src/serviceObservability/workspaceState.ts'), 'utf8',
)
assert.ok(
  workspaceStateSource.includes('preferViewFocus'),
  'workspace state must support canonicalising focus from the scenario view',
)
// fixture live region 绝不能自称服务端数据。
assert.ok(
  workspacePage.includes('已加载 fixture 主视图') && workspacePage.includes('已加载 fixture 场景'),
  'fixture live region must announce fixture data instead of claiming live server data',
)

// ---- R1 复审缺口 2：422 恢复必须清掉全部目录查询键 ----

assert.ok(
  directoryModuleSource.includes('export const directoryQueryKeys ='),
  'directory module must own the single list of directory query keys',
)
const queryKeyList = directoryModuleSource
  .slice(directoryModuleSource.indexOf('export const directoryQueryKeys ='))
  .split(']')[0]
for (const key of ['sort', 'direction', 'limit', 'health', 'data', 'tier', 'team', 'q', 'environment']) {
  assert.ok(
    queryKeyList.includes(`'${key}'`),
    `directoryQueryKeys must include ${key} so a 422 caused by it is recoverable`,
  )
}
const clearFiltersBody = functionBody(directoryPage, 'const clearFilters = () => {')
assert.ok(
  clearFiltersBody.includes('for (const key of directoryQueryKeys)'),
  'clearFilters must clear every directory query key, otherwise an invalid sort/direction/limit loops on the same 422',
)
assert.ok(
  !/for \(const key of \['q', 'team'/.test(directoryPage),
  'clearFilters must not keep its own partial literal key list',
)
// 422 恢复动作必须真的走 clearFilters。
assert.ok(
  /onClearFilters=\{clearFilters\}/.test(directoryPage) && directoryPage.includes('onClearFilters'),
  'the directory 422 state must recover through clearFilters',
)

// ---- R1 复审缺口 3：fixture 口径必须与 Go HTTP 边界一致 ----

const queryContractSource = await readFile(
  path.join(repoRoot, 'web/src/serviceObservability/queryContract.ts'), 'utf8',
)
assert.ok(
  queryContractSource.includes('decimalIntegerPattern'),
  'fixture limit/window parsing must use strict decimal integer syntax like Go strconv.Atoi',
)
assert.ok(
  queryContractSource.includes('rfc3339Pattern'),
  'fixture from/to parsing must require RFC3339 with Z or an offset like Go time.Parse',
)
assert.ok(
  queryContractSource.includes('parseCSVMembers') && queryContractSource.includes('maxCSVValues'),
  'fixture CSV parsing must mirror the Go splitCSV allowlist, dedupe and cap',
)
// Go 的 CSV 上限与页面上界必须同值。
const goCSVCap = /maxCSVValues\s+=\s+(\d+)/.exec(goAPISource)
assert.ok(goCSVCap, 'cannot read the Go CSV cap')
const tsCSVCap = /maxCSVValues:\s*(\d+)/.exec(queryContractSource)
assert.ok(tsCSVCap, 'cannot read the TS CSV cap')
assert.equal(tsCSVCap[1], goCSVCap[1], 'TS and Go CSV caps must match')
// fixture 本地筛选也要按 CSV 匹配，否则合法多值 URL 会筛出空集。
assert.ok(
  directoryPage.includes('csvMatches(searchParams.get(\'health\')')
    && directoryPage.includes('csvMatches(searchParams.get(\'data\')'),
  'fixture-local filtering must treat health/data as CSV member sets',
)

// 可执行断言：整数语法、CSV 与 RFC3339 都跑真实实现。
for (const limit of ['0x10', '1e2', '5.0', 'Infinity']) {
  expectReason(
    () => queryContract.validateDirectoryQuery({ limit }),
    'invalid_limit', `limit=${limit} must be rejected by decimal integer syntax`,
  )
}
queryContract.validateDirectoryQuery({ limit: '+50' })
queryContract.validateDirectoryQuery({ sort: ' health ', direction: ' desc ', tier: ' tier-1 ' })
queryContract.validateDirectoryQuery({ health: ['critical', 'degraded'] })
queryContract.validateDirectoryQuery({ health: ['critical', 'critical'] })
expectReason(
  () => queryContract.validateDirectoryQuery({ health: ['critical', ''] }),
  'invalid_filter', 'empty CSV member',
)
expectReason(
  () => queryContract.validateDirectoryQuery({ health: Array.from({ length: 9 }, () => 'critical') }),
  'invalid_filter', 'too many CSV members',
)
for (const bound of ['2026-08-22', '2026-08-22T02:04:22']) {
  expectReason(
    () => queryContract.validateWorkspaceScope({ from: bound, to: '2026-08-22T02:09:22Z' }),
    'invalid_time_range', `from=${bound} must require an explicit timezone`,
  )
}
queryContract.validateWorkspaceScope({
  from: '2026-08-22T10:04:22+08:00', to: '2026-08-22T10:09:22+08:00',
})
// focus 与非 focus 校验必须可分开调用：显式场景只校验后者。
assert.equal(typeof queryContract.validateWorkspaceFocus, 'function')
assert.equal(typeof queryContract.validateWorkspaceScope, 'function')
// 只看 getWorkspaceFixture 的函数体：`guardWorkspaceFocus(` 的**声明**出现在更靠前的
// 位置，用整文件比较位置会把顺序判反。
const workspaceProviderBody = stripComments(
  fixtureProviderSource.slice(fixtureProviderSource.indexOf('export function getWorkspaceFixture(')),
)
const scenarioAt = workspaceProviderBody.indexOf('resolveWorkspaceScenario(scenarioValue)')
const focusGuardAt = workspaceProviderBody.indexOf('guardWorkspaceFocus(query)')
const baseLookupAt = workspaceProviderBody.indexOf('getBaseWorkspaceFixture(serviceId)')
assert.ok(scenarioAt !== -1 && focusGuardAt !== -1 && baseLookupAt !== -1, 'provider body markers missing')
assert.ok(
  baseLookupAt < scenarioAt && scenarioAt < focusGuardAt,
  'the fixture provider must resolve service and scenario before validating focus',
)
// 显式场景分支仍要校验非 focus 字段。
const scenarioBranch = workspaceProviderBody.slice(scenarioAt, focusGuardAt)
assert.ok(
  scenarioBranch.includes('guardWorkspaceScope(query)'),
  'the explicit scenario branch must still validate the non-focus query fields',
)

// ---- C6：团队筛选来自团队目录，而不是已加载分页 ----

assert.ok(
  directoryPage.includes('listTeamsLite'),
  'live directory team filter must use the member-readable team directory',
)
assert.ok(
  /enabled: !serviceObservabilityFixtureMode/.test(directoryPage),
  'fixture mode must not depend on the live team directory query',
)
assert.ok(
  directoryPage.includes('teamsQuery.isError'),
  'a failing team query must degrade only the dropdown',
)
assert.ok(
  /if \(selectedTeam && !byID\.has\(selectedTeam\)\)/.test(directoryPage),
  'a selected team from the URL must keep a stable fallback label',
)
assert.ok(
  !/teamOptions = Array\.from\(new Map\(loadedItems/.test(directoryPage),
  'team options must no longer be limited to the loaded pages',
)

console.log(`service-observability verification passed: 3 routes, ${scenarios.scenarios.length + 1} workspace states, `
  + `${tsSortKeys.length} shared sort keys, executable query-contract regressions`)
