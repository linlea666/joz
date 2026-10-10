import type { LogEntry } from '../../lib/api/logs'

const colors: Record<string, string> = {
  error: '#c73429',
  warn: '#9b6400',
  success: '#15803d',
  info: '#4f607a',
}
export function formatContext(raw: string) {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}
export function EventList({
  events,
  onTrace,
}: {
  events: LogEntry[]
  onTrace: (row: LogEntry) => void
}) {
  if (!events.length)
    return (
      <p className="p-8 text-center text-nofx-text-muted">
        当前筛选下没有日志。历史记录可能尚未产生或已过保留期。
      </p>
    )
  return (
    <div className="space-y-3">
      {events.map((row) => (
        <article
          key={`${row.scope}-${row.id}`}
          className="rounded-xl border border-nofx-border bg-nofx-bg p-4"
        >
          <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-sm">
            <time className="text-nofx-text-muted">
              {new Date(row.occurred_at).toLocaleString()}
            </time>
            <b style={{ color: colors[row.level] || colors.info }}>
              {row.level.toUpperCase()}
            </b>
            <span className="font-mono text-nofx-text-muted">
              [{row.component}] {row.event}
            </span>
            <span>{row.message}</span>
          </div>
          <div className="mt-2 flex flex-wrap gap-3 text-xs text-nofx-text-muted">
            {row.trader_id && <span>交易员：{row.trader_id}</span>}
            {row.channel_id && <span>来源频道：{row.channel_id}</span>}
            {row.logical_channel_id && (
              <span>逻辑频道：{row.logical_channel_id}</span>
            )}
            {row.duration_ms > 0 && <span>{row.duration_ms} ms</span>}
          </div>
          <details className="mt-3 text-sm">
            <summary className="cursor-pointer">查看上下文与关联 ID</summary>
            <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-all text-xs">
              {formatContext(
                JSON.stringify({
                  source_event_id: row.source_event_id,
                  delivery_id: row.delivery_id,
                  signal_id: row.signal_id,
                  trace_id: row.trace_id,
                  message_id: row.message_id,
                })
              )}
            </pre>
            <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-all text-xs">
              {formatContext(row.context_json)}
            </pre>
          </details>
          {row.scope === 'trade' &&
            (row.signal_id ||
              row.source_event_id ||
              row.trace_id?.startsWith('reconcile-')) && (
              <button
                className="mt-3 text-sm font-bold text-nofx-gold"
                onClick={() => onTrace(row)}
              >
                查看完整链路
              </button>
            )}
        </article>
      ))}
    </div>
  )
}
