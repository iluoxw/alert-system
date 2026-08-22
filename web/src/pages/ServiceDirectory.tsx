import { useEffect, useMemo } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { Button, Empty, Input, Select } from 'antd'
import {
  AppstoreOutlined,
  ArrowDownOutlined,
  ArrowRightOutlined,
  ArrowUpOutlined,
  CloseCircleOutlined,
  DashboardOutlined,
  FilterOutlined,
  ReadOutlined,
  SettingOutlined,
  SwapOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import { DataStatusBadge, HealthBadge } from '../components/ServiceObservability'
import { getDirectoryFixture } from '../serviceObservability/fixtures'
import { formatClock } from '../serviceObservability/format'
import {
  directorySearchStorageKey,
  formatRelativeObservedAt,
  isDirectorySortKey,
  signalDisclosure,
  sortDirectoryItems,
} from '../serviceObservability/directory'
import type { DirectorySortKey, SortDirection } from '../serviceObservability/directory'
import type { DirectoryItem, SignalValue } from '../serviceObservability/types'
import '../serviceObservability/serviceObservability.css'

const directory = getDirectoryFixture()
const scrollStorageKey = 'firebreak.service-directory.scroll-y'

function compactSignal(signal: SignalValue): string {
  if (signal.availability !== 'available') return '未知'
  if (signal.semantic === 'saturation' && signal.measurement_role === 'proxy') {
    return `容量压力 proxy ${signal.display_value}`
  }
  return signal.display_value
}

function signalLabel(signal: SignalValue): string {
  if (signal.semantic === 'latency') return '延迟'
  if (signal.semantic === 'traffic') return '流量'
  if (signal.semantic === 'errors') return '错误'
  return '容量'
}

function itemMatches(item: DirectoryItem, searchParams: URLSearchParams): boolean {
  const query = searchParams.get('q')?.trim().toLowerCase() ?? ''
  const team = searchParams.get('team')
  const tier = searchParams.get('tier')
  const health = searchParams.get('health')
  const data = searchParams.get('data')
  const layer = searchParams.get('layer')
  const profile = searchParams.get('profile')
  const haystack = `${item.service.name} ${item.service.entity_key} ${item.service.description} ${item.service.owner_team_name}`.toLowerCase()
  return (!query || haystack.includes(query))
    && (!team || item.service.owner_team_id === team)
    && (!tier || item.service.tier === tier)
    && (!health || item.health.status === health)
    && (!data || item.data_status.state === data)
    && (!layer || item.service.layer === layer)
    && (!profile || (profile === 'published' ? item.profile?.status === 'published' : item.profile === null))
}

function SortHeader({
  label,
  sortKey,
  activeSort,
  direction,
  onSort,
}: {
  label: string
  sortKey: DirectorySortKey
  activeSort?: DirectorySortKey
  direction: SortDirection
  onSort: (key: DirectorySortKey) => void
}) {
  const active = activeSort === sortKey
  return (
    <button
      type="button"
      className={`service-sort-button${active ? ' is-active' : ''}`}
      onClick={() => onSort(sortKey)}
      aria-label={`${label}，${active ? (direction === 'asc' ? '当前升序' : '当前降序') : '点击排序'}`}
    >
      <span>{label}</span>
      {active ? direction === 'asc' ? <ArrowUpOutlined /> : <ArrowDownOutlined /> : <SwapOutlined />}
    </button>
  )
}

function ariaSort(activeSort: DirectorySortKey | undefined, key: DirectorySortKey, direction: SortDirection) {
  if (activeSort !== key) return 'none' as const
  return direction === 'asc' ? 'ascending' as const : 'descending' as const
}

function isRowSecondaryTarget(target: EventTarget | null): boolean {
  return target instanceof Element && Boolean(target.closest('a, button, input, [role="button"]'))
}

export function ServiceDirectory() {
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const sortParam = searchParams.get('sort')
  const activeSort = isDirectorySortKey(sortParam) ? sortParam : undefined
  const direction: SortDirection = searchParams.get('direction') === 'desc' ? 'desc' : 'asc'
  const items = useMemo(() => {
    const filtered = directory.items.filter((item) => itemMatches(item, searchParams))
    return sortDirectoryItems(filtered, activeSort, direction)
  }, [activeSort, direction, searchParams])
  const q = searchParams.get('q') ?? ''

  useEffect(() => {
    const saved = window.sessionStorage.getItem(scrollStorageKey)
    if (saved) window.requestAnimationFrame(() => window.scrollTo({ top: Number(saved) || 0 }))
    const remember = () => window.sessionStorage.setItem(scrollStorageKey, String(window.scrollY))
    window.addEventListener('scroll', remember, { passive: true })
    return () => window.removeEventListener('scroll', remember)
  }, [])

  useEffect(() => {
    try {
      window.sessionStorage.setItem(directorySearchStorageKey, searchParams.toString())
    } catch {
      // 浏览器禁用 sessionStorage 时，URL 仍是完整的可恢复事实源。
    }
  }, [searchParams])

  const updateFilter = (key: string, value?: string) => {
    const next = new URLSearchParams(searchParams)
    if (value) next.set(key, value)
    else next.delete(key)
    setSearchParams(next, { replace: true })
  }

  const clearFilters = () => {
    const next = new URLSearchParams(searchParams)
    for (const key of ['q', 'team', 'tier', 'health', 'data', 'layer', 'profile']) next.delete(key)
    setSearchParams(next, { replace: true })
  }

  const updateSort = (key: DirectorySortKey) => {
    const next = new URLSearchParams(searchParams)
    const defaultDirection: SortDirection = key === 'fault' || key === 'observed' ? 'desc' : 'asc'
    const nextDirection = activeSort === key ? (direction === 'asc' ? 'desc' : 'asc') : defaultDirection
    next.set('sort', key)
    next.set('direction', nextDirection)
    setSearchParams(next)
  }

  const teamOptions = Array.from(new Map(directory.items.map((item) => [
    item.service.owner_team_id,
    { value: item.service.owner_team_id, label: item.service.owner_team_name },
  ])).values())

  return (
    <main className="service-ops-page service-directory-v2">
      <header className="service-page-intro">
        <div>
          <span className="service-eyebrow">SERVICE OPERATIONS</span>
          <h2>服务目录</h2>
          <p>从服务责任边界进入健康、Fault、依赖与诊断。</p>
        </div>
        <time className="service-last-updated" dateTime={directory.assembled_at} title={`${directory.assembled_at} · UTC`} tabIndex={0}>
          目录更新于 {formatClock(directory.assembled_at)}
        </time>
      </header>

      <section className="service-summary-ledger" aria-label="服务目录摘要">
        <article>
          <AppstoreOutlined />
          <div><span>受管服务</span><strong>{directory.summary.managed}</strong><small>平台与应用服务</small></div>
        </article>
        <article>
          <CloseCircleOutlined />
          <div><span>Critical</span><strong>{directory.summary.critical}</strong><small>需要立即判断</small></div>
        </article>
        <article>
          <WarningOutlined />
          <div><span>Partial</span><strong>{directory.summary.partial}</strong><small>数据不完整</small></div>
        </article>
        <article>
          <SettingOutlined />
          <div><span>待配置</span><strong>{directory.summary.unconfigured}</strong><small>缺少 Profile 或 owner</small></div>
        </article>
      </section>

      <section className="service-ledger-surface">
        <div className="service-directory-toolbar">
          <Input.Search
            allowClear
            value={q}
            onChange={(event) => updateFilter('q', event.target.value)}
            placeholder="搜索服务或 entity key"
            aria-label="搜索服务"
          />
          <Select
            allowClear
            value={searchParams.get('team') || undefined}
            onChange={(value) => updateFilter('team', value)}
            placeholder="全部团队"
            options={teamOptions}
            aria-label="按团队筛选"
          />
          <Select
            allowClear
            value={searchParams.get('tier') || undefined}
            onChange={(value) => updateFilter('tier', value)}
            placeholder="全部 Tier"
            options={['tier-0', 'tier-1', 'tier-2', 'tier-3'].map((value) => ({ value, label: value.toUpperCase() }))}
            aria-label="按服务等级筛选"
          />
          <Select
            allowClear
            value={searchParams.get('health') || undefined}
            onChange={(value) => updateFilter('health', value)}
            placeholder="全部健康状态"
            options={['critical', 'degraded', 'unknown', 'healthy'].map((value) => ({ value, label: value[0].toUpperCase() + value.slice(1) }))}
            aria-label="按健康状态筛选"
          />
          <Select
            allowClear
            value={searchParams.get('data') || undefined}
            onChange={(value) => updateFilter('data', value)}
            placeholder="全部数据状态"
            options={['complete', 'partial', 'stale', 'unconfigured'].map((value) => ({ value, label: value[0].toUpperCase() + value.slice(1) }))}
            aria-label="按数据状态筛选"
          />
          <Select
            allowClear
            value={searchParams.get('layer') || undefined}
            onChange={(value) => updateFilter('layer', value)}
            placeholder="全部分层"
            options={[
              { value: 'platform', label: '平台层' },
              { value: 'infrastructure', label: '基础设施层' },
              { value: 'application', label: '应用层' },
            ]}
            aria-label="按服务分层筛选"
          />
          <Select
            allowClear
            value={searchParams.get('profile') || undefined}
            onChange={(value) => updateFilter('profile', value)}
            placeholder="全部 Profile"
            options={[
              { value: 'published', label: 'Profile 已发布' },
              { value: 'missing', label: 'Profile 未配置' },
            ]}
            aria-label="按 Profile 状态筛选"
          />
          <span className="service-directory-count">
            <b>E2B fixture set</b>{items.length} 条当前结果 / {directory.summary.managed} 个受管服务
          </span>
        </div>

        {items.length === 0 ? (
          <div className="service-directory-empty">
            <Empty description="没有匹配的服务" />
            <Button icon={<FilterOutlined />} onClick={clearFilters}>清除筛选</Button>
          </div>
        ) : (
          <div className="service-table-scroll">
            <table className="service-directory-table">
              <thead>
                <tr>
                  <th aria-sort={ariaSort(activeSort, 'service', direction)}><SortHeader label="服务" sortKey="service" activeSort={activeSort} direction={direction} onSort={updateSort} /></th>
                  <th aria-sort={ariaSort(activeSort, 'health', direction)}><SortHeader label="健康" sortKey="health" activeSort={activeSort} direction={direction} onSort={updateSort} /></th>
                  <th>黄金信号</th>
                  <th aria-sort={ariaSort(activeSort, 'fault', direction)}><SortHeader label="Fault / Impact" sortKey="fault" activeSort={activeSort} direction={direction} onSort={updateSort} /></th>
                  <th>
                    <span className="service-sort-group">
                      <SortHeader label="负责团队" sortKey="owner" activeSort={activeSort} direction={direction} onSort={updateSort} />
                      <SortHeader label="Tier" sortKey="tier" activeSort={activeSort} direction={direction} onSort={updateSort} />
                    </span>
                  </th>
                  <th>
                    <span className="service-sort-group">
                      <SortHeader label="数据" sortKey="data" activeSort={activeSort} direction={direction} onSort={updateSort} />
                      <SortHeader label="观测" sortKey="observed" activeSort={activeSort} direction={direction} onSort={updateSort} />
                    </span>
                  </th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => {
                  const serviceHref = `/services/${item.service.service_id}`
                  const registrationHref = item.links?.find((link) => link.kind === 'service_registration')?.href ?? '/settings/services'
                  const primaryHref = item.profile ? serviceHref : registrationHref
                  const secondaryLinks = item.links?.filter((link) => link.kind !== 'service_registration') ?? []
                  return (
                    <tr
                      key={item.service.service_id}
                      className={`is-${item.health.status}`}
                      tabIndex={0}
                      aria-label={`${item.profile ? '打开服务' : '配置 Profile'} ${item.service.name}`}
                      onClick={(event) => {
                        if (!isRowSecondaryTarget(event.target)) navigate(primaryHref)
                      }}
                      onKeyDown={(event) => {
                        if (event.target === event.currentTarget && event.key === 'Enter') navigate(primaryHref)
                      }}
                    >
                      <td>
                        <Link className="service-directory-identity" to={primaryHref}>
                          <strong>{item.service.name}</strong>
                          <span className="service-kind-chip">
                            {item.service.layer === 'platform' ? '平台' : item.service.layer === 'infrastructure' ? '基础设施' : '应用'}
                          </span>
                          <em className={`service-profile-chip${item.profile ? ' is-published' : ''}`}>
                            {item.profile ? `Profile v${item.profile.profile_version}` : 'Profile 未配置'}
                          </em>
                          <code>{item.service.entity_key}</code>
                          <small>{item.service.description}</small>
                        </Link>
                      </td>
                      <td><HealthBadge state={item.health.status} /></td>
                      <td>
                        <div className="service-signal-compact">
                          {item.golden_signals.length === 0 && (
                            <span className="is-unknown"><b>Golden</b>Unknown · 未配置 Profile</span>
                          )}
                          {item.golden_signals.map((signal) => (
                            <span key={signal.binding_key} className={`is-${signal.health}`} tabIndex={0} title={signalDisclosure(signal)}>
                              <b>{signalLabel(signal)}</b>
                              {compactSignal(signal)}
                            </span>
                          ))}
                        </div>
                      </td>
                      <td>
                        <Link className={item.domain_counts.active_faults > 0 ? 'service-count-critical' : ''} to={`/faults?service_id=${item.service.service_id}`}>
                          {item.domain_counts.active_faults} 个 Fault
                        </Link>
                        <small>{item.domain_counts.confirmed_impacts} confirmed Impact</small>
                      </td>
                      <td>
                        <button className="service-filter-link" onClick={() => updateFilter('team', item.service.owner_team_id)}>
                          {item.service.owner_team_name}
                        </button>
                        <span> · </span>
                        <button className="service-filter-link" onClick={() => updateFilter('tier', item.service.tier)}>
                          {item.service.tier.replace('tier-', 'T')}
                        </button>
                      </td>
                      <td>
                        <DataStatusBadge state={item.data_status.state} />
                        <small>
                          <time dateTime={item.observed_at} title={`${item.observed_at} · UTC`} tabIndex={0}>
                            {formatRelativeObservedAt(item.observed_at, directory.assembled_at)}
                          </time>
                          {' · '}{item.data_status.sources.length} 个来源
                        </small>
                      </td>
                      <td>
                        <div className="service-row-actions">
                          {secondaryLinks.map((link) => (
                            <a key={link.kind} href={link.href} aria-label={`${item.service.name} ${link.label}`}>
                              {link.kind === 'dashboard' ? <DashboardOutlined /> : <ReadOutlined />}{link.label}
                            </a>
                          ))}
                          <Link className="service-open-link" to={primaryHref}>
                            {item.profile ? '打开服务' : '配置 Profile'} <ArrowRightOutlined />
                          </Link>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <footer className="service-signal-legend">
        <strong>信号：</strong>
        <HealthBadge state="healthy" compact />
        <HealthBadge state="degraded" compact />
        <HealthBadge state="critical" compact />
        <HealthBadge state="unknown" compact />
        <span>Traffic/Rate 只显示值或基线变化，不单独判断好坏。</span>
      </footer>
    </main>
  )
}
