// 目录/总览/工作区查询的前端校验口径。
//
// 它存在的唯一目的是让**显式 fixture 模式**给出与 live 完全相同的 422 reason code：
// QA 在 fixture 下看到成功页、上线后同一个 URL 却是 422，比没有 fixture 更糟。
//
// 三条硬约束：
//
//  1. allowlist 只有一个来源（./directory 的常量），页面下拉、这里的校验与静态验证器
//     全部从它派生，不各写一份字面量；
//  2. 本模块保持**纯净**：不 import fixture、不 import API client、不碰 `window`，
//     因此可以被 Node 直接编译执行，回归测试跑的是真实实现而不是它的副本；
//  3. 它**只用于 fixture 模式**。live 模式必须把原始 URL 值送到服务端换回权威 422：
//     在客户端拦一层会让"URL 是事实源"退化成"客户端先猜一遍"。
import {
  directoryDataStatusFilters,
  directoryHealthFilters,
  directorySortKeys,
  isDirectoryDataStatusFilter,
  isDirectoryHealthFilter,
  isDirectorySortKey,
  isServiceTierFilter,
  isSortDirection,
  serviceTierFilters,
} from './directory'

/** 与后端 internal/api/service_observability.go 完全一致的稳定 reason code。 */
export const queryReasonCodes = {
  invalidFilter: 'invalid_filter',
  invalidLimit: 'invalid_limit',
  invalidWindow: 'invalid_window',
  invalidCursor: 'invalid_cursor',
  invalidFocus: 'invalid_focus',
  invalidTimeRange: 'invalid_time_range',
  invalidTimezone: 'invalid_timezone',
} as const

/** 与后端一致的参数上界。 */
export const queryLimits = {
  maxQueryLength: 128,
  maxCursorLength: 512,
  maxPageLimit: 200,
  maxTimezoneLength: 64,
  maxFocusValueLength: 256,
} as const

/**
 * QueryValidationError 是校验失败的 typed 结果。
 *
 * 它刻意不是 ApiError：那会把本模块绑到 api/client（进而绑到 `window` 运行时配置）上，
 * 使它无法在 Node 里被直接测试。fixture provider 负责把它翻译成 `ApiError(422, …)`，
 * 于是页面的错误分支只有一条。
 */
export class QueryValidationError extends Error {
  reasonCode: string

  constructor(reasonCode: string, message: string) {
    super(message)
    this.name = 'QueryValidationError'
    this.reasonCode = reasonCode
  }
}

function reject(reasonCode: string, message: string): never {
  throw new QueryValidationError(reasonCode, message)
}

export interface DirectoryQueryInput {
  tier?: string
  health?: string[]
  dataStatus?: string[]
  q?: string
  sort?: string
  direction?: string
  cursor?: string
  limit?: number | string
}

export interface OverviewQueryInput {
  windowSeconds?: number | string
}

export interface FocusKindContract {
  key: string
  value_pattern: string
}

export interface WorkspaceQueryInput {
  focusKind?: string
  focusValue?: string
  from?: string
  to?: string
  timezone?: string
}

function assertAllowlist(
  values: string[] | undefined,
  accepted: (value: string) => boolean,
  message: string,
): void {
  if (!values) return
  for (const value of values) {
    if (value.trim() === '' || !accepted(value)) reject(queryReasonCodes.invalidFilter, message)
  }
}

/** validateDirectoryQuery 校验目录查询参数。 */
export function validateDirectoryQuery(query: DirectoryQueryInput): void {
  if (query.sort !== undefined && query.sort !== '' && !isDirectorySortKey(query.sort)) {
    reject(queryReasonCodes.invalidFilter, `sort 只支持 ${directorySortKeys.join('/')}`)
  }
  if (query.direction !== undefined && query.direction !== '' && !isSortDirection(query.direction)) {
    reject(queryReasonCodes.invalidFilter, 'direction 只支持 asc 或 desc')
  }
  if (query.tier !== undefined && query.tier !== '' && !isServiceTierFilter(query.tier)) {
    reject(queryReasonCodes.invalidFilter, `tier 只支持 ${serviceTierFilters.join('/')}`)
  }
  assertAllowlist(query.health, isDirectoryHealthFilter, `health 只支持 ${directoryHealthFilters.join('/')}`)
  assertAllowlist(
    query.dataStatus,
    isDirectoryDataStatusFilter,
    `data_status 只支持 ${directoryDataStatusFilters.join('/')}`,
  )
  if (query.q !== undefined && query.q.length > queryLimits.maxQueryLength) {
    reject(queryReasonCodes.invalidFilter, '搜索片段过长')
  }
  if (query.limit !== undefined && query.limit !== '') {
    const limit = Number(query.limit)
    if (!Number.isInteger(limit) || limit < 1 || limit > queryLimits.maxPageLimit) {
      reject(queryReasonCodes.invalidLimit, `limit 必须是 1 到 ${queryLimits.maxPageLimit} 之间的整数`)
    }
  }
  if (query.cursor !== undefined && query.cursor.length > queryLimits.maxCursorLength) {
    reject(queryReasonCodes.invalidCursor, '游标非法或已过期，请重新从第一页开始')
  }
}

/** validateOverviewQuery 校验总览查询参数。 */
export function validateOverviewQuery(query: OverviewQueryInput): void {
  if (query.windowSeconds === undefined || query.windowSeconds === '') return
  const windowSeconds = Number(query.windowSeconds)
  if (!Number.isInteger(windowSeconds) || windowSeconds < 1) {
    reject(queryReasonCodes.invalidWindow, 'window_seconds 必须是正整数')
  }
}

/**
 * validateWorkspaceQuery 校验工作区查询参数。
 *
 * focusKinds 由调用方（fixture provider）从已发布 Profile 投影里取，因此本模块不认识
 * 任何具体服务：没有 focus kind 字面量，也没有服务专属 ID 分支。
 *
 * 时间窗只校验配对与绝对时间格式。基数/窗口上界依赖 Profile limits，属于服务端职责，
 * fixture 不假装拥有那份配置。
 */
export function validateWorkspaceQuery(
  focusKinds: FocusKindContract[],
  query: WorkspaceQueryInput,
): void {
  const kind = query.focusKind?.trim() ?? ''
  const value = query.focusValue?.trim() ?? ''
  if ((kind === '') !== (value === '')) {
    reject(queryReasonCodes.invalidFocus, 'focus_kind 与 focus_value 必须同时提供')
  }
  if (kind !== '') {
    if (value.length > queryLimits.maxFocusValueLength) {
      reject(queryReasonCodes.invalidFocus, 'focus_value 过长')
    }
    const declared = focusKinds.find((focus) => focus.key === kind)
    if (!declared || !new RegExp(declared.value_pattern).test(value)) {
      reject(queryReasonCodes.invalidFocus, 'focus 不满足该服务已发布 Profile 的定义')
    }
  }
  const from = query.from?.trim() ?? ''
  const to = query.to?.trim() ?? ''
  if ((from === '') !== (to === '')) {
    reject(queryReasonCodes.invalidTimeRange, 'from 与 to 必须同时提供')
  }
  for (const bound of [from, to]) {
    if (bound !== '' && Number.isNaN(Date.parse(bound))) {
      reject(queryReasonCodes.invalidTimeRange, 'from/to 必须是带时区的绝对 RFC3339 时间')
    }
  }
  const timezone = query.timezone?.trim() ?? ''
  if (timezone !== '' && timezone.length > queryLimits.maxTimezoneLength) {
    reject(queryReasonCodes.invalidTimezone, 'timezone 格式非法')
  }
}

/**
 * alignFocusIdentity 把视图里所有等于 previous 的字符串换成 next。
 *
 * previous 由调用方从 fixture 自身的 `focus.value` 取，因此这里没有任何服务专属 ID
 * 字面量——"通用 renderer 不含 E2B 分支"在类型与实现两侧都成立。它只统一"这一次聚焦的
 * 是哪个请求"这一个身份值，不改任何观测状态、数值或时间。
 */
export function alignFocusIdentity<T>(view: T, previous: string, next: string): T {
  if (previous === '' || previous === next) return view
  const rewrite = (node: unknown): unknown => {
    if (typeof node === 'string') return node === previous ? next : node
    if (Array.isArray(node)) return node.map(rewrite)
    if (node !== null && typeof node === 'object') {
      return Object.fromEntries(Object.entries(node).map(([key, value]) => [key, rewrite(value)]))
    }
    return node
  }
  return rewrite(view) as T
}
