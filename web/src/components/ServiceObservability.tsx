import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import {
  CheckCircleOutlined,
  ClockCircleOutlined,
  CloseCircleOutlined,
  InfoCircleOutlined,
  MinusCircleOutlined,
  QuestionCircleOutlined,
  RightOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import type {
  AvailabilityState,
  DataState,
  FreshnessState,
  HealthState,
  ServiceIdentity,
  SignalValue,
} from '../serviceObservability/types'
import { formatDateTime, formatReasonCode, formatWindow } from '../serviceObservability/format'
import { serviceDirectoryReturnHref } from '../serviceObservability/directory'

const healthLabels: Record<HealthState, string> = {
  healthy: 'Healthy',
  degraded: 'Degraded',
  critical: 'Critical',
  unknown: 'Unknown',
  neutral: 'Neutral',
}

const dataLabels: Record<DataState, string> = {
  complete: 'Complete',
  partial: 'Partial',
  stale: 'Stale',
  unconfigured: 'Unconfigured',
}

const availabilityLabels: Record<AvailabilityState, string> = {
  available: 'Available',
  empty: 'Empty',
  unknown: 'Unknown',
  forbidden: 'Forbidden',
  unavailable: 'Unavailable',
  expired: 'Expired',
}

function stateIcon(state: HealthState | DataState | AvailabilityState | FreshnessState): ReactNode {
  switch (state) {
    case 'healthy':
    case 'complete':
    case 'available':
    case 'fresh':
      return <CheckCircleOutlined />
    case 'critical':
      return <CloseCircleOutlined />
    case 'degraded':
    case 'partial':
      return <WarningOutlined />
    case 'stale':
      return <ClockCircleOutlined />
    case 'empty':
      return <MinusCircleOutlined />
    case 'neutral':
      return <InfoCircleOutlined />
    default:
      return <QuestionCircleOutlined />
  }
}

export function HealthBadge({ state, compact = false }: { state: HealthState; compact?: boolean }) {
  return (
    <span className={`service-state service-state--${state}${compact ? ' is-compact' : ''}`}>
      {stateIcon(state)}
      <span>{healthLabels[state]}</span>
    </span>
  )
}

export function DataStatusBadge({ state, compact = false }: { state: DataState; compact?: boolean }) {
  return (
    <span className={`service-state service-state--${state}${compact ? ' is-compact' : ''}`}>
      {stateIcon(state)}
      <span>{dataLabels[state]}</span>
    </span>
  )
}

export function AvailabilityBadge({ state }: { state: AvailabilityState }) {
  return (
    <span className={`service-state service-state--${state} is-compact`}>
      {stateIcon(state)}
      <span>{availabilityLabels[state]}</span>
    </span>
  )
}

export function ServiceBreadcrumb({ service, workspace = false }: { service?: ServiceIdentity; workspace?: boolean }) {
  return (
    <nav className="service-breadcrumb" aria-label="面包屑">
      <Link to={serviceDirectoryReturnHref()}>服务</Link>
      {service && <RightOutlined aria-hidden />}
      {service && (workspace ? <Link to={`/services/${service.service_id}`}>{service.name}</Link> : <span>{service.name}</span>)}
      {workspace && <RightOutlined aria-hidden />}
      {workspace && <span>可观测性</span>}
    </nav>
  )
}

function signalTitle(signal: SignalValue): string {
  if (signal.semantic === 'saturation' && signal.measurement_role === 'proxy') return '最坏容量压力 proxy'
  return signal.label
}

function signalMeta(signal: SignalValue): string {
  if (signal.availability !== 'available') return formatReasonCode(signal.unknown_reason) || '当前观测不可用'
  const parts = [`${formatWindow(signal.window_seconds)}窗口`, signal.aggregation.toUpperCase()]
  if (signal.baseline_delta_percent !== undefined) parts.push('对比基线')
  if (signal.threshold_source) parts.push('Service Profile 阈值')
  if (signal.measurement_role === 'proxy') parts.push('容量压力 proxy')
  if (signal.semantic === 'utilization') parts.push('非真实 Saturation')
  return parts.join(' · ')
}

function signalDelta(signal: SignalValue): string | null {
  if (signal.availability !== 'available') return null
  if (signal.baseline_delta_percent !== undefined) {
    return `较基线 ${signal.baseline_delta_percent >= 0 ? '↑' : '↓'} ${Math.abs(signal.baseline_delta_percent)}%`
  }
  return null
}

export function GoldenLedger({ signals }: { signals: SignalValue[] }) {
  return (
    <section className="golden-ledger" aria-label="Google 四大黄金信号">
      {signals.map((signal) => {
        const unavailable = signal.availability !== 'available'
        const delta = signalDelta(signal)
        return (
          <article
            key={signal.binding_key}
            className={`golden-ledger__item is-${signal.health}${unavailable ? ' is-unavailable' : ''}`}
            tabIndex={0}
            title={signal.observed_at ? `${formatDateTime(signal.observed_at)}${signal.proxy_reason ? `；${signal.proxy_reason}` : ''}` : signal.unknown_reason}
          >
            <div className="golden-ledger__label">
              <span>{signalTitle(signal)}</span>
              {signal.measurement_role === 'proxy' && <span className="signal-proxy-label">PROXY</span>}
            </div>
            <div className="golden-ledger__reading">
              <strong>{signal.display_value}</strong>
              {delta && <span>{delta}</span>}
              <HealthBadge state={signal.health} compact />
            </div>
            <small>{signalMeta(signal)}</small>
            {signal.proxy_reason && <small className="golden-ledger__disclosure">{signal.proxy_reason}</small>}
          </article>
        )
      })}
    </section>
  )
}

export function SectionHeading({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <header className="service-section-heading">
      <h3>{title}</h3>
      {action}
    </header>
  )
}
