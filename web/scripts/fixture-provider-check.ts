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

  // ---- 显式场景必须战胜过期/非法的 URL focus（Codex R1 复审缺口 1）----

  const staleInvalidFocus = { focusKind: primaryFocus.key, focusValue: 'not-a-sandbox' }
  const partialWithStaleFocus = fixtures.getWorkspaceFixture(serviceId, 'partial', staleInvalidFocus)
  equal('非法旧 focus 不阻挡显式场景', partialWithStaleFocus.scenario?.key, 'telemetry_bundle_timeout_partial_unknown')
  equal('显式场景的 focus 回到场景视图', partialWithStaleFocus.view?.focus?.value, baseFocusValue)
  equal('Partial 场景仍是 partial', partialWithStaleFocus.view?.data_status.state, 'partial')
  check(
    'Partial 场景的 Golden 全为 Unknown（不假绿）',
    (partialWithStaleFocus.view?.golden_signals ?? []).every(
      (signal) => signal.value === null && signal.health === 'unknown',
    ),
  )
  // 未声明的 focus kind 同样不能挡住场景。
  const staleUnknownKind = { focusKind: 'not_declared', focusValue: 'whatever' }
  equal(
    '未声明 focus kind 不阻挡显式场景',
    fixtures.getWorkspaceFixture(serviceId, 'stale', staleUnknownKind).scenario?.key,
    'observed_relation_stale',
  )
  // 但非 focus 字段仍然要校验：场景不是"绕过一切校验"的后门。
  await expectApiError('显式场景仍校验时间窗配对', 'invalid_time_range', () =>
    fixtures.getWorkspaceFixture(serviceId, 'partial', { from: base.scope.from }))
  await expectApiError('显式场景仍校验绝对时间', 'invalid_time_range', () =>
    fixtures.getWorkspaceFixture(serviceId, 'partial', { from: '2026-08-22', to: '2026-08-23' }))
  // 注意：R1 曾断言"未知 service 优先于非法 focus"，那与 live 相反——live 的 parseFocus
  // 在 facade 之前跑，所以缺失服务 + 非法 focus 是 422。正确口径在下方"缺失服务与非法
  // focus 的先后顺序"一节。

  // ---- 与 Go HTTP 边界的口径对齐（Codex R1 复审缺口 3）----

  // trim：`?sort=%20health%20` 在 live 是合法的 health。
  check('标量参数按 Go 口径 trim', fixtures.getDirectoryFixture({
    sort: ' health ', direction: ' desc ', tier: ' tier-1 ',
  }).items.length > 0)
  // CSV：合法多值必须通过；空成员/未知值/超过 8 个成员必须 422。
  check('合法 CSV health 通过', fixtures.getDirectoryFixture({ health: ['critical', 'degraded'] }).items.length > 0)
  check('CSV 重复值静默去重', fixtures.getDirectoryFixture({ health: ['critical', 'critical'] }).items.length > 0)
  await expectApiError('CSV 空成员', 'invalid_filter', () => fixtures.getDirectoryFixture({ health: ['critical', ''] }))
  await expectApiError('CSV 成员过多', 'invalid_filter', () =>
    fixtures.getDirectoryFixture({ health: Array.from({ length: 9 }, () => 'critical') }))
  // 整数语法：JS 的 Number 会接受 0x10 / 1e2 / 5.0，Go 的 Atoi 不会。
  for (const limit of ['0x10', '1e2', '5.0', ' 5 5', 'Infinity']) {
    await expectApiError(`limit=${limit} 必须按整数语法拒绝`, 'invalid_limit', () =>
      fixtures.getDirectoryFixture({ limit }))
  }
  check('limit 允许带正号', fixtures.getDirectoryFixture({ limit: '+50' }).items.length > 0)
  for (const windowSeconds of ['0x10', '1e2', '2.5']) {
    await expectApiError(`window_seconds=${windowSeconds} 必须按整数语法拒绝`, 'invalid_window', () =>
      fixtures.getOverviewFixture(serviceId, { windowSeconds }))
  }
  // RFC3339：日期或无时区时间戳必须拒绝（Date.parse 会接受它们）。
  for (const bound of ['2026-08-22', '2026-08-22T02:04:22', '2026/08/22 02:04:22']) {
    await expectApiError(`from=${bound} 必须按 RFC3339 拒绝`, 'invalid_time_range', () =>
      fixtures.getWorkspaceFixture(serviceId, null, { from: bound, to: base.scope.to }))
  }
  check('带偏移量的 RFC3339 通过', Boolean(fixtures.getWorkspaceFixture(serviceId, null, {
    from: '2026-08-22T10:04:22+08:00', to: '2026-08-22T10:09:22+08:00',
  }).view))
  // timezone 形状上界不放宽（真正的 IANA 加载属于 R2 C4）。
  await expectApiError('超长 timezone', 'invalid_timezone', () =>
    fixtures.getWorkspaceFixture(serviceId, null, { timezone: 'z'.repeat(65) }))

  // ---- RFC3339 与 Go 边界逐例对齐（复审缺口 1）----
  //
  // 期望值来自在 go1.25 上实测 time.Parse(time.RFC3339, ...) 的结果，包括两个 Go 也接受
  // 的"意外"偏移量（+25:00 / +08:60）——用 Date.parse 兜底会在这两例上判反，同时又会把
  // 2026-02-30 规范化成 3 月 2 日而错误通过。
  const rfc3339Table: Array<[string, boolean]> = [
    ['2026-08-22T02:04:22Z', true],
    ['2026-08-22t02:04:22z', false],
    ['2026-08-22T02:04:22z', false],
    ['2026-08-22t02:04:22Z', false],
    ['2026-02-30T02:04:22Z', false],
    ['2026-02-29T02:04:22Z', false],
    ['2024-02-29T02:04:22Z', true],
    ['2026-13-01T02:04:22Z', false],
    ['2026-00-10T02:04:22Z', false],
    ['2026-08-00T02:04:22Z', false],
    ['2026-08-32T02:04:22Z', false],
    ['2026-04-31T02:04:22Z', false],
    ['2026-08-22T24:00:00Z', false],
    ['2026-08-22T23:60:00Z', false],
    ['2026-08-22T23:59:60Z', false],
    ['2026-08-22T23:59:59.123Z', true],
    ['2026-08-22T23:59:59.Z', false],
    ['2026-08-22T23:59:59+08:00', true],
    ['2026-08-22T23:59:59-08:00', true],
    ['2026-08-22T23:59:59+25:00', true],
    ['2026-08-22T23:59:59+08:60', true],
    ['2026-08-22T23:59:59+0800', false],
    ['2026-08-22T23:59:59', false],
    ['2026-08-22', false],
    ['2026-8-22T02:04:22Z', false],
    ['2026-08-22T02:04:22.000000001Z', true],
    ['2026-08-22T02:04:22ZZ', false],
  ]
  for (const [candidate, accepted] of rfc3339Table) {
    const to = accepted ? candidate : base.scope.to
    if (accepted) {
      check(`RFC3339 接受 ${candidate}`, Boolean(fixtures.getWorkspaceFixture(serviceId, null, {
        from: candidate, to: candidate,
      }).view))
    } else {
      await expectApiError(`RFC3339 拒绝 ${candidate}`, 'invalid_time_range', () =>
        fixtures.getWorkspaceFixture(serviceId, null, { from: candidate, to }))
    }
  }

  // ---- 缺失服务与非法 focus 的先后顺序必须与 live 一致（复审缺口 2）----

  // live 的 parseFocus 在 facade 之前跑，所以"语法合法但不存在的 service + 非法 focus"
  // 是 422，不是 404。
  await expectApiError('缺失服务 + 非法 focus 仍是 422', 'invalid_focus', () =>
    fixtures.getWorkspaceFixture('svc_missing_fixture', null, {
      focusKind: primaryFocus.key, focusValue: 'not-a-sandbox',
    }))
  await expectApiError('缺失服务 + 非法时间窗仍是 422', 'invalid_time_range', () =>
    fixtures.getWorkspaceFixture('svc_missing_fixture', null, { from: base.scope.from }))
  // 只有 query 合法时才走到"服务不存在"。
  equal('缺失服务 + 合法 query 是 404', fixtures.getWorkspaceFixture('svc_missing_fixture', null, {
    focusKind: base.focus.kind, focusValue: baseFocusValue,
  }).view, null)
  // 场景例外只对已知服务成立：不存在的服务不能靠加 scenario 绕过 query 校验。
  await expectApiError('缺失服务加 scenario 仍不能绕过 focus 校验', 'invalid_focus', () =>
    fixtures.getWorkspaceFixture('svc_missing_fixture', 'partial', {
      focusKind: primaryFocus.key, focusValue: 'not-a-sandbox',
    }))
  equal('缺失服务加 scenario 仍是 404', fixtures.getWorkspaceFixture('svc_missing_fixture', 'partial', {}).view, null)
  // 形状都不合法的 service ID 先于 query 校验被判 404（对齐 live parseServiceID）。
  equal('超长 service ID 优先 404', fixtures.getWorkspaceFixture('s'.repeat(65), null, {
    focusKind: primaryFocus.key, focusValue: 'not-a-sandbox',
  }).view, null)

  // ---- environment 契约（复审缺口 3）----

  const fixtureEnvironment = base.scope.environment
  await expectApiError('目录非法 environment', 'invalid_filter', () =>
    fixtures.getDirectoryFixture({ environment: '-bad env' }))
  await expectApiError('总览非法 environment', 'invalid_filter', () =>
    fixtures.getOverviewFixture(serviceId, { environment: 'prod uction' }))
  await expectApiError('工作区非法 environment', 'invalid_filter', () =>
    fixtures.getWorkspaceFixture(serviceId, null, { environment: 'x'.repeat(65) }))
  // 匹配或缺省：完整 Page View。
  check('目录缺省 environment 返回完整视图', fixtures.getDirectoryFixture({}).items.length > 0)
  check('目录匹配 environment 返回完整视图',
    fixtures.getDirectoryFixture({ environment: fixtureEnvironment }).items.length > 0)
  equal('总览匹配 environment 可用',
    fixtures.getOverviewFixture(serviceId, { environment: fixtureEnvironment })?.service.service_id, serviceId)
  check('工作区匹配 environment 可用', Boolean(fixtures.getWorkspaceFixture(serviceId, null, {
    environment: fixtureEnvironment, focusKind: base.focus.kind, focusValue: baseFocusValue,
  }).view))
  // 另一个合法环境：目录是有界空视图，总览/工作区是 404。
  const otherEnvironment = `${fixtureEnvironment}-other`
  const otherDirectory = fixtures.getDirectoryFixture({ environment: otherEnvironment })
  equal('另一环境的目录没有条目', otherDirectory.items.length, 0)
  equal('另一环境的目录没有下一页', otherDirectory.next_cursor, null)
  equal('另一环境的目录摘要 managed 归零', otherDirectory.summary.managed, 0)
  equal('另一环境的目录摘要 critical 归零', otherDirectory.summary.critical, 0)
  equal('另一环境的目录摘要 partial 归零', otherDirectory.summary.partial, 0)
  equal('另一环境的目录摘要 unconfigured 归零', otherDirectory.summary.unconfigured, 0)
  check('另一环境的目录不泄露服务事实', !JSON.stringify(otherDirectory).includes(serviceId))
  equal('另一环境的总览是 404',
    fixtures.getOverviewFixture(serviceId, { environment: otherEnvironment }), null)
  equal('另一环境的工作区是 404', fixtures.getWorkspaceFixture(serviceId, null, {
    environment: otherEnvironment, focusKind: base.focus.kind, focusValue: baseFocusValue,
  }).view, null)

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
