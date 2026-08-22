import { Fragment, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Button, Input, Result, Select } from 'antd'
import {
  ApiOutlined,
  AppstoreOutlined,
  ArrowRightOutlined,
  CloseCircleOutlined,
  CodeSandboxOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  ReadOutlined,
  SafetyCertificateOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import {
  DataStatusBadge,
  GoldenLedger,
  HealthBadge,
  SectionHeading,
  ServiceBreadcrumb,
} from '../components/ServiceObservability'
import { getBaseWorkspaceFixture, getOverviewFixture } from '../serviceObservability/fixtures'
import {
  formatClock,
  formatDateTime,
  formatDuration,
  formatProvenance,
  formatSourceName,
  formatWindow,
} from '../serviceObservability/format'

const dependencyIcon = (layer: string) => layer === 'infrastructure' ? <DatabaseOutlined /> : <ApiOutlined />
const serviceIcon = (kind: string) => kind === 'infrastructure_capability' ? <CodeSandboxOutlined /> : <AppstoreOutlined />

export function ServiceOverview() {
  const { serviceId = '' } = useParams()
  const navigate = useNavigate()
  const view = useMemo(() => getOverviewFixture(serviceId), [serviceId])
  const workspace = useMemo(() => getBaseWorkspaceFixture(serviceId), [serviceId])
  const primaryFocus = view?.focus_kinds.find((focus) => focus.primary) ?? view?.focus_kinds[0]
  const [focusKind, setFocusKind] = useState(primaryFocus?.key ?? '')
  const [focusValue, setFocusValue] = useState(workspace?.focus.value ?? primaryFocus?.placeholder ?? '')

  if (!view || !workspace) {
    return <Result status="404" title="服务不存在" subTitle="当前 Phase 1 fixture 中没有该服务。" extra={<Button onClick={() => navigate('/services')}>返回服务目录</Button>} />
  }

  const focusDefinition = view.focus_kinds.find((focus) => focus.key === focusKind)
  const focusValid = Boolean(focusDefinition && new RegExp(focusDefinition.value_pattern).test(focusValue.trim()))
  const dependencyGroups = Array.from(view.dependencies.reduce((groups, dependency) => {
    const group = groups.get(dependency.group_key) ?? { key: dependency.group_key, label: dependency.group_label, items: [] }
    group.items.push(dependency)
    groups.set(dependency.group_key, group)
    return groups
  }, new Map<string, { key: string; label: string; items: typeof view.dependencies }>()).values())
  const workspaceWindowSeconds = Math.max(0, Math.round((Date.parse(workspace.scope.to) - Date.parse(workspace.scope.from)) / 1000))
  const workspaceWindowLabel = `${formatWindow(workspaceWindowSeconds)} · ${formatClock(workspace.scope.from, workspace.scope.timezone)}–${formatClock(workspace.scope.to, workspace.scope.timezone)}`

  const openWorkspace = () => {
    if (!focusValid) return
    const params = new URLSearchParams({
      focus_kind: focusKind,
      focus_value: focusValue.trim(),
      from: workspace.scope.from,
      to: workspace.scope.to,
    })
    if (focusValue.trim() !== workspace.focus.value) params.set('scenario', 'empty')
    navigate(`/services/${view.service.service_id}/observability?${params}`)
  }

  return (
    <main className="service-ops-page service-overview-page">
      <ServiceBreadcrumb service={view.service} />

      <header className="service-overview-hero">
        <div className="service-overview-identity">
          <span className="service-overview-icon" aria-hidden="true">{serviceIcon(view.service.kind)}</span>
          <div>
            <div className="service-overview-title-row">
              <h2>{view.service.name}</h2>
              <HealthBadge state={view.health.status} />
            </div>
            <p>{view.service.description}</p>
            <div className="service-overview-meta">
              <span>{view.service.layer}</span>
              <code>{view.service.entity_key}</code>
              <span>负责人 <b>{view.service.owner_team_name}</b></span>
              <span>层级 <b>{view.service.tier.replace('tier-', 'T')}</b></span>
              <span>Profile <b>v{view.profile?.profile_version ?? '—'}</b></span>
            </div>
          </div>
        </div>
        <div className="service-overview-actions">
          <time dateTime={view.assembled_at} title={formatDateTime(view.assembled_at, workspace.scope.timezone)}>
            服务上下文更新于 {formatClock(view.assembled_at, workspace.scope.timezone)}
          </time>
          <Button type="primary" onClick={() => navigate(`/services/${view.service.service_id}/observability`)}>
            进入可观测性工作区
          </Button>
          {view.links.map((link) => <a key={link.kind} href={link.href}>{link.label}</a>)}
        </div>
      </header>

      <GoldenLedger signals={view.golden_signals} />

      <div className="service-overview-layout">
        <div className="service-overview-main">
          <section className="service-panel">
            <SectionHeading title="当前需要判断" action={<Link to="/faults">查看全部 Fault <ArrowRightOutlined /></Link>} />
            <div className="service-fault-list">
              {view.faults.map((fault) => (
                <Link key={fault.fault_id} to={`/faults/${fault.fault_id}`} className={`service-fault-row is-${fault.severity}`}>
                  <span className="service-fault-mark">{fault.severity === 'critical' ? <CloseCircleOutlined /> : <WarningOutlined />}</span>
                  <strong>{fault.fault_id}</strong>
                  <span>{fault.title}</span>
                  <em>{fault.state === 'active' ? '故障中' : '候选'}</em>
                  <small>{fault.severity}</small>
                  <small>{formatDuration(fault.duration_seconds)}</small>
                  <small>{fault.owner_team_name}</small>
                  <small>{fault.confirmed_impact_count} confirmed · {fault.potential_impact_count} potential</small>
                  <ArrowRightOutlined />
                </Link>
              ))}
            </div>
          </section>

          <section className="service-panel">
            <SectionHeading title="关键依赖" />
            <div className="service-table-scroll">
              <table className="service-dependency-table">
                <thead><tr><th>分组</th><th>组件</th><th>职责</th><th>关系来源</th><th>健康状态</th><th>最近观测</th><th aria-label="操作" /></tr></thead>
                <tbody>
                  {dependencyGroups.map((group) => (
                    <Fragment key={group.key}>
                      {group.items.map((dependency, index) => (
                        <tr key={dependency.entity_id}>
                          {index === 0 && <td className="service-dependency-group-cell" rowSpan={group.items.length}><strong>{group.label}</strong><span>{group.items.length} 项</span></td>}
                          <td>{dependencyIcon(dependency.layer)} <Link to={`/infrastructure/${dependency.entity_id}`}>{dependency.name}</Link></td>
                          <td>{dependency.role}</td>
                          <td>{formatProvenance(dependency.provenance)}</td>
                          <td><HealthBadge state={dependency.health} compact /></td>
                          <td>
                            <time
                              dateTime={dependency.observed_at}
                              title={formatDateTime(dependency.observed_at, workspace.scope.timezone)}
                              tabIndex={0}
                            >
                              {formatClock(dependency.observed_at, workspace.scope.timezone)}
                            </time>
                          </td>
                          <td><Link aria-label={`打开 ${dependency.name}`} to={`/infrastructure/${dependency.entity_id}`}><ArrowRightOutlined /></Link></td>
                        </tr>
                      ))}
                    </Fragment>
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          <section className="service-panel service-recent-events">
            <SectionHeading title="最近事件" />
            {view.recent_events.map((event) => (
              <div key={event.event_id} className={`service-event-row is-${event.kind}`}>
                <time
                  dateTime={event.occurred_at}
                  title={formatDateTime(event.occurred_at, workspace.scope.timezone)}
                  tabIndex={0}
                >
                  {formatClock(event.occurred_at, workspace.scope.timezone)}
                </time>
                <span>{event.title}</span>
                <small>{formatSourceName(event.source)}</small>
              </div>
            ))}
          </section>
        </div>

        <aside className="service-overview-aside">
          <section className="service-panel service-run-status">
            <SectionHeading title="运行状态" />
            <dl>
              <div><dt>服务健康</dt><dd><HealthBadge state={view.health.status} compact /></dd></div>
              <div><dt>数据完整性</dt><dd><DataStatusBadge state={view.data_status.state} compact /></dd></div>
              <div><dt>活动 Fault</dt><dd>{view.domain_counts.active_faults}</dd></div>
              <div><dt>Confirmed Impact</dt><dd>{view.domain_counts.confirmed_impacts}</dd></div>
              <div><dt>响应中 Incident</dt><dd>{view.domain_counts.active_incidents}</dd></div>
            </dl>
          </section>

          <section className="service-panel service-diagnosis-launcher">
            <SectionHeading title="开始诊断" />
            <label>
              <span>定位字段</span>
              <Select
                value={focusKind}
                onChange={(value) => {
                  setFocusKind(value)
                  const definition = view.focus_kinds.find((focus) => focus.key === value)
                  setFocusValue(definition?.placeholder ?? '')
                }}
                options={view.focus_kinds.map((focus) => ({ value: focus.key, label: focus.label }))}
              />
            </label>
            <label>
              <span>{focusDefinition?.label ?? 'Focus'}</span>
              <Input
                value={focusValue}
                status={focusValue && !focusValid ? 'error' : undefined}
                aria-invalid={Boolean(focusValue && !focusValid)}
                aria-describedby={focusValue && !focusValid ? 'service-focus-validation' : 'service-focus-validation-note'}
                onChange={(event) => setFocusValue(event.target.value)}
                onPressEnter={openWorkspace}
                placeholder={focusDefinition?.placeholder}
              />
            </label>
            {focusValue && !focusValid ? (
              <small id="service-focus-validation" className="service-focus-error" role="alert">
                输入格式不符合当前定位字段。
              </small>
            ) : null}
            <label>
              <span>时间范围</span>
              <Select
                value={String(workspaceWindowSeconds)}
                aria-label="诊断时间范围"
                options={[{ value: String(workspaceWindowSeconds), label: workspaceWindowLabel }]}
              />
            </label>
            <Button type="primary" block disabled={!focusValid} onClick={openWorkspace}>定位请求</Button>
            <small id="service-focus-validation-note">将在可观测性工作区打开该请求；URL 不包含凭据或原始查询。</small>
          </section>

          <section className="service-panel service-responsibility">
            <SectionHeading title="责任与入口" />
            <dl>
              <div><dt>负责人</dt><dd>{view.service.owner_team_name}</dd></div>
              <div><dt>服务层级</dt><dd>{view.service.tier.replace('tier-', 'T')}</dd></div>
              <div><dt>环境</dt><dd>{view.service.environment}</dd></div>
              <div><dt>相关依赖数量</dt><dd>{view.dependencies.length}</dd></div>
            </dl>
            {view.links.map((link) => (
              <a key={link.kind} href={link.href} className="service-responsibility-link">
                {link.kind === 'dashboard' ? <DashboardOutlined /> : link.kind === 'runbook' ? <ReadOutlined /> : <SafetyCertificateOutlined />}
                {link.label}<ArrowRightOutlined />
              </a>
            ))}
          </section>
        </aside>
      </div>
    </main>
  )
}
