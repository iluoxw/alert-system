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
const serviceShapeAt = workspaceProviderBody.indexOf('isUsableServiceID(serviceId)')
const scenarioAt = workspaceProviderBody.indexOf('resolveWorkspaceScenario(scenarioValue)')
const focusGuardAt = workspaceProviderBody.indexOf('guardWorkspaceFocus(query)')
const missingServiceAt = workspaceProviderBody.indexOf('if (!base) return')
assert.ok(
  serviceShapeAt !== -1 && scenarioAt !== -1 && focusGuardAt !== -1 && missingServiceAt !== -1,
  'provider body markers missing',
)
// live 的 parseServiceID 只看形状且先于一切 query 校验。
assert.ok(serviceShapeAt < focusGuardAt, 'service ID shape must be checked before query validation')
// 显式 QA 场景的例外分支必须在 focus 校验之前，并且以"已知服务"为前提。
assert.ok(scenarioAt < focusGuardAt, 'the explicit scenario branch must precede focus validation')
assert.ok(
  /if \(base && scenario\)/.test(workspaceProviderBody),
  'the scenario exception must require a known service; a missing service cannot bypass validation',
)
// 正常路径必须与 live 同序：先把 query 判完，再判"服务不存在"。
assert.ok(
  focusGuardAt < missingServiceAt,
  'the normal fixture path must validate the query before answering 404, matching the live handler',
)
// 显式场景分支仍要校验非 focus 字段，并且必须应用环境**语义**不匹配：只校验语法会让
// `scenario=partial&environment=staging` 端出另一个环境的事实。
const scenarioBranch = workspaceProviderBody.slice(scenarioAt, focusGuardAt)
assert.ok(
  scenarioBranch.includes('guardWorkspaceScope(query)'),
  'the explicit scenario branch must still validate the non-focus query fields',
)
assert.ok(
  scenarioBranch.includes('environmentMismatch(query.environment, base)'),
  'the explicit scenario branch must reject a mismatched environment with 404, not serve other-environment facts',
)
assert.ok(
  scenarioBranch.indexOf('guardWorkspaceScope(query)')
    < scenarioBranch.indexOf('environmentMismatch(query.environment, base)'),
  'non-focus validation must run before the environment mismatch check',
)
assert.ok(
  scenarioBranch.indexOf('environmentMismatch(query.environment, base)')
    < scenarioBranch.indexOf('applyJsonPatch(base, scenario.patch)'),
  'the environment mismatch check must run before the scenario patch is applied',
)
// environment 必须先于 focus 判定（live handler 是 parseEnvironment -> parseFocus）。
const environmentGuardAt = workspaceProviderBody.indexOf('guardEnvironment(query.environment)')
assert.ok(environmentGuardAt !== -1, 'the normal path must validate environment explicitly')
assert.ok(
  environmentGuardAt < focusGuardAt,
  'environment must be validated before focus so both-invalid returns invalid_filter like Go',
)
const scopeGuardAt = workspaceProviderBody.indexOf('guardWorkspaceScope(query)', focusGuardAt)
assert.ok(
  focusGuardAt < scopeGuardAt,
  'focus must still be validated before the time range, matching parseFocus -> parseTimeRange',
)

// ---- 复审缺口 1：RFC3339 必须是自洽严格解析，不依赖 Date.parse ----

assert.ok(
  queryContractSource.includes('export function isStrictRFC3339'),
  'the query contract must expose a strict RFC3339 validator',
)
assert.ok(
  queryContractSource.includes('daysInMonth') && queryContractSource.includes('isLeapYear'),
  'strict RFC3339 must validate the calendar, not just the shape',
)
assert.ok(
  /\[\.,\]\\d\+/.test(queryContractSource),
  'strict RFC3339 must accept a dot or comma fractional separator followed by digits, like Go',
)
assert.ok(
  !/Date\.parse/.test(stripComments(queryContractSource)),
  'strict RFC3339 must not rely on Date.parse, which both over- and under-accepts vs Go',
)
// 期望表来自在 go1.25 上实测 time.Parse(time.RFC3339, ...)。
for (const [candidate, accepted] of [
  ['2026-08-22T02:04:22Z', true],
  ['2026-08-22t02:04:22z', false],
  ['2026-08-22T02:04:22z', false],
  ['2026-08-22t02:04:22Z', false],
  ['2026-02-30T02:04:22Z', false],
  ['2026-02-29T02:04:22Z', false],
  ['2024-02-29T02:04:22Z', true],
  ['2026-13-01T02:04:22Z', false],
  ['2026-08-32T02:04:22Z', false],
  ['2026-04-31T02:04:22Z', false],
  ['2026-08-22T24:00:00Z', false],
  ['2026-08-22T23:60:00Z', false],
  ['2026-08-22T23:59:60Z', false],
  ['2026-08-22T23:59:59.123Z', true],
  ['2026-08-22T23:59:59.Z', false],
  ['2026-08-22T23:59:59+08:00', true],
  ['2026-08-22T23:59:59+25:00', true],
  ['2026-08-22T23:59:59+08:60', true],
  ['2026-08-22T23:59:59+0800', false],
  ['2026-08-22T23:59:59', false],
  ['2026-08-22', false],
  ['2026-8-22T02:04:22Z', false],
  ['2026-08-22T02:04:22.000000001Z', true],
  // Go 的通用解析路径接受逗号小数秒；分隔符后必须至少一位数字。
  ['2026-08-22T02:04:22,123Z', true],
  ['2026-08-22T02:04:22,1Z', true],
  ['2026-08-22T02:04:22,123+08:00', true],
  ['2026-08-22T02:04:22,Z', false],
  ['2026-08-22T02:04:22;123Z', false],
  ['2026-08-22T02:04:22,,1Z', false],
  ['2026-08-22T02:04:22,12,3Z', false],
]) {
  assert.equal(
    queryContract.isStrictRFC3339(candidate), accepted,
    `strict RFC3339 must ${accepted ? 'accept' : 'reject'} ${candidate} exactly like Go`,
  )
}

// ---- 复审缺口 3：environment 必须进入 TS 查询契约 ----

assert.ok(
  queryContractSource.includes('export function validateEnvironment'),
  'the query contract must validate environment like all three API routes do',
)
// 形状与后端同一条正则。
const goEnvironmentPattern = /environmentPattern = regexp\.MustCompile\(`([^`]+)`\)/.exec(goAPISource)
assert.ok(goEnvironmentPattern, 'cannot read the Go environment pattern')
assert.ok(
  queryContractSource.includes(goEnvironmentPattern[1]),
  `TS environment pattern must match Go ${goEnvironmentPattern[1]}`,
)
queryContract.validateEnvironment('production')
queryContract.validateEnvironment(' production ')
queryContract.validateEnvironment(undefined)
for (const environment of ['-bad', 'prod uction', 'x'.repeat(65), '_prod']) {
  expectReason(
    () => queryContract.validateEnvironment(environment),
    'invalid_filter', `environment=${environment}`,
  )
}
// 三个入口都要校验 environment。
expectReason(() => queryContract.validateDirectoryQuery({ environment: '-bad' }), 'invalid_filter', 'directory environment')
expectReason(() => queryContract.validateOverviewQuery({ environment: '-bad' }), 'invalid_filter', 'overview environment')
expectReason(
  () => queryContract.validateWorkspaceQuery([], { environment: '-bad' }),
  'invalid_filter', 'workspace environment',
)
// fixture 的环境作用域必须从 Page View 数据派生，不能写死服务 ID。
assert.ok(
  fixtureProviderSource.includes('directoryEnvironments()')
    && fixtureProviderSource.includes('emptyDirectoryView('),
  'the directory fixture must scope itself by the environment carried in its own page view',
)
assert.ok(
  fixtureProviderSource.includes('view.scope.environment')
    && fixtureProviderSource.includes('overviewFixture.service.environment'),
  'overview/workspace fixtures must compare the requested environment with their own page view',
)
// 环境归属判定只有一份实现，两条工作区分支共用它。
assert.ok(
  fixtureProviderSource.includes('function environmentMismatch('),
  'the environment ownership check must live in a single shared helper',
)
assert.equal(
  (fixtureProviderSource.match(/environmentMismatch\(query\.environment, base\)/g) ?? []).length, 2,
  'both workspace branches (scenario and normal) must apply the environment ownership check',
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


// ============================================================
// R2：C3 游标 fail-closed / C4 timezone 真值 / C5 链接安全策略
// ============================================================

// ---- C3：游标必须要求 io.EOF（Go 侧断言，防止实现被回退）----

const cursorSource = await readFile(
  path.join(repoRoot, 'internal/serviceobs/facade/cursor.go'), 'utf8',
)
assert.ok(
  /errors\.Is\(err, io\.EOF\)/.test(cursorSource),
  'cursor decoding must require io.EOF so concatenated JSON payloads are rejected',
)
assert.ok(
  cursorSource.includes('DisallowUnknownFields'),
  'cursor decoding must keep rejecting unknown fields',
)

// ---- C4：后端 timezone 必须 LoadLocation，且 tzdata 编进二进制 ----

assert.ok(
  /time\.LoadLocation\(timezone\)/.test(goAPISource),
  'the API timezone boundary must verify the zone actually loads',
)
assert.ok(
  goAPISource.includes('_ "time/tzdata"'),
  'the binary must embed the IANA database instead of depending on image zoneinfo',
)
// 形状校验不得被 LoadLocation 取代。
assert.ok(
  goAPISource.includes('timezonePattern.MatchString(timezone)'),
  'the shape/length guard must stay in front of LoadLocation',
)
// 前端 fixture 契约必须同口径：形状 + 运行时可加载。
assert.ok(
  queryContractSource.includes('export function validateTimezone')
    && queryContractSource.includes('isLoadableTimezone'),
  'the fixture query contract must validate timezone shape and runtime availability',
)
const goTimezonePattern = /timezonePattern = regexp\.MustCompile\(`([^`]+)`\)/.exec(goAPISource)
assert.ok(goTimezonePattern, 'cannot read the Go timezone pattern')
assert.ok(
  queryContractSource.includes(goTimezonePattern[1]),
  `TS timezone pattern must match Go ${goTimezonePattern[1]}`,
)
assert.ok(
  /Go .*time\.LoadLocation|生产权威/.test(queryContractSource),
  'the fixture contract must document that Go LoadLocation stays the production authority',
)
// 可执行：真实时区通过，形状合法但不存在的时区被拒。
for (const zone of ['UTC', 'Asia/Shanghai', 'America/New_York', 'Europe/Berlin']) {
  assert.equal(queryContract.validateTimezone(zone), zone, `${zone} must be accepted`)
}
assert.equal(queryContract.validateTimezone(' UTC '), 'UTC', 'timezone must be trimmed like Go')
assert.equal(queryContract.validateTimezone(undefined), '', 'blank timezone stays blank')
// `+08:00` / `-05:00` 是形状规则的行为判别用例：Intl 接受它们（ES2024 的偏移量时区），
// 但后端形状正则要求以字母开头，Go 的 LoadLocation 也不接受。少了形状校验就会出现
// "fixture 通过、live 422"。
// `Local` 是 Go 的进程级特殊值：LoadLocation 接受它，但它不是确定的 IANA 显示时区，
// 浏览器 Intl 也拒绝它。两侧都必须显式拒绝，否则同一个值 live 接受、fixture 422，
// 而带 Local 的 Page View 在前端无法如实渲染。
for (const zone of [
  'Mars/Olympus', 'Asia/Atlantis', 'Asia Shanghai', 'z'.repeat(65),
  '+08:00', '-05:00', '_bad', '9Bad', 'Local', 'local', 'LOCAL',
]) {
  expectReason(() => queryContract.validateTimezone(zone), 'invalid_timezone', `timezone=${zone}`)
}
// 断言必须落在**用法**上：只检查常量名存在，会被"删掉比较、留下声明"的改动绕过。
assert.ok(
  /=== localTimezoneSentinel\.toLowerCase\(\)/.test(queryContractSource),
  'the fixture timezone contract must compare against the process-local sentinel, not just declare it',
)
assert.ok(
  /strings\.EqualFold\(timezone, localTimezoneSentinel\)/.test(goAPISource),
  'the API timezone boundary must compare against the process-local sentinel, not just declare it',
)
// 过期注释不得再声称"真实时区加载不在本模块"。
assert.ok(
  !/属于 R2 的 C4，不在本模块放宽/.test(queryContractSource),
  'the stale comment about timezone loading being out of scope must be gone',
)
// fixture provider 必须把有效时区反映到返回视图。
assert.ok(
  fixtureProviderSource.includes('applyRequestedTimezone('),
  'the fixture workspace must reflect a valid requested timezone in scope.timezone',
)
assert.equal(
  (fixtureProviderSource.match(/applyRequestedTimezone\(/g) ?? []).length, 4,
  'main, Empty and explicit scenario paths must all reflect the requested timezone',
)

// ---- C4：时间渲染必须显式指定时区 ----

const formatSource = await readFile(
  path.join(repoRoot, 'web/src/serviceObservability/format.ts'), 'utf8',
)
assert.ok(
  /formatClock\(value: string, timezone = 'UTC'\)/.test(formatSource)
    && /formatDateTime\(value: string, timezone = 'UTC'\)/.test(formatSource),
  'default time formatting must be UTC, never a silent regional zone',
)
assert.ok(
  !formatSource.includes("timezone = 'Asia/Shanghai'"),
  'no formatter may default to Asia/Shanghai',
)
// Service Observability 路由内的每个调用点都必须显式传时区。
const timeRenderingSources = await Promise.all([
  'web/src/pages/ServiceDirectory.tsx',
  'web/src/pages/ServiceOverview.tsx',
  'web/src/pages/ServiceObservabilityWorkspace.tsx',
  'web/src/components/ServiceObservability.tsx',
  'web/src/components/serviceObservability/WorkspaceContext.tsx',
  'web/src/components/serviceObservability/JourneyRail.tsx',
  'web/src/components/serviceObservability/DomainTimeline.tsx',
  'web/src/components/serviceObservability/TopologyExplorer.tsx',
].map(async (filename) => [filename, await readFile(path.join(repoRoot, filename), 'utf8')]))
for (const [filename, source] of timeRenderingSources) {
  for (const call of source.matchAll(/format(?:Clock|DateTime)\(([^)]*)\)/g)) {
    assert.ok(
      call[1].includes(','),
      `${filename} must pass an explicit timezone: ${call[0]}`,
    )
  }
}
// Directory 没有 scope，显式 UTC；Workspace 的证据行用 view.scope.timezone。
assert.ok(
  directoryPage.includes("const directoryTimezone = 'UTC'"),
  'the directory must render UTC explicitly because DirectoryView carries no timezone',
)
const workspaceContextSource = timeRenderingSources
  .find(([filename]) => filename.endsWith('WorkspaceContext.tsx'))[1]
assert.ok(
  /function EvidenceRow\(\{ evidence, timezone \}/.test(workspaceContextSource),
  'the workspace evidence row must receive the page view timezone',
)
assert.ok(
  /timezone=\{view\.scope\.timezone\}/.test(workspaceContextSource),
  'evidence rows must be rendered with view.scope.timezone',
)

// ---- C5：链接安全策略只有一份，两侧同口径 ----

const goSafeLinkSource = await readFile(path.join(repoRoot, 'internal/safelink/safelink.go'), 'utf8')
const tsSafeHrefSource = await readFile(
  path.join(repoRoot, 'web/src/serviceObservability/safeHref.ts'), 'utf8',
)
// 写入校验与投影必须复用同一个 Go 策略包，不各写一份。
const serviceCatalogSource = await readFile(
  path.join(repoRoot, 'internal/api/service_catalog.go'), 'utf8',
)
const projectorSource = await readFile(
  path.join(repoRoot, 'internal/serviceobs/view/projector.go'), 'utf8',
)
assert.ok(
  serviceCatalogSource.includes('alert-system/internal/safelink')
    && projectorSource.includes('alert-system/internal/safelink'),
  'service write validation and page view projection must share one link policy package',
)
assert.ok(
  serviceCatalogSource.includes('service_link_invalid'),
  'invalid dashboard/runbook writes must return the stable service_link_invalid reason code',
)
assert.ok(
  /safelink\.SafeOrEmpty\(identity\.DashboardURL\)/.test(projectorSource)
    && /safelink\.SafeOrEmpty\(identity\.RunbookURL\)/.test(projectorSource),
  'the projector must fail closed on historical unsafe dashboard/runbook values',
)
// 两侧策略描述必须一致（同一批被拒 scheme 与形态）。
for (const rejected of ['javascript', 'data:', 'vbscript', 'mailto', 'scheme-relative']) {
  assert.ok(
    goSafeLinkSource.includes(rejected),
    `the Go link policy must document rejecting ${rejected}`,
  )
  assert.ok(
    tsSafeHrefSource.includes(rejected),
    `the TS link policy must document rejecting ${rejected}`,
  )
}
// EvidenceRef deep link 必须过共享 helper，且不安全时不渲染 anchor/Button href。
assert.ok(
  workspaceContextSource.includes("from '../../serviceObservability/safeHref'"),
  'evidence deep links must go through the shared safeHref helper',
)
// 正向断言：locator 必须经过 safeHref 才能变成 href。只断言"没有原始 href={...}"太窄——
// 把 `const href = safeHref(...)` 改成 `const href = evidence.locator.deep_link` 也能绕过。
assert.ok(
  /const href = safeHref\(evidence\.locator\.deep_link\)/.test(workspaceContextSource),
  'the evidence row href must come from safeHref(evidence.locator.deep_link)',
)
assert.ok(
  !/href=\{evidence\.locator\.deep_link\}/.test(workspaceContextSource)
    && !/href=\{traceEvidence\.locator\.deep_link\}/.test(workspaceContextSource),
  'raw evidence locators must never be rendered as an href',
)
assert.ok(
  workspaceContextSource.includes('is-unopenable') && workspaceContextSource.includes('不可打开'),
  'unsafe evidence must still render summary/availability as a non-clickable row',
)
assert.ok(
  /traceEvidence && safeHref\(traceEvidence\.locator\.deep_link\)/.test(workspaceContextSource),
  'the trace action must be omitted when the locator is unsafe',
)

// 可执行：TS 策略与 Go 读**同一张**共享用例表。
//
// 两侧各写一份清单只能证明"各自自洽"；读同一个文件才能机械证明口径一致。Go 侧在
// internal/safelink/safelink_test.go 读同一份 JSON。
const safeHrefModulePath = path.join(tmpDir, 'safeHref.js')
execFileSync('npx', [
  'tsc', '--ignoreConfig', 'src/serviceObservability/safeHref.ts',
  '--outDir', tmpDir, '--module', 'commonjs', '--target', 'es2022', '--skipLibCheck',
], { cwd: webRoot, stdio: 'pipe' })
const safeHrefModule = require(safeHrefModulePath)
const linkPolicyTable = JSON.parse(await readFile(
  path.join(repoRoot, 'internal/safelink/testdata/link_policy_cases.json'), 'utf8',
))
assert.ok(linkPolicyTable.cases.length >= 55, 'the shared link policy table must stay comprehensive')
let hostBearingPositives = 0
let hostlessNegatives = 0
for (const { name, href, safe } of linkPolicyTable.cases) {
  assert.equal(
    safeHrefModule.isSafeHref(href), safe,
    `TS link policy must ${safe ? 'accept' : 'reject'} ${JSON.stringify(href)} (${name})`,
  )
  assert.equal(
    safeHrefModule.safeHref(href), safe ? href.trim() : undefined,
    `safeHref must ${safe ? 'return the trimmed value' : 'return undefined'} for ${JSON.stringify(href)}`,
  )
  if (safe && /^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(href.trim())) hostBearingPositives += 1
  if (!safe && /^https?:\/\//i.test(href.trim())) hostlessNegatives += 1
}
// 表里必须同时有"带真实主机的正例"和"hostless/malformed 的反例"，否则这条断言会退化成
// 只覆盖内部路径。
assert.ok(hostBearingPositives >= 8, `expected host-bearing positives, got ${hostBearingPositives}`)
assert.ok(hostlessNegatives >= 8, `expected hostless/malformed negatives, got ${hostlessNegatives}`)

// 绝对 URL 分支必须真的解析 URL 并要求 hostname 非空，不能只看 scheme 前缀。
assert.ok(
  /new URL\(value\)/.test(tsSafeHrefSource) && /parsed\.hostname !== ''/.test(tsSafeHrefSource),
  'the TS absolute branch must parse the URL and require a non-empty hostname',
)
assert.ok(
  /url\.Parse\(value\)/.test(goSafeLinkSource) && /parsed\.Hostname\(\) == ""/.test(goSafeLinkSource),
  'the Go absolute branch must parse the URL and require a non-empty hostname',
)
// 空 authority 必须显式拒绝：WHATWG 会把 `http:///x` 规范化成外站主机 `x`，而 Go 认为
// 它没有 host。目标取决于解析器的值不能变成可点击链接。
assert.ok(
  tsSafeHrefSource.includes('emptyAuthorityPattern'),
  'the TS policy must explicitly reject an empty authority to stay aligned with Go',
)
// 端口上界：net/url 只要求端口是数字，WHATWG 则对超出 16 位的端口抛 TypeError。Go 侧
// 必须自己补这道上界检查，否则越界端口会在写入/投影侧通过、只在前端 fail closed。
assert.ok(
  /number <= maxPort/.test(goSafeLinkSource) && /maxPort = 65535/.test(goSafeLinkSource),
  'the Go policy must bound a present port to the browser-usable range',
)
// 这些用例必须留在**共享表**里，而不是只在两侧各自断言一次：共享表才是"Go 与 TS 在同
// 一个值上结论相同"的证明载体。
const expectedPortCases = [
  ['https://example.com:65536/path', false],
  ['https://example.com:99999/path', false],
  ['https://example.com:65535/path', true],
  ['https://example.com:0/path', true],
]
for (const [href, safe] of expectedPortCases) {
  const shared = linkPolicyTable.cases.find((entry) => entry.href === href)
  assert.ok(shared, `the shared link policy table must keep the port boundary case ${href}`)
  assert.equal(shared.safe, safe, `the shared table must expect safe=${safe} for ${href}`)
  assert.equal(safeHrefModule.isSafeHref(href), safe, `TS must agree with the shared table on ${href}`)
  // 顺带把"浏览器到底能不能用这个端口"固定成可执行事实，而不是注释里的断言。
  if (safe) {
    assert.doesNotThrow(() => new URL(href), `${href} must stay usable in a browser`)
  } else {
    assert.throws(() => new URL(href), TypeError, `${href} must be unusable in a browser`)
  }
}

// 契约 fixture 里现有的链接必须全部仍然安全（不得因为收紧而破坏既有 demo）。
for (const item of directory.items) {
  for (const link of item.links ?? []) {
    assert.ok(
      safeHrefModule.isSafeHref(link.href),
      `existing directory fixture link must stay renderable: ${link.href}`,
    )
  }
}
for (const link of overview.links) {
  assert.ok(
    safeHrefModule.isSafeHref(link.href),
    `existing overview fixture link must stay renderable: ${link.href}`,
  )
}
for (const evidence of workspace.evidence_refs) {
  assert.ok(
    safeHrefModule.isSafeHref(evidence.locator.deep_link),
    `existing fixture evidence deep link must stay clickable: ${evidence.locator.deep_link}`,
  )
}

console.log(`service-observability verification passed: 3 routes, ${scenarios.scenarios.length + 1} workspace states, `
  + `${tsSortKeys.length} shared sort keys, executable query-contract regressions`)
