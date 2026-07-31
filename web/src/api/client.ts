// API 客户端：类型定义严格对齐后端 JSON 契约。
//
// - 认证端点见 internal/api/server.go 的 handleLogin/handleLogout/handleMe（任务 25）；
// - 告警列表端点见 internal/api/alerts.go 的 list/newAlertResponse（任务 26）。
//
// 约定：所有请求都带 credentials: 'include'，登录态由后端下发的 HttpOnly
// session_token cookie 维持，前端不读取/存储 token 本身。任意受保护端点返回 401
// （{"error":"unauthenticated"}）时，通过 onUnauthorized 回调统一处理跳转登录页
// （见 setUnauthorizedHandler），调用方不需要每处自行判断 401。

export type Severity = 'critical' | 'warning' | 'info'
export type Status = 'firing' | 'acknowledged' | 'resolved'

// Alert 对应 internal/api/alerts.go 的 alertResponse（字段与 json tag 一一对应）。
// AckedAt/EndsAt 后端未设置时序列化为 null，对应 string | null。
export interface Alert {
  id: string
  fingerprint: string
  status: Status
  severity: Severity
  severity_fallback: boolean
  title: string
  source: string
  labels: Record<string, string>
  resource_ref: Record<string, string>
  signal_context?: unknown
  resolve_policy: string
  ttl_seconds?: number
  source_integration_id?: string
  dedup_count: number
  group_key?: string
  team_id?: string
  silenced_by?: string[]
  acked_by?: string
  acked_at: string | null
  starts_at: string
  last_seen_at: string
  ends_at: string | null
  created_at: string
  updated_at: string
}

export interface AlertListResponse {
  alerts: Alert[]
  total: number
  limit: number
  offset: number
}

// AlertFilter 对应 GET /api/v1/alerts 的查询参数；labels 是 AND 语义的键值对
// （后端用 label[k]=v 的形式解析，见 alerts.go list() 的 c.QueryMap("label")）。
export interface AlertFilter {
  severity?: Severity
  status?: Status
  teamId?: string
  labels?: Record<string, string>
  limit?: number
  offset?: number
}

export interface MeResponse {
  user_id: string
  username: string
  is_admin: boolean
}

export interface LoginResponse {
  username: string
}

// ApiError 携带 HTTP 状态码与后端 {"error": "..."} 里的文案，供调用方展示。
// details 对应少数端点（如 internal/api/shifts.go 的时段冲突）额外返回的
// {"details": [...]} 数组，多数端点不带该字段，调用方按需断言其形状。
export class ApiError extends Error {
  status: number
  details?: unknown

  constructor(status: number, message: string, details?: unknown) {
    super(message)
    this.status = status
    this.details = details
  }
}

// onUnauthorized 由 App 在挂载时注册一次（见 src/App.tsx 的 AuthGuard），用于把
// "会话失效"这一横切关注点从每个调用点里剥离出去；未注册时静默忽略。/auth/login
// 自身的 401（密码错误）不会触发这个回调，见 request() 里的路径判断。
let onUnauthorized: (() => void) | null = null

export function setUnauthorizedHandler(handler: (() => void) | null): void {
  onUnauthorized = handler
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    credentials: 'include',
    headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  })

  // /auth/login 本身的 401 是"用户名或密码错误"，不是会话失效，不走统一跳转逻辑
  // （否则登录页会在密码输错时把自己重定向到登录页，形成怪异的死循环）。
  if (res.status === 401 && path !== '/auth/login' && onUnauthorized) {
    onUnauthorized()
  }

  if (!res.ok) {
    let message = `请求失败（${res.status}）`
    let details: unknown
    try {
      const body = (await res.json()) as { error?: string; details?: unknown }
      if (body.error) message = body.error
      details = body.details
    } catch {
      // 响应体不是 JSON（如 204 或网络层错误页）时保留默认文案
    }
    throw new ApiError(res.status, message, details)
  }

  if (res.status === 204) {
    return undefined as T
  }
  return (await res.json()) as T
}

export function login(username: string, password: string): Promise<LoginResponse> {
  return request<LoginResponse>('/auth/login', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
}

export function logout(): Promise<void> {
  return request<void>('/auth/logout', { method: 'POST' })
}

export function me(): Promise<MeResponse> {
  return request<MeResponse>('/me')
}

export function listAlerts(filter: AlertFilter): Promise<AlertListResponse> {
  const params = new URLSearchParams()
  if (filter.severity) params.set('severity', filter.severity)
  if (filter.status) params.set('status', filter.status)
  if (filter.teamId) params.set('team_id', filter.teamId)
  if (filter.labels) {
    for (const [k, v] of Object.entries(filter.labels)) {
      if (k && v) params.set(`label[${k}]`, v)
    }
  }
  if (filter.limit !== undefined) params.set('limit', String(filter.limit))
  if (filter.offset !== undefined) params.set('offset', String(filter.offset))

  const qs = params.toString()
  return request<AlertListResponse>(`/alerts${qs ? `?${qs}` : ''}`)
}

// withQuery 把非 undefined/非空字符串的参数拼成查询串，供下面各 list 函数复用
// （与 listAlerts 手写 URLSearchParams 的效果一致，多资源场景下抽出来避免重复）。
function withQuery(params: Record<string, string | number | undefined>): string {
  const usp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') usp.set(k, String(v))
  }
  const qs = usp.toString()
  return qs ? `?${qs}` : ''
}

// ============================================================
// 目录（轻量，internal/api/directory.go，serviceability C4/R4）
// ============================================================
//
// 对接 GET /api/v1/teams、/api/v1/users：登录即可访问（无需 admin），响应字段
// 就 id + name/display_name 两个（后端硬字段白名单），供 useNameMaps 把告警
// 列表/详情里裸露的 team_id/acked_by 等 UUID 解析成可读名字。与下方 /admin/teams、
// /admin/users（TeamRequest/User 全量字段，仅 admin 可见）是两套不同用途的端点，
// 不要混用。

export interface TeamLite {
  id: string
  name: string
}

export interface UserLite {
  id: string
  display_name: string
}

export function listTeamsLite(limit?: number): Promise<{ teams: TeamLite[] }> {
  return request(`/teams${withQuery({ limit })}`)
}
export function listUsersLite(limit?: number): Promise<{ users: UserLite[] }> {
  return request(`/users${withQuery({ limit })}`)
}

// ============================================================
// 告警详情与处置（internal/api/alerts.go 的 detail/ack/resolve/handoff）
// ============================================================

// TimelineAction 对应 internal/domain/state.go 的 TimelineAction 枚举。
// notify 为历史动作（已废弃但历史数据仍存在，需兼容渲染）；
// notify_sent/notify_failed 是其替代，区分发送成功/失败（R3）。
export type TimelineAction =
  | 'ack'
  | 'resolve'
  | 'handoff'
  | 'notify'
  | 'escalate'
  | 'silence'
  | 'notify_sent'
  | 'notify_failed'

// TimelineEntry 对应 timelineEntryResponse（省略了 alert_id，由所在详情响应的
// alert.id 隐含）。
export interface TimelineEntry {
  actor: string
  action: TimelineAction
  detail?: Record<string, unknown>
  at: string
}

// ChangeEvent 对应 changeEventResponse（recent_changes 数组元素）。
export interface ChangeEvent {
  id: string
  kind: string
  resource_ref: Record<string, string>
  payload?: unknown
  occurred_at: string
}

// AlertDetailResponse 对应 GET /alerts/:id 的响应体。
export interface AlertDetailResponse {
  alert: Alert
  timeline: TimelineEntry[]
  recent_changes: ChangeEvent[]
}

// AlertActionResponse 对应 ack/resolve/handoff 三个处置端点的响应体（都只回传
// 更新后的 alert）。
export interface AlertActionResponse {
  alert: Alert
}

export function getAlertDetail(id: string): Promise<AlertDetailResponse> {
  return request<AlertDetailResponse>(`/alerts/${id}`)
}

// ackAlert/resolveAlert 是幂等操作：告警已处于目标状态时后端仍返回 200 和当前
// 状态，不会报错。
export function ackAlert(id: string): Promise<AlertActionResponse> {
  return request<AlertActionResponse>(`/alerts/${id}/ack`, { method: 'POST' })
}

export function resolveAlert(id: string): Promise<AlertActionResponse> {
  return request<AlertActionResponse>(`/alerts/${id}/resolve`, { method: 'POST' })
}

// handoffAlert 不是幂等操作：对已 resolved 的告警会返回 409（ApiError.status
// === 409），调用方应据此提示"告警当前状态不允许该操作"。
export function handoffAlert(id: string, target: string): Promise<AlertActionResponse> {
  return request<AlertActionResponse>(`/alerts/${id}/handoff`, {
    method: 'POST',
    body: JSON.stringify({ target }),
  })
}

// ============================================================
// 配置管理（internal/api/config.go + silences.go，全部 /admin 下，仅 admin 可见）
// ============================================================

// Matcher/MatcherSet 对应 internal/domain/matcher.go：AND 语义的标签匹配规则集，
// 被 routes/notification-policies/silences 三处复用。
export type MatchOp = '=' | '!=' | '=~' | '!~'

export interface Matcher {
  name: string
  op: MatchOp
  value: string
}

export type MatcherSet = Matcher[]

// ---- teams ----

export interface Team {
  id: string
  name: string
  created_at: string
  updated_at: string
}

export interface TeamRequest {
  name: string
}

export function listTeams(limit?: number): Promise<{ teams: Team[] }> {
  return request(`/admin/teams${withQuery({ limit })}`)
}
export function getTeam(id: string): Promise<Team> {
  return request(`/admin/teams/${id}`)
}
export function createTeam(body: TeamRequest): Promise<Team> {
  return request('/admin/teams', { method: 'POST', body: JSON.stringify(body) })
}
export function updateTeam(id: string, body: TeamRequest): Promise<void> {
  return request(`/admin/teams/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteTeam(id: string): Promise<void> {
  return request(`/admin/teams/${id}`, { method: 'DELETE' })
}

// ---- team members ----
//
// 对接 internal/api/members.go（团队成员管理四端点，R1 最后 admin 护栏）：全部
// admin only。移除/降级团队最后一名 admin 后端返回 422 中文提示；重复添加同一
// 成员返回 409；role 非法返回 422——统一走 ApiError.message 展示，不在前端重复
// 这些判断（自降级/自移除的二次确认是前端独有的 UX 职责，见 Teams.tsx）。

export interface Member {
  user_id: string
  username: string
  display_name: string
  role: 'admin' | 'member'
}

export interface AddMemberRequest {
  user_id: string
  role: 'admin' | 'member'
}

export function listTeamMembers(teamId: string): Promise<{ members: Member[] }> {
  return request(`/admin/teams/${teamId}/members`)
}
export function addTeamMember(teamId: string, body: AddMemberRequest): Promise<Member> {
  return request(`/admin/teams/${teamId}/members`, { method: 'POST', body: JSON.stringify(body) })
}
export function updateTeamMemberRole(teamId: string, userId: string, role: 'admin' | 'member'): Promise<Member> {
  return request(`/admin/teams/${teamId}/members/${userId}`, {
    method: 'PATCH',
    body: JSON.stringify({ role }),
  })
}
export function removeTeamMember(teamId: string, userId: string): Promise<void> {
  return request(`/admin/teams/${teamId}/members/${userId}`, { method: 'DELETE' })
}

// ---- team ops settings ----
//
// 对应 internal/api/ops_settings.go（任务 10，R2 配置入口）：日报/陈旧告警清扫/
// 值班播报三个周期任务的开关、时刻/间隔与共用通知渠道，按团队维度存取。GET 未
// 配置的团队也返回 200 + 默认值（不是 404），PUT 是全量替换语义——后端不补默认、
// 不接受部分字段，前端必须先 GET 拿完整值再整体提交。

export interface OpsChannelTarget {
  kind: string
  target: string
}

export interface OpsSettings {
  team_id: string
  timezone: string
  daily_report_enabled: boolean
  daily_report_time: string
  stale_sweep_enabled: boolean
  stale_sweep_interval_seconds: number
  oncall_broadcast_enabled: boolean
  oncall_broadcast_time: string
  channels: OpsChannelTarget[]
}

export type OpsSettingsRequest = Omit<OpsSettings, 'team_id'>

export function getOpsSettings(teamId: string): Promise<OpsSettings> {
  return request(`/teams/${teamId}/ops-settings`)
}
export function updateOpsSettings(teamId: string, body: OpsSettingsRequest): Promise<OpsSettings> {
  return request(`/admin/teams/${teamId}/ops-settings`, { method: 'PUT', body: JSON.stringify(body) })
}

// ---- users ----
//
// 创建接受明文 password（服务端 bcrypt 哈希后落库）；更新不支持改用户名/密码
// （store.UserRepo.Update 的签名只覆盖 display_name/email/enabled，重置密码是
// 后端本期未提供的能力，YAGNI）。响应体永不含密码相关字段。

export interface User {
  id: string
  username: string
  display_name: string
  email: string
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface CreateUserRequest {
  username: string
  password: string
  display_name: string
  email?: string
  enabled?: boolean
}

export interface UpdateUserRequest {
  display_name: string
  email?: string
  enabled: boolean
}

export function listUsers(limit?: number): Promise<{ users: User[] }> {
  return request(`/admin/users${withQuery({ limit })}`)
}
export function getUser(id: string): Promise<User> {
  return request(`/admin/users/${id}`)
}
export function createUser(body: CreateUserRequest): Promise<User> {
  return request('/admin/users', { method: 'POST', body: JSON.stringify(body) })
}
export function updateUser(id: string, body: UpdateUserRequest): Promise<void> {
  return request(`/admin/users/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteUser(id: string): Promise<void> {
  return request(`/admin/users/${id}`, { method: 'DELETE' })
}

// ---- routes ----
//
// priority 是全局唯一的整数（routes.priority 列 UNIQUE 约束，不是按 team 唯一），
// 冲突时后端返回 409。

export interface RouteConfig {
  id: string
  priority: number
  matchers: MatcherSet
  target_team_id: string
  group_by: string[]
  group_wait_seconds: number
  group_interval_seconds: number
  repeat_interval_seconds: number
  created_at: string
  updated_at: string
}

export interface RouteRequest {
  priority: number
  matchers: MatcherSet
  target_team_id: string
  group_by: string[]
  group_wait_seconds: number
  group_interval_seconds: number
  repeat_interval_seconds: number
}

export function listRoutes(limit?: number): Promise<{ routes: RouteConfig[] }> {
  return request(`/admin/routes${withQuery({ limit })}`)
}
export function getRoute(id: string): Promise<RouteConfig> {
  return request(`/admin/routes/${id}`)
}
export function createRoute(body: RouteRequest): Promise<RouteConfig> {
  return request('/admin/routes', { method: 'POST', body: JSON.stringify(body) })
}
export function updateRoute(id: string, body: RouteRequest): Promise<void> {
  return request(`/admin/routes/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteRoute(id: string): Promise<void> {
  return request(`/admin/routes/${id}`, { method: 'DELETE' })
}

// ---- schedules ----

export interface Schedule {
  id: string
  team_id: string
  name: string
  timezone: string
  created_at: string
  updated_at: string
}

export interface ScheduleRequest {
  team_id: string
  name: string
  timezone?: string
}

export function listSchedules(params?: { teamId?: string; limit?: number }): Promise<{ schedules: Schedule[] }> {
  return request(`/admin/schedules${withQuery({ team_id: params?.teamId, limit: params?.limit })}`)
}
export function getSchedule(id: string): Promise<Schedule> {
  return request(`/admin/schedules/${id}`)
}
export function createSchedule(body: ScheduleRequest): Promise<Schedule> {
  return request('/admin/schedules', { method: 'POST', body: JSON.stringify(body) })
}
export function updateSchedule(id: string, body: ScheduleRequest): Promise<void> {
  return request(`/admin/schedules/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteSchedule(id: string): Promise<void> {
  return request(`/admin/schedules/${id}`, { method: 'DELETE' })
}

// ---- shifts ----
//
// 对应 internal/api/shifts.go（任务 6，R2 班次管理 + 轮换生成）：schedule 下属
// 班次 CRUD + 基于 oncall.GenerateRotation 的批量生成。GET 不带 from/to 时后端
// 默认返回 now~now+90d 窗口。时段冲突返回 422，冲突明细走 ApiError.details（形状
// 即 ShiftConflictDetail[]），不是普通 error 文案，调用方需专门处理 status===422
// 且 details 非空的情况（见 Schedules.tsx conflictLines）。
//
// id 设为可选：generate 端点 dry_run=true 时返回的预览条目没有 id（尚未落库），
// 与落库后的响应共用同一个 Shift 类型，使预览表格与正式列表能复用同一套渲染
// （硬约束：预览与落库复用同一渲染组件）。

export interface Shift {
  id?: string
  user_id: string
  user_display_name: string
  starts_at: string
  ends_at: string
}

export interface ShiftRequest {
  user_id: string
  starts_at: string
  ends_at: string
}

export interface ShiftConflictDetail {
  shift_id: string
  user_display_name: string
  starts_at: string
  ends_at: string
}

export interface GenerateShiftsRequest {
  user_ids: string[]
  shift_duration_seconds: number
  starts_at: string
  rounds: number
  dry_run: boolean
}

export function listShifts(scheduleId: string, params?: { from?: string; to?: string }): Promise<{ shifts: Shift[] }> {
  return request(`/admin/schedules/${scheduleId}/shifts${withQuery({ from: params?.from, to: params?.to })}`)
}
export function createShift(scheduleId: string, body: ShiftRequest): Promise<Shift> {
  return request(`/admin/schedules/${scheduleId}/shifts`, { method: 'POST', body: JSON.stringify(body) })
}
export function updateShift(scheduleId: string, shiftId: string, body: ShiftRequest): Promise<Shift> {
  return request(`/admin/schedules/${scheduleId}/shifts/${shiftId}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteShift(scheduleId: string, shiftId: string): Promise<void> {
  return request(`/admin/schedules/${scheduleId}/shifts/${shiftId}`, { method: 'DELETE' })
}
export function generateShifts(scheduleId: string, body: GenerateShiftsRequest): Promise<{ shifts: Shift[] }> {
  return request(`/admin/schedules/${scheduleId}/shifts/generate`, { method: 'POST', body: JSON.stringify(body) })
}

// ---- escalation policies ----
//
// levels 是升级链：每一级有等待时长与责任团队集合，targets 里的每个 ID 都是团队
// ID（任务 23 已裁决的契约），校验失败时后端会指出具体是哪一级的哪个 target。

export interface EscalationLevel {
  wait_seconds: number
  targets: string[]
}

// SeverityConfig 对应 store.SeverityConfig（ops-enhancements 任务 4/11，R1 覆写口径）：
// overrides 按 severity 覆写升级链各级等待秒数（可短于 levels，缺位回退默认），
// skip 里的 severity 不参与即时升级。后端 omitempty，未配置时序列化为 {}，两个
// 子字段都可能是 undefined（零值兼容），不传 severity_config 字段 = 全默认。
export interface SeverityConfig {
  overrides?: Partial<Record<Severity, number[]>>
  skip?: Severity[]
}

export interface EscalationPolicy {
  id: string
  team_id: string
  levels: EscalationLevel[]
  severity_config: SeverityConfig
  created_at: string
  updated_at: string
}

export interface EscalationPolicyRequest {
  team_id: string
  levels: EscalationLevel[]
  severity_config?: SeverityConfig
}

export function listEscalationPolicies(params?: {
  teamId?: string
  limit?: number
}): Promise<{ escalation_policies: EscalationPolicy[] }> {
  return request(`/admin/escalation-policies${withQuery({ team_id: params?.teamId, limit: params?.limit })}`)
}
export function getEscalationPolicy(id: string): Promise<EscalationPolicy> {
  return request(`/admin/escalation-policies/${id}`)
}
export function createEscalationPolicy(body: EscalationPolicyRequest): Promise<EscalationPolicy> {
  return request('/admin/escalation-policies', { method: 'POST', body: JSON.stringify(body) })
}
export function updateEscalationPolicy(id: string, body: EscalationPolicyRequest): Promise<void> {
  return request(`/admin/escalation-policies/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteEscalationPolicy(id: string): Promise<void> {
  return request(`/admin/escalation-policies/${id}`, { method: 'DELETE' })
}

// ---- notification policies ----
//
// steps 按 severity 分层（R13.1），每一级引用的 channel kind 必须已在渠道注册表
// 注册，否则后端返回 422 并指出缺失的 kind（R13.6）。team_id 为空=团队默认，
// team_id 与 matchers 都为空=全局默认（不同于 routes/silences，这里允许空
// matchers，见 config.go 顶部注释）。

export interface NotificationStep {
  wait_seconds: number
  channels: string[]
  mode: 'replace' | 'spread'
}

export type NotificationSteps = Partial<Record<Severity, NotificationStep[]>>

export interface NotificationPolicy {
  id: string
  name: string
  matchers: MatcherSet
  team_id: string
  steps: NotificationSteps
  created_at: string
  updated_at: string
}

export interface NotificationPolicyRequest {
  name: string
  matchers: MatcherSet
  team_id?: string
  steps: NotificationSteps
}

export function listNotificationPolicies(params?: {
  teamId?: string
  limit?: number
}): Promise<{ notification_policies: NotificationPolicy[] }> {
  return request(`/admin/notification-policies${withQuery({ team_id: params?.teamId, limit: params?.limit })}`)
}
export function getNotificationPolicy(id: string): Promise<NotificationPolicy> {
  return request(`/admin/notification-policies/${id}`)
}
export function createNotificationPolicy(body: NotificationPolicyRequest): Promise<NotificationPolicy> {
  return request('/admin/notification-policies', { method: 'POST', body: JSON.stringify(body) })
}
export function updateNotificationPolicy(id: string, body: NotificationPolicyRequest): Promise<void> {
  return request(`/admin/notification-policies/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteNotificationPolicy(id: string): Promise<void> {
  return request(`/admin/notification-policies/${id}`, { method: 'DELETE' })
}

// ---- integrations ----
//
// R1.3：明文 token 只在创建成功的响应里出现一次（IntegrationCreateResponse.token），
// 列表/详情/更新的响应体都不含 token 字段，前端拿不到就不应该假装能再展示一次。
// 更新请求的 enabled/default_severity 是必填的（后端 updateIntegrationRequest
// 用不带 omitempty 语义的具体类型，缺失会被绑定成零值——即"忘记传 enabled"会被
// 服务端理解成"禁用"），所以这里创建/更新共用同一个必填字段的请求类型，调用方
// 表单必须始终提供当前值。

export interface Integration {
  id: string
  kind: string
  name: string
  default_severity: Severity
  enabled: boolean
  config: unknown
  created_at: string
  updated_at: string
}

export interface IntegrationCreateResponse extends Integration {
  token: string // 明文，仅此一次
}

export interface IntegrationRequest {
  kind: string
  name: string
  default_severity: Severity
  enabled: boolean
  config?: unknown
}

export function listIntegrations(limit?: number): Promise<{ integrations: Integration[] }> {
  return request(`/admin/integrations${withQuery({ limit })}`)
}
export function getIntegration(id: string): Promise<Integration> {
  return request(`/admin/integrations/${id}`)
}
export function createIntegration(body: IntegrationRequest): Promise<IntegrationCreateResponse> {
  return request('/admin/integrations', { method: 'POST', body: JSON.stringify(body) })
}
export function updateIntegration(id: string, body: IntegrationRequest): Promise<void> {
  return request(`/admin/integrations/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteIntegration(id: string): Promise<void> {
  return request(`/admin/integrations/${id}`, { method: 'DELETE' })
}

// ---- silences ----
//
// R10.5：删除静默会在同一事务内清理已静默告警的引用，立即恢复通知。创建/更新
// 校验 ends_at>starts_at 且 matchers 非空（422），与 routes/notification-policies
// 允许空 matchers 的语义不同——留白会静默一切告警，风险太高，后端专门拒绝。

export interface Silence {
  id: string
  matchers: MatcherSet
  starts_at: string
  ends_at: string
  created_by: string
  comment: string
  created_at: string
  updated_at: string
}

export interface SilenceRequest {
  matchers: MatcherSet
  starts_at: string
  ends_at: string
  comment?: string
}

export function listSilences(limit?: number): Promise<{ silences: Silence[] }> {
  return request(`/admin/silences${withQuery({ limit })}`)
}
export function getSilence(id: string): Promise<Silence> {
  return request(`/admin/silences/${id}`)
}
export function createSilence(body: SilenceRequest): Promise<Silence> {
  return request('/admin/silences', { method: 'POST', body: JSON.stringify(body) })
}
export function updateSilence(id: string, body: SilenceRequest): Promise<void> {
  return request(`/admin/silences/${id}`, { method: 'PATCH', body: JSON.stringify(body) })
}
export function deleteSilence(id: string): Promise<void> {
  return request(`/admin/silences/${id}`, { method: 'DELETE' })
}
