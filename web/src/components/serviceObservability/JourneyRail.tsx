import {
  CheckCircleOutlined,
  CloseCircleOutlined,
  DownOutlined,
  QuestionCircleOutlined,
  RightOutlined,
} from '@ant-design/icons'
import { formatClock, formatDateTime, formatReasonCode, formatWindow, humanizeIdentifier } from '../../serviceObservability/format'
import type { JourneyStage, ObservabilityWorkspaceView } from '../../serviceObservability/types'

function stageIcon(stage: JourneyStage, index: number) {
  if (stage.state === 'ok') return <CheckCircleOutlined />
  if (stage.state === 'failed') return <CloseCircleOutlined />
  if (stage.state === 'unknown') return <QuestionCircleOutlined />
  return index + 1
}

function stageStateLabel(stage: JourneyStage): string {
  if (stage.reason_code) return formatReasonCode(stage.reason_code)
  if (stage.state === 'ok') return '已完成'
  if (stage.state === 'failed') return '失败'
  if (stage.state === 'pending') return '尚未开始'
  return '状态未知'
}

export function JourneyRail({
  view,
  selectedStageKey,
  onSelect,
  compact,
  expanded,
  onToggle,
}: {
  view: ObservabilityWorkspaceView
  selectedStageKey?: string
  onSelect: (stage: JourneyStage) => void
  compact: boolean
  expanded: boolean
  onToggle: () => void
}) {
  const journeyLabel = humanizeIdentifier(view.focus.journey)
  const focusLabel = humanizeIdentifier(view.focus.kind)
  const windowSeconds = Math.max(1, Math.round((Date.parse(view.scope.to) - Date.parse(view.scope.from)) / 1000))
  return (
    <section className={`workspace-journey-panel${compact ? ' is-compact' : ''}${expanded ? ' is-expanded' : ''}`}>
      <header>
        <div>
          <h3>{journeyLabel} 阶段</h3>
          <small>{view.focus.value}</small>
        </div>
        {compact && (
          <button type="button" className="journey-expand-button" onClick={onToggle} aria-expanded={expanded}>
            {expanded ? <DownOutlined /> : <RightOutlined />}
            {expanded ? '收起事实' : '展开事实'}
          </button>
        )}
      </header>
      <div className="journey-stage-list" role="list" aria-label={`${journeyLabel} 阶段`}>
        {view.journey.map((stage, index) => (
          <button
            type="button"
            role="listitem"
            key={stage.key}
            className={`is-${stage.state}${selectedStageKey === stage.key ? ' is-selected' : ''}`}
            onClick={() => onSelect(stage)}
            aria-pressed={selectedStageKey === stage.key}
          >
            <span className="journey-stage-icon">{stageIcon(stage, index)}</span>
            <span><strong>{stage.label}</strong><small>{stageStateLabel(stage)}</small></span>
            <em>{stage.state}</em>
          </button>
        ))}
      </div>
      {(!compact || expanded) && (
        <dl className="workspace-focus-facts">
          <div><dt>{focusLabel}</dt><dd>{view.focus.value}</dd></div>
          <div><dt>Tenant</dt><dd>{view.scope.tenant}</dd></div>
          <div><dt>Environment</dt><dd>{view.scope.environment}</dd></div>
          <div>
            <dt>Window</dt>
            <dd>
              <time dateTime={view.scope.from} title={`${formatDateTime(view.scope.from, view.scope.timezone)} – ${formatDateTime(view.scope.to, view.scope.timezone)}`} tabIndex={0}>
                {formatClock(view.scope.from, view.scope.timezone)}–{formatClock(view.scope.to, view.scope.timezone)} · {formatWindow(windowSeconds)}
              </time>
            </dd>
          </div>
        </dl>
      )}
    </section>
  )
}
