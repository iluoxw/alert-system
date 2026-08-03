import { CheckCircleOutlined, ClockCircleOutlined, ExclamationCircleOutlined, InfoCircleOutlined } from '@ant-design/icons'
import type { ResponseState, Severity, SourceState, Status, SuppressionState } from '../api/client'

const severityLabel: Record<Severity, string> = {
  critical: '严重',
  warning: '警告',
  info: '提示',
}

const statusLabel: Record<Status, string> = {
  firing: '未处理',
  acknowledged: '已认领',
  resolved: '已恢复',
}

export function SeverityBadge({ severity }: { severity: Severity }) {
  const Icon = severity === 'critical'
    ? ExclamationCircleOutlined
    : severity === 'warning'
      ? ClockCircleOutlined
      : InfoCircleOutlined

  return (
    <span className={`alert-badge alert-badge--severity-${severity}`}>
      <Icon aria-hidden="true" />
      {severityLabel[severity]}
    </span>
  )
}

export function StatusBadge({ status }: { status: Status }) {
  const Icon = status === 'resolved' ? CheckCircleOutlined : ClockCircleOutlined

  return (
    <span className={`alert-badge alert-badge--status-${status}`}>
      <Icon aria-hidden="true" />
      {statusLabel[status]}
    </span>
  )
}

export function QuietBadge({ label }: { label: '已静默' | '抖动中' }) {
  return <span className={`alert-badge alert-badge--quiet alert-badge--${label === '已静默' ? 'silenced' : 'flapping'}`}>{label}</span>
}

const sourceStateLabel: Record<SourceState, string> = {
  firing: '来源触发中',
  resolved: '来源已恢复',
  unknown: '来源待确认',
}

const responseStateLabel: Record<ResponseState, string> = {
  unacknowledged: '未认领',
  acknowledged: '已认领',
}

const suppressionStateLabel: Record<SuppressionState, string> = {
  none: '未抑制',
  silenced: '已静默',
  inhibited: '已抑制',
}

export function SourceStateBadge({ state }: { state: SourceState }) {
  const Icon = state === 'resolved' ? CheckCircleOutlined : state === 'unknown' ? ExclamationCircleOutlined : ClockCircleOutlined
  return <span className={`alert-badge alert-badge--source-${state}`}><Icon aria-hidden="true" />{sourceStateLabel[state]}</span>
}

export function ResponseStateBadge({ state }: { state: ResponseState }) {
  return <span className={`alert-badge alert-badge--response-${state}`}><ClockCircleOutlined aria-hidden="true" />{responseStateLabel[state]}</span>
}

export function SuppressionStateBadge({ state }: { state: SuppressionState }) {
  return <span className={`alert-badge alert-badge--suppression-${state}`}>{suppressionStateLabel[state]}</span>
}
