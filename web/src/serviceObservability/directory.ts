import type { DataState, DirectoryItem, HealthState, SignalValue } from './types'

export type DirectorySortKey = 'service' | 'owner' | 'tier' | 'health' | 'fault' | 'data' | 'observed'
export type SortDirection = 'asc' | 'desc'

export const directorySearchStorageKey = 'firebreak.service-directory.search'

const collator = new Intl.Collator('zh-CN', { numeric: true, sensitivity: 'base' })
const healthPriority: Record<Exclude<HealthState, 'neutral'>, number> = {
  critical: 0,
  degraded: 1,
  unknown: 2,
  healthy: 3,
}
const dataPriority: Record<DataState, number> = {
  complete: 0,
  partial: 1,
  stale: 2,
  unconfigured: 3,
}

function tierPriority(tier: string): number {
  const match = tier.match(/(\d+)$/)
  return match ? Number(match[1]) : Number.MAX_SAFE_INTEGER
}

function compareNumber(left: number, right: number): number {
  return left === right ? 0 : left < right ? -1 : 1
}

export function compareDirectoryTriage(left: DirectoryItem, right: DirectoryItem): number {
  return compareNumber(healthPriority[left.health.status], healthPriority[right.health.status])
    || compareNumber(right.domain_counts.confirmed_impacts, left.domain_counts.confirmed_impacts)
    || compareNumber(right.domain_counts.active_faults, left.domain_counts.active_faults)
    || compareNumber(tierPriority(left.service.tier), tierPriority(right.service.tier))
    || collator.compare(left.service.name, right.service.name)
}

function compareByKey(left: DirectoryItem, right: DirectoryItem, key: DirectorySortKey): number {
  switch (key) {
    case 'service':
      return collator.compare(left.service.name, right.service.name)
    case 'owner':
      return collator.compare(left.service.owner_team_name, right.service.owner_team_name)
        || collator.compare(left.service.name, right.service.name)
    case 'tier':
      return compareNumber(tierPriority(left.service.tier), tierPriority(right.service.tier))
    case 'health':
      return compareNumber(healthPriority[left.health.status], healthPriority[right.health.status])
    case 'fault':
      return compareNumber(left.domain_counts.active_faults, right.domain_counts.active_faults)
        || compareNumber(left.domain_counts.confirmed_impacts, right.domain_counts.confirmed_impacts)
    case 'data':
      return compareNumber(dataPriority[left.data_status.state], dataPriority[right.data_status.state])
    case 'observed':
      return compareNumber(Date.parse(left.observed_at), Date.parse(right.observed_at))
  }
}

export function sortDirectoryItems(
  items: DirectoryItem[],
  key?: DirectorySortKey,
  direction: SortDirection = 'asc',
): DirectoryItem[] {
  const sorted = [...items]
  if (!key) return sorted.sort(compareDirectoryTriage)
  const directionFactor = direction === 'asc' ? 1 : -1
  return sorted.sort((left, right) => (
    compareByKey(left, right, key) * directionFactor
    || collator.compare(left.service.name, right.service.name)
  ))
}

export function isDirectorySortKey(value: string | null): value is DirectorySortKey {
  return value !== null && ['service', 'owner', 'tier', 'health', 'fault', 'data', 'observed'].includes(value)
}

export function formatRelativeObservedAt(observedAt: string, assembledAt: string): string {
  const seconds = Math.max(0, Math.round((Date.parse(assembledAt) - Date.parse(observedAt)) / 1000))
  if (!Number.isFinite(seconds)) return '时间未知'
  if (seconds < 60) return `${seconds} 秒前`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.floor(minutes / 60)
  return `${hours} 小时前`
}

export function signalDisclosure(signal: SignalValue): string {
  const windowMinutes = Math.max(1, Math.round(signal.window_seconds / 60))
  const observed = signal.observed_at ? `；观测时间 ${signal.observed_at}` : ''
  const threshold = signal.threshold_source ? '；阈值来自已发布 Service Profile' : ''
  const proxy = signal.measurement_role === 'proxy' && signal.proxy_reason ? `；${signal.proxy_reason}` : ''
  return `${signal.label}；窗口 ${windowMinutes} 分钟${threshold}${observed}${proxy}`
}

export function serviceDirectoryReturnHref(): string {
  if (typeof window === 'undefined') return '/services'
  try {
    const search = window.sessionStorage.getItem(directorySearchStorageKey)
    return search ? `/services?${search}` : '/services'
  } catch {
    return '/services'
  }
}
