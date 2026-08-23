// 显式 fixture 模式的 provider 行为回归。
//
// 它跑的是**真实模块**：契约 example 通过 Vite 的 `?raw` 导入进来，校验与身份对齐来自
// queryContract，patch 应用来自 fixtures 自己的 applyJsonPatch。因此这里断言的是 C1/C2
// 的实际实现，而不是它在测试里的副本。
//
// 由 `npm run test:service-observability` 通过 Vite SSR 构建后在 Node 里执行。
// api/client 在模块加载时会读取运行时配置，所以入口先装好最小 window 再动态 import。

type Failure = { label: string; detail: string }

const failures: Failure[] = []
let checks = 0

function check(label: string, condition: boolean, detail = ''): void {
  checks += 1
  if (!condition) failures.push({ label, detail })
}

function equal(label: string, actual: unknown, expected: unknown): void {
  check(label, actual === expected, `actual=${String(actual)} expected=${String(expected)}`)
}

async function expectApiError(
  label: string,
  reasonCode: string,
  run: () => unknown,
): Promise<void> {
  checks += 1
  try {
    run()
    failures.push({ label, detail: '没有抛出错误' })
  } catch (error) {
    const thrown = error as { status?: number; reasonCode?: string }
    if (thrown.status !== 422 || thrown.reasonCode !== reasonCode) {
      failures.push({
        label,
        detail: `status=${String(thrown.status)} reason_code=${String(thrown.reasonCode)} 期望 422/${reasonCode}`,
      })
    }
  }
}

async function main(): Promise<void> {
  // api/client 顶层读取 window.__FIREBREAK_CONFIG__；SSR 场景下先装最小实现。
  const globalScope = globalThis as unknown as { window?: unknown }
  globalScope.window = { __FIREBREAK_CONFIG__: { apiBaseUrl: '/api' } }

  const fixtures = await import('../src/serviceObservability/fixtures')
  const overview = fixtures.getOverviewFixture('svc_e2b_fixture')
  if (!overview) throw new Error('总览 fixture 缺失，无法继续')
  const serviceId = overview.service.service_id
  const primaryFocus = overview.focus_kinds.find((focus) => focus.primary)
  if (!primaryFocus) throw new Error('fixture 必须恰好声明一个 primary focus')

  const base = fixtures.getBaseWorkspaceFixture(serviceId)
  if (!base?.focus) throw new Error('工作区 fixture 必须带 focus')
  const baseFocusValue = base.focus.value

  // ---- C1：fixture 与 live 的 422 reason code 必须一致 ----
  await expectApiError('目录非法 sort', 'invalid_filter', () => fixtures.getDirectoryFixture({ sort: 'cpu' }))
  await expectApiError('目录非法 direction', 'invalid_filter', () => fixtures.getDirectoryFixture({ direction: 'sideways' }))
  await expectApiError('目录非法 health', 'invalid_filter', () => fixtures.getDirectoryFixture({ health: ['exploded'] }))
  await expectApiError('目录非法 data_status', 'invalid_filter', () => fixtures.getDirectoryFixture({ dataStatus: ['fresh'] }))
  await expectApiError('目录非法 limit', 'invalid_limit', () => fixtures.getDirectoryFixture({ limit: '0' }))
  await expectApiError('总览非法 window', 'invalid_window', () => fixtures.getOverviewFixture(serviceId, { windowSeconds: '0' }))
  await expectApiError('工作区只传 focus_kind', 'invalid_focus', () => fixtures.getWorkspaceFixture(serviceId, null, { focusKind: primaryFocus.key }))
  await expectApiError('工作区 focus 不满足 pattern', 'invalid_focus', () => fixtures.getWorkspaceFixture(serviceId, null, {
    focusKind: primaryFocus.key, focusValue: 'not-a-sandbox',
  }))
  await expectApiError('工作区只传 from', 'invalid_time_range', () => fixtures.getWorkspaceFixture(serviceId, null, { from: base.scope.from }))

  // 合法查询必须仍然成功。
  check('合法目录查询成功', fixtures.getDirectoryFixture({
    sort: 'health', direction: 'desc', health: ['critical'], dataStatus: ['partial'], limit: 50,
  }).items.length > 0)

  // ---- C2：query-aware 主视图 / Empty / 场景 ----

  const main = fixtures.getWorkspaceFixture(serviceId, null, {
    focusKind: base.focus.kind, focusValue: baseFocusValue,
  })
  equal('请求 base focus 返回主视图 focus', main.view?.focus?.value, baseFocusValue)
  equal('主视图不是 QA 场景', main.scenario, null)
  const mainJourneyStates = (main.view?.journey ?? []).map((stage) => stage.state).join(',')
  check('主视图保留完整 journey 事实', mainJourneyStates.includes('ok'), mainJourneyStates)

  // 合法但不同的 focus -> bounded Empty，且身份与请求一致。
  // 刻意构造成"既不是主视图 focus 的子串也不是它的超串"，否则下面的"不残留旧身份"
  // 断言会因为子串包含而失真。
  const requested = `${baseFocusValue.slice(0, -1)}q9`
  check('构造的请求 focus 仍满足 profile pattern', new RegExp(primaryFocus.value_pattern).test(requested), requested)
  const empty = fixtures.getWorkspaceFixture(serviceId, null, {
    focusKind: base.focus.kind, focusValue: requested,
  })
  equal('Empty 视图 focus 与请求一致', empty.view?.focus?.value, requested)
  equal('按 focus 得到的 Empty 不是 QA 场景', empty.scenario, null)
  const emptySerialized = JSON.stringify(empty.view)
  check('Empty 视图不残留场景固定 ID', !emptySerialized.includes('sbx_notfound1'), 'scenario identity leaked')
  check('Empty 视图不残留主视图 focus', !emptySerialized.includes(baseFocusValue), 'main focus leaked')
  const journeySection = (empty.view?.sections ?? []).find((section) => section.section_key === 'journey')
  equal('Empty 的 journey section 是 empty', journeySection?.state, 'empty')
  check(
    'Empty 的每个阶段都是 unknown（Empty 不等于 Healthy）',
    (empty.view?.journey ?? []).every((stage) => stage.state === 'unknown'),
  )
  equal('Empty 仍是一次成功查询', empty.view?.data_status.state, 'complete')
  equal('Empty 的证据集合为空', empty.view?.evidence_refs.length, 0)

  // main -> Empty -> main：回到 base focus 必须完整恢复主视图。
  const backToMain = fixtures.getWorkspaceFixture(serviceId, null, {
    focusKind: base.focus.kind, focusValue: baseFocusValue,
  })
  equal('Empty 之后回到主视图', backToMain.view?.focus?.value, baseFocusValue)
  equal(
    'Empty 之后主视图逐字节恢复',
    JSON.stringify(backToMain.view), JSON.stringify(main.view),
  )

  // 显式 QA 场景优先，并且 focus 以场景为准（调用方据此规范化 URL）。
  const forbidden = fixtures.getWorkspaceFixture(serviceId, 'forbidden', {
    focusKind: base.focus.kind, focusValue: requested,
  })
  equal('显式场景被解析', forbidden.scenario?.key, 'logs_forbidden_traces_allowed')
  equal('显式场景的 focus 以场景为准', forbidden.view?.focus?.value, baseFocusValue)
  equal('Forbidden 场景不改服务健康', forbidden.view?.health.status, base.health.status)
  equal('Forbidden 场景禁用日志能力', forbidden.view?.capabilities.view_logs, false)
  equal('Forbidden 场景保留 traces 能力', forbidden.view?.capabilities.view_traces, true)

  // 未知服务返回 null，让页面走统一 404 状态。
  equal('未知服务的工作区为 null', fixtures.getWorkspaceFixture('svc_unknown', null, {}).view, null)
  equal('未知服务的总览为 null', fixtures.getOverviewFixture('svc_unknown'), null)

  if (failures.length > 0) {
    for (const failure of failures) {
      console.error(`FAIL ${failure.label}${failure.detail ? `: ${failure.detail}` : ''}`)
    }
    throw new Error(`fixture provider 回归失败 ${failures.length}/${checks}`)
  }
  console.log(`fixture provider regression passed: ${checks} checks`)
}

await main()
