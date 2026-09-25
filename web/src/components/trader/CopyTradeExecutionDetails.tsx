import type { CopyTradeEvent, CopyTradeSignal } from '../../types'
import { t, type Language } from '../../i18n/translations'

function numeric(value: unknown, percent = false): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—'
  return `${value.toLocaleString(undefined, { maximumFractionDigits: 10 })}${percent ? '%' : ''}`
}

function readDecision(event: CopyTradeEvent): Record<string, unknown> | null {
  if (event.event !== 'copytrade.entry.decision') return null
  try {
    const context = JSON.parse(event.context_json || '{}')
    const decision = context?.decision
    if (!decision || typeof decision !== 'object' || Array.isArray(decision))
      return null
    return {
      ...decision,
      symbol: context.symbol,
      source_order_type: context.source_order_type,
    }
  } catch {
    return null
  }
}

export function tradeStateLabel(state: string, language: Language): string {
  const key = `copytrade.tradeStates.${state}`
  const label = t(key, language)
  return label === key ? state : label
}

export function CopyTradeExecutionDetails({
  events,
  language,
  expectsEntry,
  signal,
}: {
  events: CopyTradeEvent[] | 'loading' | 'error' | undefined
  language: Language
  expectsEntry: boolean
  signal?: CopyTradeSignal
}) {
  if (!events || events === 'loading') {
    return (
      <p className="text-xs text-nofx-text-muted py-2">
        {t('copytrade.loadingExecution', language)}
      </p>
    )
  }
  if (events === 'error') {
    return (
      <p className="text-xs text-nofx-danger py-2">
        {t('copytrade.executionLoadFailed', language)}
      </p>
    )
  }

  const decisions = events.flatMap((event) => {
    const decision = readDecision(event)
    return decision ? [{ event, decision }] : []
  })

  return (
    <div className="space-y-3 p-3 mb-3 rounded border border-nofx-gold/20">
      <h3 className="font-semibold text-nofx-text">
        {t('copytrade.executionDetails', language)}
      </h3>
      {!!signal?.instruction_results?.length && (
        <div className="space-y-1">
          <h4 className="font-semibold">
            {t('copytrade.actionResults', language)}
          </h4>
          {signal.instruction_results.map((result) => (
            <p key={result.index}>
              {result.symbol} {result.direction} · {result.action} ·{' '}
              {result.status}
              {result.skip_reason ? ` (${result.skip_reason})` : ''}
              {result.status !== 'executed' && result.detail
                ? `: ${result.detail}`
                : ''}
            </p>
          ))}
        </div>
      )}
      {!!signal?.action_results?.length && (
        <div className="space-y-1">
          {signal.action_results.map((action) => (
            <p key={action.id}>
              {action.symbol} {action.direction} · {action.action} ·{' '}
              {action.status}
              {action.trade_state
                ? ` · ${tradeStateLabel(action.trade_state, language)}`
                : ''}
              {action.error ? `: ${action.error}` : ''}
            </p>
          ))}
        </div>
      )}
      {!!signal?.order_legs?.length && (
        <div className="space-y-2">
          <h4 className="font-semibold">
            {t('copytrade.orderLegs', language)}
          </h4>
          {signal.order_legs.map((leg) => (
            <div key={leg.id} className="rounded bg-nofx-bg p-2">
              <p>
                {leg.symbol} {leg.direction} · {leg.role} · {leg.order_type} ·{' '}
                {leg.status}
              </p>
              <p>
                {t('copytrade.filledQuantity', language)}:{' '}
                {numeric(leg.executed_qty)} / {numeric(leg.quantity)} ·{' '}
                {t('copytrade.averageFill', language)}: {numeric(leg.avg_price)}
              </p>
              {leg.last_error && (
                <p className="text-nofx-danger">{leg.last_error}</p>
              )}
            </div>
          ))}
        </div>
      )}
      {expectsEntry && decisions.length === 0 && (
        <p className="text-nofx-text-muted">
          {t('copytrade.noEntryDecision', language)}
        </p>
      )}
      {decisions.map(({ event, decision: d }) => {
        const orderType = (value: unknown) => {
          if (value === 'MARKET') return t('copytrade.orderMarket', language)
          if (value === 'LIMIT') return t('copytrade.orderLimit', language)
          return typeof value === 'string' && value ? value : '—'
        }
        const fields = [
          [
            t('copytrade.authorOrderType', language),
            orderType(d.source_order_type),
          ],
          [t('copytrade.referencePrice', language), numeric(d.reference_price)],
          [
            t('copytrade.decisionMarketPrice', language),
            numeric(d.market_price),
          ],
          [
            t('copytrade.adverseDeviation', language),
            numeric(d.adverse_deviation_pct, true),
          ],
          [
            t('copytrade.appliedThreshold', language),
            numeric(d.threshold_pct, true),
          ],
          [
            t('copytrade.limitToMarket', language),
            typeof d.limit_to_market_within_threshold === 'boolean'
              ? t(
                  d.limit_to_market_within_threshold
                    ? 'copytrade.on'
                    : 'copytrade.off',
                  language
                )
              : '—',
          ],
          [t('copytrade.plannedOrderType', language), orderType(d.order_type)],
          [t('copytrade.plannedEntryPrice', language), numeric(d.entry_price)],
        ]
        const reasonKey = `copytrade.entryReasons.${d.reason}`
        const reason = t(reasonKey, language)
        return (
          <div key={event.id} className="rounded bg-nofx-bg p-3 space-y-2">
            <div className="font-semibold text-nofx-text">
              {t('copytrade.entryDecision', language)} ·{' '}
              {String(d.symbol || '')} {String(d.direction || '')}
            </div>
            <dl className="grid grid-cols-2 gap-x-4 gap-y-2">
              {fields.map(([label, value]) => (
                <div key={label}>
                  <dt className="text-nofx-text-muted">{label}</dt>
                  <dd className="font-mono text-nofx-text">{value}</dd>
                </div>
              ))}
            </dl>
            <p className="text-nofx-text">
              {reason === reasonKey ? event.message : reason}
            </p>
            <p className="text-nofx-text-muted">
              {t('copytrade.decisionHint', language)}
            </p>
          </div>
        )
      })}
      <div className="space-y-1 text-nofx-text">
        {events
          .filter(
            (event) =>
              event.event !== 'copytrade.entry.decision' || !readDecision(event)
          )
          .map((event) => (
            <div key={event.id} className="flex gap-2 break-words">
              <time className="text-nofx-text-muted whitespace-nowrap">
                {new Date(event.occurred_at).toLocaleString(undefined, {
                  month: '2-digit',
                  day: '2-digit',
                  hour: '2-digit',
                  minute: '2-digit',
                  second: '2-digit',
                })}
              </time>
              <span>{event.message}</span>
            </div>
          ))}
      </div>
    </div>
  )
}
