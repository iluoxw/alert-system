export function formatClock(value: string, timezone = 'Asia/Shanghai'): string {
  try {
    return new Intl.DateTimeFormat('zh-CN', {
      timeZone: timezone,
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    }).format(new Date(value))
  } catch {
    return value
  }
}

export function formatDateTime(value: string, timezone = 'Asia/Shanghai'): string {
  try {
    return new Intl.DateTimeFormat('zh-CN', {
      timeZone: timezone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
      timeZoneName: 'short',
    }).format(new Date(value))
  } catch {
    return value
  }
}

export function formatWindow(seconds: number): string {
  if (seconds < 60) return `${seconds} 秒`
  if (seconds < 3600) return `${Math.round(seconds / 60)} 分钟`
  if (seconds < 86400) return `${Math.round(seconds / 3600)} 小时`
  return `${Math.round(seconds / 86400)} 天`
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`
  return `${Math.round(seconds / 3600)}h`
}

const reasonLabels: Record<string, string> = {
  telemetry_bundle_timeout: '数据源查询超时',
  focus_no_results: '当前范围无匹配请求',
  placement_resource_exhausted: '资源不足，Placement 失败',
}

const sourceLabels: Record<string, string> = {
  alert_system: 'alert-system',
  alertmanager: 'Alertmanager',
  deployment_system: '部署系统',
  openobserve: 'OpenObserve',
}

export function formatReasonCode(value?: string): string {
  if (!value) return ''
  return reasonLabels[value] ?? value.replaceAll('_', ' ')
}

export function formatProvenance(value: string): string {
  if (value === 'declared_and_observed') return 'declared + observed'
  return value.replaceAll('_', ' ')
}

export function formatSourceName(value: string): string {
  return sourceLabels[value] ?? value.replaceAll('_', ' ')
}

export function humanizeIdentifier(value: string): string {
  return value
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => {
      const upper = part.toUpperCase()
      if (['ID', 'API', 'HTTP', 'URL', 'SDK', 'RED', 'USE'].includes(upper)) return upper
      if (part.length <= 3 && part === part.toUpperCase()) return part
      return part[0].toUpperCase() + part.slice(1)
    })
    .join(' ')
}
