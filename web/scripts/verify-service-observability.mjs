import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const scriptDir = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDir, '../..')
const examplesDir = path.join(repoRoot, 'docs/contracts/service-observability/v1/examples')

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
  'scenario may only be reset inside the fixture-mode branch',
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

console.log(`service-observability verification passed: 3 routes, ${scenarios.scenarios.length + 1} workspace states`)
