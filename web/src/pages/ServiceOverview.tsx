import { Fragment, useMemo, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Button, Input, Result, Select, Skeleton } from 'antd'
import {
  ApiOutlined,
  AppstoreOutlined,
  ArrowRightOutlined,
  CloseCircleOutlined,
  CodeSandboxOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  ReadOutlined,
  ReloadOutlined,
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
import { serviceObservabilityFixtureMode, useServiceOverview, usePublishServiceLabel } from '../serviceObservability/data'
import {
  formatClock,
  formatDateTime,
  formatDuration,
  formatProvenance,
  formatSourceName,
  formatWindow,
} from '../serviceObservability/format'
import type { ServiceOverviewView } from '../serviceObservability/types'
import { ApiError } from '../api/client'

const dependencyIcon = (layer: string) => layer === 'infrastructure' ? <DatabaseOutlined /> : <ApiOutlined />
const serviceIcon = (kind: string) => kind === 'infrastructure_capability' ? <CodeSandboxOutlined /> : <AppstoreOutlined />

// defaultWindowSeconds 与服务端的兜底窗口一致：显式 Profile 默认窗口由服务端决定，
// 前端只在需要构造工作区链接时使用这个显示值，绝不用它覆盖服务端的时间窗。
const defaultWindowSeconds = 300

export function ServiceOverview() {
  const { serviceId = '' } = useParams()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const environment = searchParams.get('environment') ?? undefined
  // window_seconds **原样**下传：`0`、负数或非数字必须由服务端判成 422 invalid_window。
  // 以前这里归一成 undefined，于是非法链接会安静地展示一个缺省窗口的成功页面。
  const rawWindowSeconds = searchParams.get('window_seconds') ?? undefined
  const parsedWindowSeconds = Number(rawWindowSeconds)
  // 仅供显示（诊断启动器的时间范围）：非法值不参与请求，也不冒充服务端窗口。
  const displayWindowSeconds = Number.isInteger(parsedWindowSeconds) && parsedWindowSeconds > 0
    ? parsedWindowSeconds
    : undefined

  const overviewQuery = useServiceOverview(serviceId, {
    environment,
    windowSeconds: rawWindowSeconds,
  })
  const view = overviewQuery.data
  usePublishServiceLabel(view?.service.name)

  if (overviewQuery.isPending) {
    return (
      <main className="service-ops-page service-overview-page" aria-busy="true" aria-label="正在加载服务总览">
        <Skeleton active title={{ width: '30%' }} paragraph={{ rows: 3 }} />
        <Skeleton active title paragraph={{ rows: 6 }} />
      </main>
    )
  }
  if (overviewQuery.isError || !view) {
    return (
      <OverviewErrorState
        error={overviewQuery.error}
        onRetry={() => overviewQuery.refetch()}
        onBack={() => navigate('/services')}
      />
    )
  }
  return (
    <OverviewContent
      view={view}
      environment={environment}
      windowSeconds={displayWindowSeconds ?? defaultWindowSeconds}
    />
  )
}

function OverviewContent({
  view,
  environment,
  windowSeconds,
}: {
  view: ServiceOverviewView
  environment?: string
  windowSeconds: number
}) {
  const navigate = useNavigate()
  const primaryFocus = view.focus_kinds.find((focus) => focus.primary) ?? view.focus_kinds[0]
  const [focusKind, setFocusKind] = useState(primaryFocus?.key ?? '')
  const [focusValue, setFocusValue] = useState(primaryFocus?.placeholder ?? '')
  // 时区只影响显示。总览没有自己的 scope，因此统一用 UTC 渲染服务端返回的绝对时间。
  const timezone = 'UTC'

  const focusDefinition = view.focus_kinds.find((focus) => focus.key === focusKind)
  const focusValid = Boolean(focusDefinition && new RegExp(focusDefinition.value_pattern).test(focusValue.trim()))
  const dependencyGroups = useMemo(() => Array.from(view.dependencies.reduce((groups, dependency) => {
    const group = groups.get(dependency.group_key) ?? { key: dependency.group_key, label: dependency.group_label, items: [] }
    group.items.push(dependency)
    groups.set(dependency.group_key, group)
    return groups
  }, new Map<string, { key: string; label: string; items: typeof view.dependencies }>()).values()), [view.dependencies])
  const workspaceWindowLabel = `最近 ${formatWindow(windowSeconds)}`
  const unconfigured = view.profile === null

  const workspaceHref = (params?: URLSearchParams) => {
    const search = params ?? new URLSearchParams()
    if (environment) search.set('environment', environment)
    const query = search.toString()
    return `/services/${view.service.service_id}/observability${query ? `?${query}` : ''}`
  }

  const openWorkspace = () => {
    if (!focusValid) return
    const params = new URLSearchParams({ focus_kind: focusKind, focus_value: focusValue.trim() })
    navigate(workspaceHref(params))
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
              <span>Profile <b>{view.profile ? `v${view.profile.profile_version}` : '未配置'}</b></span>
            </div>
          </div>
        </div>
        <div className="service-overview-actions">
          <time dateTime={view.assembled_at} title={formatDateTime(view.assembled_at, timezone)}>
            服务上下文更新于 {formatClock(view.assembled_at, timezone)}
          </time>
          <Button type="primary" disabled={unconfigured} onClick={() => navigate(workspaceHref())}>
            进入可观测性工作区
          </Button>
          {view.links.map((link) => <a key={link.kind} href={link.href}>{link.label}</a>)}
        </div>
      </header>

      {unconfigured && (
        <section className="workspace-state-banner is-unconfigured" role="status">
          <WarningOutlined />
          <span>
            <strong>该服务尚未配置 Service Profile</strong>
            责任、declared 依赖与已提交的领域状态仍然可用；黄金信号与聚焦工作区需要先发布 Profile。
          </span>
          <Link className="ant-btn ant-btn-default" to={`/settings/services/${view.service.service_id}`}>
            前往服务登记
          </Link>
        </section>
      )}

      <GoldenLedger signals={view.golden_signals} timezone={timezone} />

      <div className="service-overview-layout">
        <div className="service-overview-main">
          <section className="service-panel">
            <SectionHeading title="当前需要判断" action={<Link to="/faults">查看全部 Fault <ArrowRightOutlined /></Link>} />
            <div className="service-fault-list">
              {view.faults.length === 0 && <p className="service-empty-note">当前没有未关闭的 Fault。</p>}
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
                            {dependency.observed_at ? (
                              <time
                                dateTime={dependency.observed_at}
                                title={formatDateTime(dependency.observed_at, timezone)}
                                tabIndex={0}
                              >
                                {formatClock(dependency.observed_at, timezone)}
                              </time>
                            ) : <span className="is-unknown">未观测</span>}
                          </td>
                          <td><Link aria-label={`打开 ${dependency.name}`} to={`/infrastructure/${dependency.entity_id}`}><ArrowRightOutlined /></Link></td>
                        </tr>
                      ))}
                    </Fragment>
                  ))}
                  {view.dependencies.length === 0 && (
                    <tr><td colSpan={7} className="service-empty-note">没有可显示的 declared 依赖。</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          </section>

          <section className="service-panel service-recent-events">
            <SectionHeading title="最近事件" />
            {view.recent_events.length === 0 && <p className="service-empty-note">当前边界内没有已提交的事件。</p>}
            {view.recent_events.map((event) => (
              <div key={event.event_id} className={`service-event-row is-${event.kind}`}>
                <time
                  dateTime={event.occurred_at}
                  title={formatDateTime(event.occurred_at, timezone)}
                  tabIndex={0}
                >
                  {formatClock(event.occurred_at, timezone)}
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
            {view.focus_kinds.length === 0 ? (
              <p className="service-empty-note">
                该服务尚未声明 focus 维度；发布 Service Profile 后才能按请求定位。
              </p>
            ) : (
              <>
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
                    value={String(windowSeconds)}
                    aria-label="诊断时间范围"
                    options={[{ value: String(windowSeconds), label: workspaceWindowLabel }]}
                  />
                </label>
                <Button type="primary" block disabled={!focusValid} onClick={openWorkspace}>定位请求</Button>
                <small id="service-focus-validation-note">将在可观测性工作区打开该请求；URL 不包含凭据或原始查询。</small>
              </>
            )}
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

// OverviewErrorState 按后端稳定 reason code 分支。API 失败绝不回落 fixture：
// 网络错误被 demo 数据掩盖，比页面报错危险得多。
function OverviewErrorState({
  error,
  onRetry,
  onBack,
}: {
  error: unknown
  onRetry: () => void
  onBack: () => void
}) {
  const apiError = error instanceof ApiError ? error : undefined
  const status = apiError?.status ?? (error as { status?: number } | undefined)?.status
  if (status === 404) {
    return (
      <Result
        status="404"
        title="服务不存在"
        subTitle={serviceObservabilityFixtureMode
          ? '当前 fixture 集合中没有该服务。'
          : '该服务没有登记，或不在当前环境内。'}
        extra={<Button onClick={onBack}>返回服务目录</Button>}
      />
    )
  }
  if (status === 422) {
    return (
      <Result
        status="warning"
        title="请求参数非法"
        subTitle={apiError?.message ?? '时间窗或环境参数不满足服务端约束。'}
        extra={<Button onClick={onBack}>返回服务目录</Button>}
      />
    )
  }
  return (
    <Result
      status="500"
      title="服务总览暂时不可用"
      subTitle={apiError?.message ?? '聚合视图请求失败；页面不会用历史数据或 fixture 掩盖这次故障。'}
      extra={[
        <Button key="retry" type="primary" icon={<ReloadOutlined />} onClick={onRetry}>重试</Button>,
        <Button key="back" onClick={onBack}>返回服务目录</Button>,
      ]}
    />
  )
}
