import { formatClock, formatDateTime, formatSourceName, formatWindow } from '../../serviceObservability/format'
import type { ObservabilityWorkspaceView, TimelineEvent } from '../../serviceObservability/types'

export function DomainTimeline({
  view,
  cursor,
  onSelect,
}: {
  view: ObservabilityWorkspaceView
  cursor?: string
  onSelect: (event: TimelineEvent) => void
}) {
  const windowSeconds = Math.max(1, Math.round((Date.parse(view.scope.to) - Date.parse(view.scope.from)) / 1000))
  return (
    <section className="workspace-timeline">
      <header><h3>事件时间线 · 当前 {formatWindow(windowSeconds)}窗口</h3><span>{view.scope.timezone}</span></header>
      <div className="workspace-timeline__track">
        {view.timeline.map((event) => (
          <button
            type="button"
            key={event.event_id}
            className={`is-${event.kind}${cursor === event.event_id ? ' is-selected' : ''}`}
            onClick={() => onSelect(event)}
            aria-pressed={cursor === event.event_id}
          >
            <time dateTime={event.occurred_at} title={formatDateTime(event.occurred_at, view.scope.timezone)}>{formatClock(event.occurred_at, view.scope.timezone)}</time>
            <span className="workspace-timeline__marker" />
            <strong>{event.title}</strong>
            <small>{formatSourceName(event.source)}</small>
          </button>
        ))}
      </div>
    </section>
  )
}
