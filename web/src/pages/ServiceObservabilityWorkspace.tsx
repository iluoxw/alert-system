import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Button, Drawer, Input, Result, Select, Skeleton } from 'antd'
import {
  ClockCircleOutlined,
  ProfileOutlined,
  QuestionCircleOutlined,
  ReloadOutlined,
  SettingOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import {
  DataStatusBadge,
  GoldenLedger,
  ServiceBreadcrumb,
} from '../components/ServiceObservability'
import { JourneyRail } from '../components/serviceObservability/JourneyRail'
import { TopologyExplorer } from '../components/serviceObservability/TopologyExplorer'
import { WorkspaceContext } from '../components/serviceObservability/WorkspaceContext'
import { DomainTimeline } from '../components/serviceObservability/DomainTimeline'
import {
  serviceObservabilityFixtureMode,
  usePublishServiceLabel,
  useServiceWorkspace,
} from '../serviceObservability/data'
import { formatClock, formatDateTime, formatWindow, humanizeIdentifier } from '../serviceObservability/format'
import {
  selectionForEvent,
  selectionForStage,
  stageForEvent,
  stageForSelection,
} from '../serviceObservability/workspaceModel'
import {
  canonicalWorkspaceSearchParams,
  patchWorkspaceSearchParams,
  readWorkspaceViewState,
  sameSearchParams,
} from '../serviceObservability/workspaceState'
import type { ContextTab, TopologyMode, TopologyScope, WorkspaceQueryPatch } from '../serviceObservability/workspaceState'
import type { JourneyStage, ObservabilityWorkspaceView, TimelineEvent, WorkspaceScenarioFixture } from '../serviceObservability/types'
import { useMediaQuery } from '../serviceObservability/useMediaQuery'
import { ApiError } from '../api/client'

const workspaceViewQueryKeys = ['stage', 'selected', 'tab', 'topology', 'scope', 'cursor'] as const

export function ServiceObservabilityWorkspace() {
  const { serviceId = '' } = useParams()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()

  // 服务端查询参数全部来自 URL：focus、时间窗与时区因此是可分享的事实源，
  // 页面不自己发明它们，也不在 live 模式下用 fixture 兜底。
  const serverQuery = useMemo(() => ({
    environment: searchParams.get('environment') ?? undefined,
    focusKind: searchParams.get('focus_kind') ?? undefined,
    focusValue: searchParams.get('focus_value') ?? undefined,
    from: searchParams.get('from') ?? undefined,
    to: searchParams.get('to') ?? undefined,
    timezone: searchParams.get('timezone') ?? undefined,
  }), [searchParams])
  // scenario 只在显式 fixture 模式有效：它是 demo/视觉 QA 的状态选择器，不是 live 概念。
  const scenarioValue = serviceObservabilityFixtureMode ? searchParams.get('scenario') : null

  const workspaceQuery = useServiceWorkspace(serviceId, serverQuery, scenarioValue)
  const view = workspaceQuery.data?.view
  usePublishServiceLabel(view?.service.name)

  if (workspaceQuery.isPending) {
    return (
      <main className="service-workspace-page" aria-busy="true" aria-label="正在加载可观测性工作区">
        <Skeleton active title={{ width: '28%' }} paragraph={{ rows: 2 }} />
        <Skeleton active title paragraph={{ rows: 10 }} />
      </main>
    )
  }
  if (workspaceQuery.isError || !view) {
    return (
      <WorkspaceErrorState
        serviceId={serviceId}
        error={workspaceQuery.error}
        onRetry={() => workspaceQuery.refetch()}
        onBack={() => navigate('/services')}
      />
    )
  }
  return (
    <WorkspaceContent
      view={view}
      scenario={workspaceQuery.data?.scenario ?? null}
      isRefetching={workspaceQuery.isFetching}
      onRetry={() => workspaceQuery.refetch()}
    />
  )
}

function WorkspaceContent({
  view,
  scenario,
  isRefetching,
  onRetry,
}: {
  view: ObservabilityWorkspaceView
  scenario: WorkspaceScenarioFixture | null
  isRefetching: boolean
  onRetry: () => void
}) {
  const [searchParams, setSearchParams] = useSearchParams()
  const viewState = useMemo(() => readWorkspaceViewState(searchParams, view), [searchParams, view])
  const [focusValue, setFocusValue] = useState(searchParams.get('focus_value') ?? view.focus?.value ?? '')
  const [contextDrawerOpen, setContextDrawerOpen] = useState(false)
  const [journeyExpanded, setJourneyExpanded] = useState(false)
  const contextInDrawer = useMediaQuery('(max-width: 1439px)')
  const compactJourney = useMediaQuery('(max-width: 1179px)')
  const mobileWorkspace = useMediaQuery('(max-width: 900px)')

  useEffect(() => {
    // 显式 QA 场景下 focus 以场景视图为准：URL 不保留与视图矛盾的旧 focus。
    const canonical = canonicalWorkspaceSearchParams(searchParams, view, viewState, Boolean(scenario))
    if (!sameSearchParams(searchParams, canonical)) setSearchParams(canonical, { replace: true })
  }, [scenario, searchParams, setSearchParams, view, viewState])

  useEffect(() => {
    setFocusValue(searchParams.get('focus_value') ?? view.focus?.value ?? '')
  }, [searchParams, view])

  useEffect(() => {
    if (!contextInDrawer) setContextDrawerOpen(false)
  }, [contextInDrawer])

  const emptyFocus = view.sections.find((section) => section.section_key === 'journey')?.state === 'empty'
  const retryableFailure = view.data_status.sources.some((source) => source.state === 'unavailable' && source.retryable)
  const focusKind = view.focus?.kind ?? searchParams.get('focus_kind') ?? ''
  const focusLabel = focusKind ? humanizeIdentifier(focusKind) : '请求定位'
  const windowSeconds = Math.max(1, Math.round((Date.parse(view.scope.to) - Date.parse(view.scope.from)) / 1000))

  const updateQuery = (patch: WorkspaceQueryPatch, replace = false) => {
    setSearchParams(patchWorkspaceSearchParams(searchParams, patch), { replace })
  }

  const runDiagnosis = () => {
    const value = focusValue.trim()
    if (!value || !focusKind) return
    // 只改 URL：新的 focus 由服务端（fixture 模式下由 query-aware provider）重新装配一次，
    // 前端不猜结果。
    //
    // fixture 模式必须同时清掉 QA `scenario`：留着它就等于"锁死一张固定截图"，于是输入框
    // 和 URL 是新 focus、Journey 卡还显示旧场景的 ID。live 模式没有 scenario 概念，
    // 这里的 null 是无害的幂等清理。
    updateQuery({
      focus_kind: focusKind,
      focus_value: value,
      scenario: null,
      stage: null,
      selected: null,
      cursor: null,
      tab: 'summary',
      scope: 'focus',
    })
  }

  // 重试失败数据在 live 模式就是 refetch：它绝不修改 scenario（scenario 只是 fixture
  // 模式的状态选择器，用它"修复"一次真实故障等于把故障藏起来）。
  const retryFailed = () => {
    if (serviceObservabilityFixtureMode) {
      updateQuery({ scenario: null, stage: null, selected: null, cursor: null, tab: 'summary', scope: 'focus' })
      return
    }
    onRetry()
  }

  const selectStage = (stage: JourneyStage) => {
    updateQuery({
      stage: stage.key,
      selected: selectionForStage(view, stage) ?? null,
      cursor: null,
      tab: 'summary',
      scope: 'focus',
    })
  }

  const selectObject = (value: string) => {
    const matchingStage = stageForSelection(view, value)
    updateQuery({
      selected: value,
      stage: matchingStage?.key ?? viewState.selectedStageKey ?? null,
      cursor: null,
      tab: 'summary',
    })
    if (contextInDrawer) setContextDrawerOpen(true)
  }

  const selectEvent = (event: TimelineEvent) => {
    const selected = selectionForEvent(view, event)
    const stage = stageForEvent(view, event)
    updateQuery({
      cursor: event.event_id,
      selected: selected ?? null,
      stage: stage?.key ?? null,
      tab: 'events',
      scope: 'focus',
    })
    if (contextInDrawer) setContextDrawerOpen(true)
  }

  const changeTab = (tab: ContextTab) => updateQuery({ tab })
  const changeTopologyMode = (topology: TopologyMode) => updateQuery({ topology })
  const changeTopologyScope = (scope: TopologyScope) => updateQuery({ scope })

  const contextPanel = (
    <WorkspaceContext
      view={view}
      activeTab={viewState.activeTab}
      selectedObject={viewState.selectedObject}
      selectedStageKey={viewState.selectedStageKey}
      cursor={viewState.cursor}
      onTabChange={changeTab}
      onEventSelect={selectEvent}
    />
  )

  return (
    <main className="service-workspace-page" data-view-state-keys={workspaceViewQueryKeys.join(',')}>
      <section className="workspace-command-bar">
        <ServiceBreadcrumb service={view.service} workspace />
        <span className="workspace-owner"><ProfileOutlined /> 负责人 {view.service.owner_team_name}</span>
        <span>层级 <b>{view.service.tier.replace('tier-', 'T')}</b></span>
        <Select className="workspace-time-range" value={String(windowSeconds)} options={[{ value: String(windowSeconds), label: `最近 ${formatWindow(windowSeconds)}` }]} aria-label="时间范围" />
        <Select className="workspace-focus-kind" value={focusKind || undefined} placeholder="选择定位字段" options={focusKind ? [{ value: focusKind, label: focusLabel }] : []} aria-label="Focus 类型" />
        <Input className="workspace-focus-input" value={focusValue} onChange={(event) => setFocusValue(event.target.value)} onPressEnter={runDiagnosis} aria-label="Focus 值" />
        <Button className="workspace-run-diagnosis" type="primary" onClick={runDiagnosis} disabled={!focusValue.trim() || !focusKind}>执行只读诊断</Button>
        <span className="workspace-data-state">
          <DataStatusBadge state={view.data_status.state} compact />
          <time dateTime={view.assembled_at} title={formatDateTime(view.assembled_at, view.scope.timezone)}>
            {formatClock(view.assembled_at, view.scope.timezone)}
          </time>
        </span>
      </section>

      {(retryableFailure || view.data_status.state === 'stale' || emptyFocus) && (
        <section className={`workspace-state-banner is-${view.data_status.state}${emptyFocus ? ' is-empty' : ''}`} role="status">
          {retryableFailure ? <WarningOutlined /> : view.data_status.state === 'stale' ? <ClockCircleOutlined /> : <QuestionCircleOutlined />}
          <span>
            <strong>{emptyFocus ? `查询成功，但没有匹配该 ${focusLabel} 的请求` : retryableFailure ? '部分数据不可用' : 'Observed path 已过新鲜度 TTL'}</strong>
            {emptyFocus ? '保留 declared topology；不会发明 journey 或请求路径。' : view.diagnostic_summary.title}
          </span>
          <Button icon={<ReloadOutlined />} loading={isRefetching} onClick={retryFailed}>
            {emptyFocus ? '重新查询' : retryableFailure ? '重试失败数据' : '刷新观测'}
          </Button>
        </section>
      )}

      <GoldenLedger signals={view.golden_signals} timezone={view.scope.timezone} />

      <div className={`workspace-main-grid${contextInDrawer ? ' has-context-drawer' : ''}${compactJourney ? ' has-compact-journey' : ''}`}>
        <JourneyRail
          view={view}
          selectedStageKey={viewState.selectedStageKey}
          onSelect={selectStage}
          compact={compactJourney}
          expanded={journeyExpanded}
          onToggle={() => setJourneyExpanded((value) => !value)}
        />
        <TopologyExplorer
          view={view}
          selectedStageKey={viewState.selectedStageKey}
          mode={viewState.topologyMode}
          scope={viewState.topologyScope}
          selectedObject={viewState.selectedObject}
          onModeChange={changeTopologyMode}
          onScopeChange={changeTopologyScope}
          onSelectObject={selectObject}
          onOpenContext={contextInDrawer ? () => setContextDrawerOpen(true) : undefined}
        />
        {!contextInDrawer && contextPanel}
      </div>

      <DomainTimeline view={view} cursor={viewState.cursor} onSelect={selectEvent} />

      <Drawer
        title={`${view.service.name} · 调查上下文`}
        placement="right"
        width={mobileWorkspace ? '100%' : 440}
        open={contextInDrawer && contextDrawerOpen}
        onClose={() => setContextDrawerOpen(false)}
        className="workspace-context-drawer"
        destroyOnHidden
      >
        {contextPanel}
      </Drawer>

      <span className="workspace-live-region" aria-live="polite">
        {/* fixture 主视图绝不能自称"服务端聚合视图"：那会让 QA 以为看到的是 live 数据。 */}
        {scenario
          ? `已加载 fixture 场景：${scenario.label}`
          : serviceObservabilityFixtureMode
            ? '已加载 fixture 主视图'
            : '已加载服务端聚合视图'}；当前选择 {viewState.selectedObject ?? '无'}；拓扑范围 {viewState.topologyScope}；当前页签 {viewState.activeTab}
      </span>
    </main>
  )
}

// WorkspaceErrorState 把 404 / 409 / 422 / 503 分别渲染成可操作的页面状态。
//
// 409 是最需要区分的一种：服务存在、责任与领域状态都在，只是没有已发布 Profile——
// 工作区不猜指标也不猜布局，而是把人送去服务登记。
function WorkspaceErrorState({
  serviceId,
  error,
  onRetry,
  onBack,
}: {
  serviceId: string
  error: unknown
  onRetry: () => void
  onBack: () => void
}) {
  const apiError = error instanceof ApiError ? error : undefined
  const status = apiError?.status ?? (error as { status?: number } | undefined)?.status
  if (status === 409) {
    return (
      <Result
        icon={<SettingOutlined />}
        title="该服务尚未配置 Service Profile"
        subTitle="工作区需要已发布 Profile 才能确定 focus、journey 与信号绑定；服务总览、责任与领域状态仍然可用。"
        extra={[
          <Link key="overview" className="ant-btn ant-btn-primary" to={`/services/${serviceId}`}>返回服务总览</Link>,
          <Link key="registration" className="ant-btn ant-btn-default" to={`/settings/services/${serviceId}`}>前往服务登记</Link>,
        ]}
      />
    )
  }
  if (status === 404) {
    return (
      <Result
        status="404"
        title="服务工作区不存在"
        subTitle={serviceObservabilityFixtureMode
          ? '当前 fixture 集合中没有该服务。'
          : '该服务没有登记，或不在当前环境内。'}
        extra={<Button onClick={onBack}>返回服务目录</Button>}
      />
    )
  }
  if (status === 422) {
    const scopeTooBroad = apiError?.reasonCode === 'scope_too_broad'
    return (
      <Result
        status="warning"
        title={scopeTooBroad ? '查询范围超出限制' : '定位参数非法'}
        subTitle={apiError?.message ?? (scopeTooBroad
          ? '请收窄时间窗后重试；服务端不会静默截断成一个你没请求过的范围。'
          : 'focus 或时间窗不满足该服务已发布 Profile 的定义。')}
        extra={[
          <Link key="overview" className="ant-btn ant-btn-primary" to={`/services/${serviceId}`}>返回服务总览</Link>,
          <Button key="back" onClick={onBack}>返回服务目录</Button>,
        ]}
      />
    )
  }
  return (
    <Result
      status="500"
      title="可观测性工作区暂时不可用"
      subTitle={apiError?.message ?? '聚合视图请求失败；页面不会用历史数据或 fixture 掩盖这次故障。'}
      extra={[
        <Button key="retry" type="primary" icon={<ReloadOutlined />} onClick={onRetry}>重试</Button>,
        <Button key="back" onClick={onBack}>返回服务目录</Button>,
      ]}
    />
  )
}
