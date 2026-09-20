import type { Fault, FaultDetailResponse, ResourceNode } from '../api/client'

export const faultStatusLabel: Record<Fault['status'], string> = {
  candidate: '候选',
  active: '故障中',
  monitoring: '观察中',
  resolved: '已解决',
}

export const severityOrder: Record<Fault['severity'], number> = {
  critical: 3,
  warning: 2,
  info: 1,
}

export function relativeTime(value?: string) {
  if (!value) return '尚未同步'
  const seconds = Math.max(0, Math.round((Date.now() - new Date(value).getTime()) / 1000))
  if (seconds < 60) return `${seconds} 秒前`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  return `${Math.floor(seconds / 86400)} 天前`
}

export function duration(value: string) {
  const minutes = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 60_000))
  if (minutes < 60) return `${minutes}m`
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h ${minutes % 60}m`
  return `${Math.floor(minutes / 1440)}d`
}

export function faultDisplayID(id: string) {
  return `FLT-${id.replaceAll('-', '').slice(0, 8).toUpperCase()}`
}

export function incidentDisplayID(number: number) {
  return `INC-${String(number).padStart(6, '0')}`
}

export function impactTotals(detail?: FaultDetailResponse) {
  return (detail?.impacts ?? []).reduce((totals, { impact }) => {
    totals[impact.state] += 1
    return totals
  }, { confirmed: 0, potential: 0, ruled_out: 0 })
}

export function ownerTeam(fault: Fault, nodes: Map<string, ResourceNode>, teams: Map<string, string>) {
  const teamID = fault.owner_team_id || (fault.resource_node_id ? nodes.get(fault.resource_node_id)?.owner_team_id : undefined)
  return teamID ? teams.get(teamID) || teamID : '未登记团队'
}

export function isFaultAcked(fault: Fault) {
  return Boolean(fault.acked_at)
}

export function formatIndicatorRate(value: number | null | undefined) {
  return value == null ? '暂无数据' : `${(value * 100).toFixed(1)}%`
}

export function formatIndicatorSeconds(value: number | null | undefined) {
  if (value == null) return '暂无数据'
  const minutes = Math.round(value / 60)
  if (minutes < 1) return `${Math.round(value)}秒`
  if (minutes < 60) return `${minutes}分钟`
  return `${(minutes / 60).toFixed(1)}小时`
}

export function formatIndicatorCount(value: number | null | undefined) {
  return value == null ? '暂无数据' : String(value)
}

export function ackLabel(fault: Fault) {
  return isFaultAcked(fault) ? (fault.acked_by_name || '已认领') : '未认领'
}
