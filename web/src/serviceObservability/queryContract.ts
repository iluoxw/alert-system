// 目录/总览/工作区查询的前端校验口径。
//
// 它存在的唯一目的是让**显式 fixture 模式**给出与 live 完全相同的 422 reason code：
// QA 在 fixture 下看到成功页、上线后同一个 URL 却是 422，比没有 fixture 更糟。
//
// 四条硬约束：
//
//  1. allowlist 只有一个来源（./directory 的常量），页面下拉、这里的校验与静态验证器
//     全部从它派生，不各写一份字面量；
//  2. 本模块保持**纯净**：不 import fixture、不 import API client、不碰 `window`，
//     因此可以被 Node 直接编译执行，回归测试跑的是真实实现而不是它的副本；
//  3. 它**只用于 fixture 模式**。live 模式必须把原始 URL 值送到服务端换回权威 422：
//     在客户端拦一层会让"URL 是事实源"退化成"客户端先猜一遍"；
//  4. 语义必须逐条对齐 internal/api/service_observability.go 的 HTTP 边界——包括
//     trim 口径、CSV 去重与上限、整数**语法**（不是 JS 的宽松数字转换）、以及
//     RFC3339 必须带 Z/偏移量。差一点就会出现"fixture 通过、live 422"的假绿。
//
// 刻意保留的口径差异只有一条：后端要求 `team_id` 是 UUID，而目录 fixture 用的是
// `team_platform_sre_fixture` 这类符号 ID（Phase 1 demo 事实）。在 fixture 侧强制 UUID
// 会让 fixture demo 自己的团队筛选 422，因此这里不校验 team_id 形状；live 仍由后端把关。
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

/**
 * 与后端一致的参数上界。
 *
 * 长度上界在后端是**字节**（Go 的 `len(string)`），所以这里也按 UTF-8 字节算：按
 * UTF-16 长度算会让 43 个中文字符在 fixture 下通过、在 live 下 422。
 */
export const queryLimits = {
  maxEnvironmentBytes: 64,
  maxQueryBytes: 128,
  maxCursorLength: 512,
  maxPageLimit: 200,
  maxTimezoneBytes: 64,
  maxFocusKindBytes: 64,
  maxFocusValueBytes: 256,
  maxCSVValues: 8,
} as const

const utf8Encoder = new TextEncoder()

function byteLength(value: string): number {
  return utf8Encoder.encode(value).length
}

// containsControlCharacters 与后端同口径：拒绝 C0 控制字符与 DEL。
function containsControlCharacters(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    if (code < 0x20 || code === 0x7f) return true
  }
  return false
}

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
  environment?: string
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
  environment?: string
  windowSeconds?: number | string
}

export interface FocusKindContract {
  key: string
  value_pattern: string
}

export interface WorkspaceQueryInput {
  environment?: string
  focusKind?: string
  focusValue?: string
  from?: string
  to?: string
  timezone?: string
}

// environmentPattern 与后端 internal/api/service_observability.go 完全一致。
// 三条路由都校验 environment，因此 fixture 侧缺了它就会出现"fixture 通过、live 422"。
const environmentPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/

/**
 * validateEnvironment 校验 environment 形状并返回 trim 后的值。
 *
 * 空值（trim 后为空）表示"未传"：后端据此沿用 registration 环境，fixture 据此返回
 * 自身环境的 Page View。形状非法一律 422 invalid_filter。
 */
export function validateEnvironment(environment: string | undefined): string {
  const value = scalar(environment)
  if (value === '') return ''
  if (byteLength(value) > queryLimits.maxEnvironmentBytes || !environmentPattern.test(value)) {
    reject(queryReasonCodes.invalidFilter, 'environment 格式非法')
  }
  return value
}

/**
 * scalar 取出一个标量参数：trim 后为空视为"未传"。
 *
 * 后端对每个查询参数都先 `strings.TrimSpace`，所以 `?sort=%20health%20` 在 live 是合法的
 * `health`。fixture 不 trim 就会把同一个 URL 判成非法。
 */
function scalar(value: string | undefined): string {
  return value === undefined ? '' : value.trim()
}

/**
 * parseCSVMembers 与后端 splitCSV 逐条对齐。
 *
 * 顺序也一致：先判成员数量上限，再逐个 trim、拒绝空成员、静默去重。数量判定发生在去重
 * **之前**，因此 `a,a,a,…` 超过 8 个成员同样是 422，而不是先去重后侥幸通过。
 */
export function parseCSVMembers(raw: string | undefined): string[] {
  const trimmed = scalar(raw)
  if (trimmed === '') return []
  const parts = trimmed.split(',')
  if (parts.length > queryLimits.maxCSVValues) {
    reject(queryReasonCodes.invalidFilter, '筛选值过多')
  }
  const seen = new Set<string>()
  const values: string[] = []
  for (const part of parts) {
    const value = part.trim()
    if (value === '') reject(queryReasonCodes.invalidFilter, '筛选值不能为空')
    if (seen.has(value)) continue
    seen.add(value)
    values.push(value)
  }
  return values
}

/**
 * parseCSVQueryValues 把页面传下来的 CSV 数组还原成 URL 原文再解析。
 *
 * 页面为了保持"URL 原样下传"会把 `health=a,b` 拆成 `['a','b']`，client 再 join 回去。
 * 这里用同一个 join 复原原文，保证 fixture 与 live 看到的是同一个字符串（包括
 * `critical,` 这种末尾空成员）。
 */
export function parseCSVQueryValues(values: string[] | undefined): string[] {
  if (!values || values.length === 0) return []
  return parseCSVMembers(values.join(','))
}

// 严格的十进制整数语法，对齐 Go 的 strconv.Atoi：接受可选正负号 + 纯数字，
// 拒绝 `0x10`、`1e2`、`5.0`、`Infinity` 与全角数字——JS 的 Number() 会把前三个都接受，
// 于是 `limit=0x10` 在 fixture 通过、在 live 422。
const decimalIntegerPattern = /^[+-]?[0-9]+$/

function parseIntegerParameter(
  raw: number | string | undefined,
  reasonCode: string,
  message: string,
): number | undefined {
  if (raw === undefined) return undefined
  if (typeof raw === 'number') {
    if (!Number.isInteger(raw)) reject(reasonCode, message)
    return raw
  }
  const trimmed = raw.trim()
  if (trimmed === '') return undefined
  if (!decimalIntegerPattern.test(trimmed)) reject(reasonCode, message)
  const parsed = Number(trimmed)
  if (!Number.isSafeInteger(parsed)) reject(reasonCode, message)
  return parsed
}

function assertAllowlist(
  values: string[],
  accepted: (value: string) => boolean,
  message: string,
): void {
  for (const value of values) {
    if (!accepted(value)) reject(queryReasonCodes.invalidFilter, message)
  }
}

/** validateDirectoryQuery 校验目录查询参数。顺序与 live handler 一致：environment 最先。 */
export function validateDirectoryQuery(query: DirectoryQueryInput): void {
  validateEnvironment(query.environment)
  const sort = scalar(query.sort)
  if (sort !== '' && !isDirectorySortKey(sort)) {
    reject(queryReasonCodes.invalidFilter, `sort 只支持 ${directorySortKeys.join('/')}`)
  }
  const direction = scalar(query.direction)
  if (direction !== '' && !isSortDirection(direction)) {
    reject(queryReasonCodes.invalidFilter, 'direction 只支持 asc 或 desc')
  }
  const tier = scalar(query.tier)
  if (tier !== '' && !isServiceTierFilter(tier)) {
    reject(queryReasonCodes.invalidFilter, `tier 只支持 ${serviceTierFilters.join('/')}`)
  }
  assertAllowlist(
    parseCSVQueryValues(query.health),
    isDirectoryHealthFilter,
    `health 只支持 ${directoryHealthFilters.join('/')}`,
  )
  assertAllowlist(
    parseCSVQueryValues(query.dataStatus),
    isDirectoryDataStatusFilter,
    `data_status 只支持 ${directoryDataStatusFilters.join('/')}`,
  )
  const q = scalar(query.q)
  if (byteLength(q) > queryLimits.maxQueryBytes || containsControlCharacters(q)) {
    reject(queryReasonCodes.invalidFilter, '搜索片段过长或包含非法字符')
  }
  const limit = parseIntegerParameter(
    query.limit,
    queryReasonCodes.invalidLimit,
    `limit 必须是 1 到 ${queryLimits.maxPageLimit} 之间的整数`,
  )
  if (limit !== undefined && (limit < 1 || limit > queryLimits.maxPageLimit)) {
    reject(queryReasonCodes.invalidLimit, `limit 必须是 1 到 ${queryLimits.maxPageLimit} 之间的整数`)
  }
  // cursor 不 trim：它是 opaque token，前端既不解析也不"修正"它。
  if (query.cursor !== undefined && query.cursor.length > queryLimits.maxCursorLength) {
    reject(queryReasonCodes.invalidCursor, '游标非法或已过期，请重新从第一页开始')
  }
}

/** validateOverviewQuery 校验总览查询参数。顺序与 live handler 一致：environment 先于窗口。 */
export function validateOverviewQuery(query: OverviewQueryInput): void {
  validateEnvironment(query.environment)
  const windowSeconds = parseIntegerParameter(
    query.windowSeconds,
    queryReasonCodes.invalidWindow,
    'window_seconds 必须是正整数',
  )
  if (windowSeconds !== undefined && windowSeconds < 1) {
    reject(queryReasonCodes.invalidWindow, 'window_seconds 必须是正整数')
  }
}

// 严格 RFC3339，逐条对齐 Go 的 time.Parse(time.RFC3339, ...)（已在 go1.25 上实测）：
//
//   - 分隔符与 UTC 标记必须**大写**：`2026-08-22t02:04:22z` 被 Go 拒绝；
//   - 日期必须真实存在：`2026-02-30` 与 `2026-02-29`（非闰年）被拒绝，`2024-02-29` 接受；
//   - 时钟上界 23:59:59；`24:00:00`、`23:60:00`、`23:59:60` 全部拒绝；
//   - 小数秒可选但必须至少一位数字，分隔符可以是点或**逗号**：`.123` 与 `,123` 都被
//     Go 的通用解析路径接受，`.`/`,` 单独出现、`;123`、`,,1`、`,12,3` 全部拒绝；
//   - 偏移量必须是 `Z` 或 `±HH:MM`（带冒号）；`+0800` 拒绝。
//
// 刻意与 Go 一致的两个"意外接受"：`+25:00` 与 `+08:60` 只做数字形状校验，Go 也接受它们。
// 因此这里不能用 Date.parse 兜底——Date.parse 会拒绝这两个，同时又会把 `2026-02-30`
// 规范化成 3 月 2 日而错误通过。
const rfc3339Pattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})([.,]\d+)?(Z|[+-]\d{2}:\d{2})$/

function isLeapYear(year: number): boolean {
  return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0)
}

function daysInMonth(year: number, month: number): number {
  const lengths = [31, isLeapYear(year) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  return lengths[month - 1]
}

/**
 * isStrictRFC3339 判断一个字符串是否是 Go 的 RFC3339 会接受的绝对时间。
 *
 * 它是自洽的解析器，不调用 Date.parse：JS 的日期解析既过宽（`2026-02-30` 被规范化成
 * 3 月 2 日）又过窄（拒绝 `+25:00`），两个方向都会让 fixture 与 live 判定不一致。
 */
export function isStrictRFC3339(raw: string): boolean {
  const match = rfc3339Pattern.exec(raw)
  if (!match) return false
  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  const hour = Number(match[4])
  const minute = Number(match[5])
  const second = Number(match[6])
  if (month < 1 || month > 12) return false
  if (day < 1 || day > daysInMonth(year, month)) return false
  if (hour > 23 || minute > 59 || second > 59) return false
  return true
}

function assertAbsoluteRFC3339(raw: string): void {
  if (!isStrictRFC3339(raw)) {
    reject(queryReasonCodes.invalidTimeRange, 'from/to 必须是带时区的绝对 RFC3339 时间')
  }
}

/**
 * validateWorkspaceScope 校验工作区的**非 focus** 字段：时间窗配对、绝对时间与时区形状。
 *
 * 它与 focus 校验分开，是因为显式 QA 场景必须能在 URL 上还留着旧 focus 时也确定性地
 * 加载：场景视图自带权威 focus，用旧 focus 把它判成 422 等于让 QA 状态不可达。
 *
 * 时间窗上界依赖已发布 Profile 的 limits，属服务端职责，fixture 不假装拥有那份配置。
 * timezone 这里只校验形状/长度；"能否真的加载这个时区"属于 R2 的 C4，不在本模块放宽。
 *
 * environment 也属于非 focus 字段：显式 QA 场景可以忽略过期 focus，但不能连 environment
 * 形状一起放过。
 */
export function validateWorkspaceScope(query: WorkspaceQueryInput): void {
  validateEnvironment(query.environment)
  const from = scalar(query.from)
  const to = scalar(query.to)
  if ((from === '') !== (to === '')) {
    reject(queryReasonCodes.invalidTimeRange, 'from 与 to 必须同时提供')
  }
  if (from !== '') {
    assertAbsoluteRFC3339(from)
    assertAbsoluteRFC3339(to)
  }
  validateTimezone(query.timezone)
}

// timezonePattern 与后端 internal/api/service_observability.go 的同名正则一致。
//
// 之前这里只校验长度，注释却声称校验"形状"：于是 `Asia Shanghai` 这类输入在 fixture 下
// 通过、在 live 下 422。
const timezonePattern = /^[A-Za-z][A-Za-z0-9_+/-]{0,63}$/

/**
 * validateTimezone 校验时区并返回 trim 后的值。
 *
 * 两道校验与后端一致：先形状与长度，再"这个时区是否真的存在"。运行时能力用
 * `Intl.DateTimeFormat`——它对未知时区抛 RangeError，因此 `Mars/Olympus` 会被拒绝，
 * 而 `UTC`、`Asia/Shanghai` 正常通过。
 *
 * 生产权威仍然是 Go 的 `time.LoadLocation`（见后端 parseTimezone）：这里的 Intl 判定只是
 * 让显式 fixture 模式对同一批输入给出同样的 422，不是替代后端校验。两者的时区数据来源
 * 不同（ICU vs IANA tzdata），极冷门的区名理论上可能判定不一致；真正的裁决权在后端。
 */
export function validateTimezone(timezone: string | undefined): string {
  const value = scalar(timezone)
  if (value === '') return ''
  if (byteLength(value) > queryLimits.maxTimezoneBytes || !timezonePattern.test(value)) {
    reject(queryReasonCodes.invalidTimezone, 'timezone 格式非法')
  }
  if (!isLoadableTimezone(value)) {
    reject(queryReasonCodes.invalidTimezone, 'timezone 不是可用的 IANA 时区')
  }
  return value
}

/** isLoadableTimezone 判断运行时能否真的用这个时区格式化时间。 */
export function isLoadableTimezone(timezone: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: timezone }).format(0)
    return true
  } catch {
    // RangeError：运行时不认识这个时区。形状对不代表它存在。
    return false
  }
}

/**
 * validateWorkspaceFocus 校验 focus 配对与合法性。
 *
 * focusKinds 由调用方（fixture provider）从已发布 Profile 投影里取，因此本模块不认识
 * 任何具体服务：没有 focus kind 字面量，也没有服务专属 ID 分支。
 */
export function validateWorkspaceFocus(
  focusKinds: FocusKindContract[],
  query: WorkspaceQueryInput,
): void {
  const kind = scalar(query.focusKind)
  const value = scalar(query.focusValue)
  if ((kind === '') !== (value === '')) {
    reject(queryReasonCodes.invalidFocus, 'focus_kind 与 focus_value 必须同时提供')
  }
  if (kind === '') return
  if (byteLength(kind) > queryLimits.maxFocusKindBytes) {
    reject(queryReasonCodes.invalidFocus, 'focus_kind 格式非法')
  }
  if (byteLength(value) > queryLimits.maxFocusValueBytes || containsControlCharacters(value)) {
    reject(queryReasonCodes.invalidFocus, 'focus_value 过长或包含非法字符')
  }
  const declared = focusKinds.find((focus) => focus.key === kind)
  if (!declared || !new RegExp(declared.value_pattern).test(value)) {
    reject(queryReasonCodes.invalidFocus, 'focus 不满足该服务已发布 Profile 的定义')
  }
}

/**
 * validateWorkspaceQuery 校验完整工作区查询。
 *
 * 顺序刻意与 live handler 一致（environment -> focus -> 时间窗 -> timezone）：同一个
 * URL 上有多个非法参数时，两种模式必须报出同一个 reason code。
 */
export function validateWorkspaceQuery(
  focusKinds: FocusKindContract[],
  query: WorkspaceQueryInput,
): void {
  validateEnvironment(query.environment)
  validateWorkspaceFocus(focusKinds, query)
  validateWorkspaceScope(query)
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
