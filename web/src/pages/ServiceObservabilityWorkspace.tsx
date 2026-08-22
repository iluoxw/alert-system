import { useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Button, Drawer, Input, Result, Select } from 'antd'
import {
  ClockCircleOutlined,
  ProfileOutlined,
  QuestionCircleOutlined,
  ReloadOutlined,
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
import { getBaseWorkspaceFixture, getWorkspaceFixture } from '../serviceObservability/fixtures'
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
import type { ContextTab, TopologyMode, TopologyScope, WorkspaceQueryPatch, WorkspaceViewState } from '../serviceObservability/workspaceState'
import type { JourneyStage, TimelineEvent } from '../serviceObservability/types'
import { useMediaQuery } from '../serviceObservability/useMediaQuery'

const workspaceViewQueryKeys = ['stage', 'selected', 'tab', 'topology', 'scope', 'cursor'] as const

const emptyViewState: WorkspaceViewState = {
  activeTab: 'summary',
  topologyMode: 'canvas',
  topologyScope: 'focus',
}

export function ServiceObservabilityWorkspace() {
  const { serviceId = '' } = useParams()
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const scenarioValue = searchParams.get('scenario')
  const fixture = useMemo(() => getWorkspaceFixture(serviceId, scenarioValue), [serviceId, scenarioValue])
  const base = useMemo(() => getBaseWorkspaceFixture(serviceId), [serviceId])
  const viewState = useMemo(() => (
    fixture.view ? readWorkspaceViewState(searchParams, fixture.view) : emptyViewState
  ), [fixture.view, searchParams])
  const [focusValue, setFocusValue] = useState(searchParams.get('focus_value') ?? fixture.view?.focus.value ?? '')
  const [contextDrawerOpen, setContextDrawerOpen] = useState(false)
  const [journeyExpanded, setJourneyExpanded] = useState(false)
  const contextInDrawer = useMediaQuery('(max-width: 1439px)')
  const compactJourney = useMediaQuery('(max-width: 1179px)')
  const mobileWorkspace = useMediaQuery('(max-width: 900px)')

  useEffect(() => {
    if (!fixture.view) return
    const canonical = canonicalWorkspaceSearchParams(searchParams, fixture.view, viewState)
    if (!sameSearchParams(searchParams, canonical)) setSearchParams(canonical, { replace: true })
  }, [fixture.view, searchParams, setSearchParams, viewState])

  useEffect(() => {
    if (!fixture.view) return
    setFocusValue(searchParams.get('focus_value') ?? fixture.view.focus.value)
  }, [fixture.view, searchParams])

  useEffect(() => {
    if (!contextInDrawer) setContextDrawerOpen(false)
  }, [contextInDrawer])

  if (!fixture.view || !base) {
    return <Result status="404" title="服务工作区不存在" subTitle="当前 Phase 1 fixture 中没有该服务。" extra={<Button onClick={() => navigate('/services')}>返回服务目录</Button>} />
  }

  const view = fixture.view
  const emptyFocus = view.sections.find((section) => section.section_key === 'journey')?.state === 'empty'
  const retryableFailure = view.data_status.sources.some((source) => source.state === 'unavailable' && source.retryable)
  const focusLabel = humanizeIdentifier(view.focus.kind)
  const windowSeconds = Math.max(1, Math.round((Date.parse(view.scope.to) - Date.parse(view.scope.from)) / 1000))

  const updateQuery = (patch: WorkspaceQueryPatch, replace = false) => {
    setSearchParams(patchWorkspaceSearchParams(searchParams, patch), { replace })
  }

  const runDiagnosis = () => {
    const value = focusValue.trim()
    if (!value) return
    updateQuery({
      focus_kind: view.focus.kind,
      focus_value: value,
      from: view.scope.from,
      to: view.scope.to,
      scenario: value !== base.focus.value ? 'empty' : emptyFocus ? null : undefined,
      stage: null,
      selected: null,
      cursor: null,
      tab: 'summary',
      scope: value !== base.focus.value ? 'all' : 'focus',
    })
  }

  const retryFailed = () => {
    setFocusValue(base.focus.value)
    updateQuery({
      scenario: null,
      focus_kind: base.focus.kind,
      focus_value: base.focus.value,
      from: base.scope.from,
      to: base.scope.to,
      stage: base.focus.stage ?? null,
      selected: null,
      cursor: null,
      tab: 'summary',
      scope: 'focus',
    })
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
        <Select className="workspace-focus-kind" value={view.focus.kind} options={[{ value: view.focus.kind, label: focusLabel }]} aria-label="Focus 类型" />
        <Input className="workspace-focus-input" value={focusValue} onChange={(event) => setFocusValue(event.target.value)} onPressEnter={runDiagnosis} aria-label="Focus 值" />
        <Button className="workspace-run-diagnosis" type="primary" onClick={runDiagnosis} disabled={!focusValue.trim()}>执行只读诊断</Button>
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
          <Button icon={<ReloadOutlined />} onClick={retryFailed}>{emptyFocus ? '恢复主 fixture' : retryableFailure ? '重试失败数据' : '刷新 Trace'}</Button>
        </section>
      )}

      <GoldenLedger signals={view.golden_signals} />

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
        {fixture.scenario ? `已加载场景：${fixture.scenario.label}` : '已加载完整 fixture'}；当前选择 {viewState.selectedObject ?? '无'}；拓扑范围 {viewState.topologyScope}；当前页签 {viewState.activeTab}
      </span>
    </main>
  )
}
