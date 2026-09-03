import { useState, useEffect, useCallback } from 'react'
import { X as IconX, RefreshCw, Radio, Download } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '../../lib/api'
import type {
  CopyTradeEvent,
  CopyTradeSignal,
  CopyTradeContext,
  CopyTradeAIStat,
} from '../../types'
import { t, type Language } from '../../i18n/translations'

type LogTab = 'events' | 'signals' | 'contexts' | 'aistats'
type TimeRange = 'all' | 'today' | '7d' | '30d' | 'custom'

// rangeBounds converts the selected range into unix-ms bounds (0 = unbounded).
function rangeBounds(
  range: TimeRange,
  customStart: string,
  customEnd: string
): { startMs?: number; endMs?: number } {
  const now = Date.now()
  switch (range) {
    case 'today': {
      const d = new Date()
      d.setHours(0, 0, 0, 0)
      return { startMs: d.getTime() }
    }
    case '7d':
      return { startMs: now - 7 * 24 * 3600 * 1000 }
    case '30d':
      return { startMs: now - 30 * 24 * 3600 * 1000 }
    case 'custom': {
      const start = customStart ? new Date(customStart).getTime() : undefined
      const end = customEnd ? new Date(customEnd).getTime() : undefined
      return {
        startMs: start && !Number.isNaN(start) ? start : undefined,
        endMs: end && !Number.isNaN(end) ? end : undefined,
      }
    }
    default:
      return {}
  }
}

interface CopyTradeLogModalProps {
  traderId: string
  traderName: string
  language: Language
  onClose: () => void
}

function fmtTime(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(undefined, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

const LEVEL_COLORS: Record<string, string> = {
  info: '#8A8478',
  success: '#2E8B57',
  warn: '#C9862B',
  error: '#D6433A',
}

// Full trace view of the copy-trading pipeline: events, interpreted signals,
// active trade contexts and per-model AI latency comparison.
export function CopyTradeLogModal({
  traderId,
  traderName,
  language,
  onClose,
}: CopyTradeLogModalProps) {
  const [tab, setTab] = useState<LogTab>('events')
  const [isLoading, setIsLoading] = useState(false)
  const [isExporting, setIsExporting] = useState(false)
  const [range, setRange] = useState<TimeRange>('all')
  const [customStart, setCustomStart] = useState('')
  const [customEnd, setCustomEnd] = useState('')
  const [events, setEvents] = useState<CopyTradeEvent[]>([])
  const [signals, setSignals] = useState<CopyTradeSignal[]>([])
  const [contexts, setContexts] = useState<CopyTradeContext[]>([])
  const [aiStats, setAIStats] = useState<CopyTradeAIStat[]>([])

  const load = useCallback(async () => {
    setIsLoading(true)
    try {
      const { startMs, endMs } = rangeBounds(range, customStart, customEnd)
      if (tab === 'events') {
        setEvents(await api.getCopyTradeEvents(traderId, 200, 0, startMs, endMs))
      } else if (tab === 'signals') {
        setSignals(await api.getCopyTradeSignals(traderId, 100, startMs, endMs))
      } else if (tab === 'contexts') {
        setContexts(await api.getCopyTradeContexts(traderId))
      } else {
        setAIStats(await api.getCopyTradeAIStats(7))
      }
    } catch {
      toast.error(t('copytrade.loadFailed', language))
    } finally {
      setIsLoading(false)
    }
  }, [tab, traderId, language, range, customStart, customEnd])

  const handleExport = async () => {
    if (isExporting || (tab !== 'events' && tab !== 'signals')) return
    setIsExporting(true)
    try {
      const { startMs, endMs } = rangeBounds(range, customStart, customEnd)
      await api.exportCopyTradeCSV(tab, traderId, startMs, endMs)
    } catch {
      toast.error(t('copytrade.exportFailed', language))
    } finally {
      setIsExporting(false)
    }
  }

  useEffect(() => {
    load()
  }, [load])

  // Auto-refresh events every 10s while open
  useEffect(() => {
    if (tab !== 'events') return
    const timer = setInterval(load, 10000)
    return () => clearInterval(timer)
  }, [tab, load])

  const tabs: { key: LogTab; label: string }[] = [
    { key: 'events', label: t('copytrade.tabEvents', language) },
    { key: 'signals', label: t('copytrade.tabSignals', language) },
    { key: 'contexts', label: t('copytrade.tabContexts', language) },
    { key: 'aistats', label: t('copytrade.tabAIStats', language) },
  ]

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black bg-opacity-50 backdrop-blur-sm p-4 overflow-y-auto">
      <div
        className="bg-nofx-bg-lighter border border-nofx-gold/20 rounded-xl shadow-2xl max-w-4xl w-full my-8 flex flex-col"
        style={{ maxHeight: 'calc(100vh - 4rem)' }}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-5 border-b border-nofx-gold/20">
          <div className="flex items-center gap-3">
            <div className="w-9 h-9 rounded-lg bg-nofx-gold flex items-center justify-center text-white">
              <Radio className="w-5 h-5" />
            </div>
            <div>
              <h2 className="text-lg font-bold text-nofx-text">
                {t('copytrade.logTitle', language)}
              </h2>
              <p className="text-xs text-nofx-text-muted">{traderName}</p>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={load}
              disabled={isLoading}
              className="p-2 rounded-lg text-nofx-text-muted hover:text-nofx-text hover:bg-nofx-bg-deeper transition-colors disabled:opacity-50"
              title={t('copytrade.refresh', language)}
            >
              <RefreshCw
                className={`w-4 h-4 ${isLoading ? 'animate-spin' : ''}`}
              />
            </button>
            <button
              onClick={onClose}
              className="w-8 h-8 rounded-lg text-nofx-text-muted hover:text-nofx-text hover:bg-nofx-bg-deeper transition-colors flex items-center justify-center"
            >
              <IconX className="w-4 h-4" />
            </button>
          </div>
        </div>

        {/* Tabs */}
        <div className="flex gap-1 px-5 pt-3">
          {tabs.map((item) => (
            <button
              key={item.key}
              onClick={() => setTab(item.key)}
              className={`px-4 py-2 rounded-t-lg text-sm font-medium transition-colors ${
                tab === item.key
                  ? 'bg-nofx-bg text-nofx-text border border-b-0 border-nofx-gold/20'
                  : 'text-nofx-text-muted hover:text-nofx-text'
              }`}
            >
              {item.label}
            </button>
          ))}
        </div>

        {/* Time range + export toolbar (events / signals only) */}
        {(tab === 'events' || tab === 'signals') && (
          <div className="flex flex-wrap items-center gap-2 px-5 pt-3">
            {(
              [
                ['all', t('copytrade.rangeAll', language)],
                ['today', t('copytrade.rangeToday', language)],
                ['7d', t('copytrade.range7d', language)],
                ['30d', t('copytrade.range30d', language)],
                ['custom', t('copytrade.rangeCustom', language)],
              ] as [TimeRange, string][]
            ).map(([key, label]) => (
              <button
                key={key}
                onClick={() => setRange(key)}
                className={`px-2.5 py-1 rounded-lg text-xs transition-colors ${
                  range === key
                    ? 'bg-nofx-gold text-white'
                    : 'bg-nofx-bg-deeper text-nofx-text-muted hover:text-nofx-text'
                }`}
              >
                {label}
              </button>
            ))}
            {range === 'custom' && (
              <>
                <input
                  type="datetime-local"
                  value={customStart}
                  onChange={(e) => setCustomStart(e.target.value)}
                  className="px-2 py-1 rounded-lg bg-nofx-bg-deeper text-xs text-nofx-text border border-nofx-gold/20"
                />
                <span className="text-xs text-nofx-text-muted">—</span>
                <input
                  type="datetime-local"
                  value={customEnd}
                  onChange={(e) => setCustomEnd(e.target.value)}
                  className="px-2 py-1 rounded-lg bg-nofx-bg-deeper text-xs text-nofx-text border border-nofx-gold/20"
                />
              </>
            )}
            <button
              onClick={handleExport}
              disabled={isExporting}
              className="ml-auto flex items-center gap-1.5 px-3 py-1 rounded-lg text-xs bg-nofx-bg-deeper text-nofx-text-muted hover:text-nofx-text transition-colors disabled:opacity-50"
            >
              <Download className="w-3.5 h-3.5" />
              {t('copytrade.exportCSV', language)}
            </button>
          </div>
        )}

        {/* Content */}
        <div className="flex-1 overflow-y-auto p-5 pt-3 min-h-[300px]">
          {/* Events */}
          {tab === 'events' &&
            (events.length === 0 ? (
              <EmptyHint text={t('copytrade.noEvents', language)} />
            ) : (
              <div className="space-y-1.5 font-mono text-xs">
                {events.map((ev) => (
                  <div
                    key={ev.id}
                    className="flex items-start gap-2 px-3 py-2 rounded bg-nofx-bg border border-nofx-gold/10"
                  >
                    <span className="text-nofx-text-muted whitespace-nowrap">
                      {fmtTime(ev.occurred_at)}
                    </span>
                    <span
                      className="whitespace-nowrap font-semibold"
                      style={{ color: LEVEL_COLORS[ev.level] || '#8A8478' }}
                    >
                      {ev.event.replace(/^copytrade\./, '')}
                    </span>
                    <span className="text-nofx-text break-all">
                      {ev.message}
                    </span>
                    {ev.duration_ms > 0 && (
                      <span className="ml-auto text-nofx-text-muted whitespace-nowrap">
                        {ev.duration_ms}ms
                      </span>
                    )}
                  </div>
                ))}
              </div>
            ))}

          {/* Signals */}
          {tab === 'signals' &&
            (signals.length === 0 ? (
              <EmptyHint text={t('copytrade.noSignals', language)} />
            ) : (
              <table className="w-full text-xs">
                <thead>
                  <tr className="text-left text-nofx-text-muted border-b border-nofx-gold/20">
                    <th className="py-2 pr-2">{t('copytrade.colTime', language)}</th>
                    <th className="py-2 pr-2">{t('copytrade.colAction', language)}</th>
                    <th className="py-2 pr-2">{t('copytrade.colSymbol', language)}</th>
                    <th className="py-2 pr-2">{t('copytrade.colStatus', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colLatency', language)}</th>
                    <th className="py-2 text-right">{t('copytrade.colTotalMs', language)}</th>
                  </tr>
                </thead>
                <tbody>
                  {signals.map((sig) => (
                    <tr
                      key={sig.id}
                      className="border-b border-nofx-gold/10 text-nofx-text"
                    >
                      <td className="py-2 pr-2 font-mono whitespace-nowrap">
                        {fmtTime(sig.message_timestamp)}
                      </td>
                      <td className="py-2 pr-2 font-semibold">
                        {sig.action || sig.classification || '-'}
                        {sig.direction ? ` ${sig.direction}` : ''}
                      </td>
                      <td className="py-2 pr-2 font-mono">{sig.symbol || '-'}</td>
                      <td className="py-2 pr-2">
                        <span
                          style={{
                            color:
                              sig.status === 'executed'
                                ? '#2E8B57'
                                : sig.status === 'failed'
                                  ? '#D6433A'
                                  : sig.status === 'skipped'
                                    ? '#C9862B'
                                    : '#8A8478',
                          }}
                        >
                          {sig.status}
                          {sig.skip_reason ? ` (${sig.skip_reason})` : ''}
                        </span>
                      </td>
                      <td className="py-2 pr-2 text-right font-mono">
                        {sig.llm_request_ms > 0 ? sig.llm_request_ms : '-'}
                      </td>
                      <td className="py-2 text-right font-mono">
                        {sig.total_ms > 0 ? sig.total_ms : '-'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ))}

          {/* Active trade contexts */}
          {tab === 'contexts' &&
            (contexts.length === 0 ? (
              <EmptyHint text={t('copytrade.noContexts', language)} />
            ) : (
              <table className="w-full text-xs">
                <thead>
                  <tr className="text-left text-nofx-text-muted border-b border-nofx-gold/20">
                    <th className="py-2 pr-2">{t('copytrade.colSymbol', language)}</th>
                    <th className="py-2 pr-2">{t('copytrade.colState', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colEntry', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colQty', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colSL', language)}</th>
                    <th className="py-2">{t('copytrade.colTime', language)}</th>
                  </tr>
                </thead>
                <tbody>
                  {contexts.map((ctx) => (
                    <tr
                      key={ctx.id}
                      className="border-b border-nofx-gold/10 text-nofx-text"
                    >
                      <td className="py-2 pr-2 font-mono font-semibold">
                        {ctx.symbol}{' '}
                        <span
                          style={{
                            color: ctx.direction === 'LONG' ? '#2E8B57' : '#D6433A',
                          }}
                        >
                          {ctx.direction}
                        </span>
                      </td>
                      <td className="py-2 pr-2">{ctx.state}</td>
                      <td className="py-2 pr-2 text-right font-mono">
                        {ctx.avg_fill_price || ctx.planned_entry_price || '-'}
                      </td>
                      <td className="py-2 pr-2 text-right font-mono">
                        {ctx.quantity || '-'}
                      </td>
                      <td className="py-2 pr-2 text-right font-mono">
                        {ctx.stop_loss_price || '-'}
                      </td>
                      <td className="py-2 font-mono whitespace-nowrap">
                        {fmtTime(ctx.created_at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ))}

          {/* AI latency stats */}
          {tab === 'aistats' &&
            (aiStats.length === 0 ? (
              <EmptyHint text={t('copytrade.noStats', language)} />
            ) : (
              <table className="w-full text-xs">
                <thead>
                  <tr className="text-left text-nofx-text-muted border-b border-nofx-gold/20">
                    <th className="py-2 pr-2">{t('copytrade.colModel', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colRuns', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colErrors', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colAvgMs', language)}</th>
                    <th className="py-2 pr-2 text-right">{t('copytrade.colMinMs', language)}</th>
                    <th className="py-2 text-right">{t('copytrade.colMaxMs', language)}</th>
                  </tr>
                </thead>
                <tbody>
                  {aiStats.map((s) => (
                    <tr
                      key={`${s.provider}-${s.model}`}
                      className="border-b border-nofx-gold/10 text-nofx-text"
                    >
                      <td className="py-2 pr-2 font-mono">
                        {s.model}
                        <span className="text-nofx-text-muted"> · {s.provider}</span>
                      </td>
                      <td className="py-2 pr-2 text-right font-mono">{s.runs}</td>
                      <td
                        className="py-2 pr-2 text-right font-mono"
                        style={{ color: s.errors > 0 ? '#D6433A' : undefined }}
                      >
                        {s.errors}
                      </td>
                      <td className="py-2 pr-2 text-right font-mono font-semibold">
                        {Math.round(s.avg_ms)}
                      </td>
                      <td className="py-2 pr-2 text-right font-mono">{s.min_ms}</td>
                      <td className="py-2 text-right font-mono">{s.max_ms}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ))}
        </div>
      </div>
    </div>
  )
}

function EmptyHint({ text }: { text: string }) {
  return (
    <div className="text-center py-12 text-nofx-text-muted text-sm">{text}</div>
  )
}
