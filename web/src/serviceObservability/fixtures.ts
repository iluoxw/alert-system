import directoryViewRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.directory-view.json?raw'
import overviewViewRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.overview-view.json?raw'
import workspaceViewRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.workspace-view.json?raw'
import workspaceScenariosRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.workspace-scenarios.json?raw'
import { ApiError } from '../api/client'
import type {
  ServiceDirectoryQuery,
  ServiceOverviewQuery,
  ServiceWorkspaceQuery,
} from '../api/client'
import {
  QueryValidationError,
  alignFocusIdentity,
  validateEnvironment,
  validateDirectoryQuery,
  validateOverviewQuery,
  validateWorkspaceFocus,
  validateWorkspaceScope,
} from './queryContract'
import type {
  JsonPatchOperation,
  ObservabilityWorkspaceView,
  ServiceDirectoryView,
  ServiceOverviewView,
  WorkspaceScenarioFixture,
  WorkspaceScenarios,
} from './types'

const directoryFixture = JSON.parse(directoryViewRaw) as ServiceDirectoryView
const overviewFixture = JSON.parse(overviewViewRaw) as ServiceOverviewView
const workspaceFixture = JSON.parse(workspaceViewRaw) as ObservabilityWorkspaceView
const workspaceScenarioFixture = JSON.parse(workspaceScenariosRaw) as WorkspaceScenarios

export const workspaceScenarioAliases = {
  partial: 'telemetry_bundle_timeout_partial_unknown',
  forbidden: 'logs_forbidden_traces_allowed',
  empty: 'focus_empty_valid_query',
  stale: 'observed_relation_stale',
} as const

export type WorkspaceScenarioAlias = keyof typeof workspaceScenarioAliases

function decodePointerSegment(segment: string): string {
  return segment.replaceAll('~1', '/').replaceAll('~0', '~')
}

function pointerSegments(path: string): string[] {
  if (path === '') return []
  if (!path.startsWith('/')) throw new Error(`非法 JSON Pointer: ${path}`)
  return path.slice(1).split('/').map(decodePointerSegment)
}

function parentAtPath(document: unknown, path: string): { parent: Record<string, unknown> | unknown[]; key: string } {
  const segments = pointerSegments(path)
  const key = segments.pop()
  if (key === undefined) throw new Error('场景 patch 不允许替换文档根节点')
  let cursor = document
  for (const segment of segments) {
    if (Array.isArray(cursor)) {
      const index = Number(segment)
      if (!Number.isInteger(index) || index < 0 || index >= cursor.length) {
        throw new Error(`场景 patch 数组路径不存在: ${path}`)
      }
      cursor = cursor[index]
    } else if (cursor !== null && typeof cursor === 'object' && segment in cursor) {
      cursor = (cursor as Record<string, unknown>)[segment]
    } else {
      throw new Error(`场景 patch 对象路径不存在: ${path}`)
    }
  }
  if (cursor === null || typeof cursor !== 'object') throw new Error(`场景 patch 父路径不是容器: ${path}`)
  return { parent: cursor as Record<string, unknown> | unknown[], key }
}

export function applyJsonPatch<T>(source: T, operations: JsonPatchOperation[]): T {
  const document = structuredClone(source) as unknown
  for (const operation of operations) {
    const { parent, key } = parentAtPath(document, operation.path)
    if (Array.isArray(parent)) {
      const index = key === '-' ? parent.length : Number(key)
      if (!Number.isInteger(index) || index < 0 || index > parent.length) {
        throw new Error(`场景 patch 数组索引非法: ${operation.path}`)
      }
      if (operation.op === 'remove') {
        if (index >= parent.length) throw new Error(`场景 patch 删除路径不存在: ${operation.path}`)
        parent.splice(index, 1)
      } else if (operation.op === 'add') {
        parent.splice(index, 0, structuredClone(operation.value))
      } else {
        if (index >= parent.length) throw new Error(`场景 patch 替换路径不存在: ${operation.path}`)
        parent[index] = structuredClone(operation.value)
      }
      continue
    }

    if (operation.op === 'remove') {
      if (!(key in parent)) throw new Error(`场景 patch 删除路径不存在: ${operation.path}`)
      delete parent[key]
    } else {
      if (operation.op === 'replace' && !(key in parent)) {
        throw new Error(`场景 patch 替换路径不存在: ${operation.path}`)
      }
      parent[key] = structuredClone(operation.value)
    }
  }
  return document as T
}

// ---- 与后端同口径的轻量 query 校验 ----
//
// 校验实现只有一份，落在纯净的 ./queryContract（可被 Node 直接编译执行，因此回归测试
// 跑的是真实实现而不是它的副本）。这里只把 typed 校验错误翻译成
// `ApiError(422, …, reason_code)`，让 fixture 与 live 的错误页面走同一条分支——否则 QA
// 在 fixture 下看到成功页、上线后同一个 URL 却是 422。
function asApiError(error: unknown): never {
  if (error instanceof QueryValidationError) {
    throw new ApiError(422, error.message, undefined, error.reasonCode)
  }
  throw error
}

function guardDirectoryQuery(query: ServiceDirectoryQuery): void {
  try {
    validateDirectoryQuery(query)
  } catch (error) {
    asApiError(error)
  }
}

function guardOverviewQuery(query: ServiceOverviewQuery): void {
  try {
    validateOverviewQuery(query)
  } catch (error) {
    asApiError(error)
  }
}

// guardWorkspaceScope 只校验非 focus 字段（时间窗配对、绝对时间、时区形状）。
//
// 显式 QA 场景走这一条：场景视图自带权威 focus，URL 上可能还留着上一次的旧 focus，
// 用它把场景判成 422 会让确定性的五态 QA 状态变得不可达。
function guardWorkspaceScope(query: ServiceWorkspaceQuery): void {
  try {
    validateWorkspaceScope(query)
  } catch (error) {
    asApiError(error)
  }
}

// guardWorkspaceFocus 校验 focus。focus 合法性以该服务已发布 Profile 投影出的
// focus_kinds 为准：provider 不认识任何具体 focus kind 或服务专属 ID 字面量。
function guardWorkspaceFocus(query: ServiceWorkspaceQuery): void {
  try {
    validateWorkspaceFocus(overviewFixture.focus_kinds, query)
  } catch (error) {
    asApiError(error)
  }
}

function guardEnvironment(environment: string | undefined): string {
  try {
    return validateEnvironment(environment)
  } catch (error) {
    asApiError(error)
  }
}

// maxServiceIDBytes 与后端 parseServiceID 的上界一致：形状都不满足的 ID 一律按"服务不
// 存在"处理，并且**先于**任何 query 校验——live 就是这么做的。
const maxServiceIDBytes = 64

function isUsableServiceID(serviceId: string): boolean {
  const trimmed = serviceId.trim()
  return trimmed !== '' && new TextEncoder().encode(trimmed).length <= maxServiceIDBytes
}

// fixture 的契约文件是**一个环境作用域**的 Page View。环境从 Page View 数据派生，
// 没有任何服务专属 ID 分支。
function directoryEnvironments(): Set<string> {
  return new Set(directoryFixture.items.map((item) => item.service.environment))
}

// emptyDirectoryView 返回一个有界的空目录：没有条目、没有下一页、摘要全零。
//
// 请求另一个环境时必须是这个形状，而不是把当前环境的事实端出去——那就是跨环境泄露
// 生产事实，也会让"这个环境有多少受管服务"变成一个假数字。
function emptyDirectoryView(view: ServiceDirectoryView): ServiceDirectoryView {
  return {
    ...view,
    summary: { managed: 0, critical: 0, partial: 0, unconfigured: 0 },
    items: [],
    next_cursor: null,
  }
}

export function getDirectoryFixture(query: ServiceDirectoryQuery = {}): ServiceDirectoryView {
  guardDirectoryQuery(query)
  const environment = guardEnvironment(query.environment)
  const view = structuredClone(directoryFixture)
  // 未传 environment 或命中 fixture 自身环境 -> 返回完整 Page View（包含它自己撰写的
  // summary 分母）；显式请求另一个环境 -> 有界空视图。
  if (environment === '' || directoryEnvironments().has(environment)) return view
  return emptyDirectoryView(view)
}

export function getOverviewFixture(
  serviceId: string,
  query: ServiceOverviewQuery = {},
): ServiceOverviewView | null {
  // 与 live 同序：service ID 形状 -> environment -> window -> 服务存在性。
  if (!isUsableServiceID(serviceId)) return null
  guardOverviewQuery(query)
  const environment = guardEnvironment(query.environment)
  if (overviewFixture.service.service_id !== serviceId) return null
  // 显式环境与该服务登记环境不符统一按"服务不存在"处理，不泄露其它环境是否存在。
  if (environment !== '' && environment !== overviewFixture.service.environment) return null
  return structuredClone(overviewFixture)
}

export function getBaseWorkspaceFixture(serviceId: string): ObservabilityWorkspaceView | null {
  if (workspaceFixture.service.service_id !== serviceId) return null
  return structuredClone(workspaceFixture)
}

export function resolveWorkspaceScenario(value: string | null): WorkspaceScenarioFixture | null {
  if (!value || value === 'main') return null
  const key = value in workspaceScenarioAliases
    ? workspaceScenarioAliases[value as WorkspaceScenarioAlias]
    : value
  return workspaceScenarioFixture.scenarios.find((scenario) => scenario.key === key) ?? null
}

/**
 * getWorkspaceFixture 返回显式 fixture 模式下的工作区视图。
 *
 * 它是 query-aware 的——这正是 C2 要修的缺口：以前无论 URL 上的 focus 是什么，返回的都是
 * 固定 patch，于是"输入框/URL 写着 A、Journey 卡显示 B"。现在的口径：
 *
 *   1. 显式 `scenario=` 优先（确定性五态视觉 QA）：返回该场景视图，focus 以场景为准，
 *      调用方据此把 URL 规范化，不保留互相矛盾的 focus 参数；
 *   2. 没有 scenario 时，请求 focus 与 base 主视图相同 -> 返回 main；
 *   3. 合法但不同的 focus -> 应用既有 Empty 场景，并把 focus 身份对齐到请求值，
 *      于是 URL、输入框、Journey 与诊断结论互相一致；
 *   4. focus 非法 -> 抛 422 invalid_focus（与 live 同一 reason code）。
 *
 * 顺序对齐 live handler，只有一个**刻意的 fixture-only 例外**：
 *
 *   - live 的 parseServiceID 只看形状（非空、<=64 字节），形状不合法直接 404，因此这里
 *     也先判形状；
 *   - 之后 live 依次校验 environment -> focus -> 时间窗 -> timezone，**再**由 facade 判
 *     "服务不存在"。所以一个语法合法但不存在的 service 配上非法 focus 在 live 是 422，
 *     不是 404；正常路径必须照此排序，否则 fixture 会把 422 说成 404；
 *   - 唯一例外：**已知**服务 + 显式 QA `scenario=` 时，场景视图自带权威 focus，URL 上
 *     可能还留着上一次的旧 focus。用它把场景判成 422 会让确定性五态 QA 不可达，因此
 *     这条分支跳过 focus 校验并把 focus 规范化到场景视图。它只对已知服务成立——不存在
 *     的服务不能靠加一个 scenario 参数绕过 query 校验。
 */
export function getWorkspaceFixture(
  serviceId: string,
  scenarioValue: string | null,
  query: ServiceWorkspaceQuery = {},
): {
  view: ObservabilityWorkspaceView | null
  scenario: WorkspaceScenarioFixture | null
} {
  if (!isUsableServiceID(serviceId)) return { view: null, scenario: null }

  const base = getBaseWorkspaceFixture(serviceId)
  const scenario = resolveWorkspaceScenario(scenarioValue)
  if (base && scenario) {
    // fixture-only 例外：非 focus 字段（environment、时间窗、timezone）仍然校验。
    guardWorkspaceScope(query)
    return { view: applyJsonPatch(base, scenario.patch), scenario }
  }

  // 正常路径与 live 同序：先把 query 判完，再判服务存在性。
  guardWorkspaceFocus(query)
  guardWorkspaceScope(query)
  if (!base) return { view: null, scenario: null }

  // 显式环境与该服务登记环境不符统一按"服务不存在"处理。
  const environment = guardEnvironment(query.environment)
  if (environment !== '' && environment !== base.scope.environment) {
    return { view: null, scenario: null }
  }

  const requested = query.focusValue?.trim() ?? ''
  const baseFocusValue = base.focus?.value ?? ''
  if (requested === '' || requested === baseFocusValue) {
    return { view: base, scenario: null }
  }

  // 合法但不同的 focus：一次成功查询、当前范围内没有匹配请求。Empty 不等于 Healthy——
  // 服务级信号与 declared topology 由场景本身保留，这里只对齐身份。
  const emptyScenario = resolveWorkspaceScenario(workspaceScenarioAliases.empty)
  if (!emptyScenario) return { view: base, scenario: null }
  const emptyView = applyJsonPatch(base, emptyScenario.patch)
  const aligned = alignFocusIdentity(emptyView, emptyView.focus?.value ?? '', requested)
  // scenario 返回 null：这是"按请求 focus 得到的 Empty 结果"，不是用户选择的 QA 场景，
  // URL 不应因此被写上 scenario 参数。
  return { view: aligned, scenario: null }
}

export function getWorkspaceScenarioCatalog(): WorkspaceScenarioFixture[] {
  return structuredClone(workspaceScenarioFixture.scenarios)
}
