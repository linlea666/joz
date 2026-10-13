import type { CopyTradeSignal } from '../../types'
import { t, type Language } from '../../i18n/translations'

// A signal's raw status means processing, not exchange fill. Use only recorded
// order evidence and current linked recovery state, never current config.
export function copyTradeOutcome(
  signal: CopyTradeSignal,
  language: Language
): string {
  const zh = language === 'zh'
  if ((signal.instruction_results?.length || 0) > 1) {
    const status =
      signal.status === 'executed'
        ? t('copytrade.processed', language)
        : signal.status
    return zh
      ? `${signal.instruction_results!.length} 条指令 · ${status}（查看逐动作结果）`
      : `${signal.instruction_results!.length} instructions · ${status} (see per-action results)`
  }
  const recovery = [
    signal.recovery_error,
    ...(signal.action_results || []).map((a) => a.recovery_error),
  ]
    .filter(Boolean)
    .join(' ')
  if (recovery.includes('FLAT_RECOVERY_PENDING:'))
    return zh
      ? '空仓已观察到，恢复事项待核对'
      : 'Flat observed; recovery pending'
  if (
    recovery.includes('RECOVERY_PENDING:') ||
    recovery.includes('UNPROTECTED:')
  )
    return zh ? '保护／恢复待核对' : 'Protection / recovery pending'
  const errors = [
    signal.error_message,
    ...(signal.action_results || []).map((a) => a.error),
    ...(signal.instruction_results || []).map((a) => a.detail),
  ]
    .filter(Boolean)
    .join(' ')
  if (
    /CONTRACT_NOT_TRADING|CONTRACT_UNAVAILABLE/.test(errors) ||
    (errors.includes('contract capability:') &&
      /status=(?!TRADING\b)[A-Z_]+/.test(errors))
  ) {
    const status = errors.match(/status=([A-Z_]+)/)?.[1]
    return (
      (zh ? '已识别，合约不可交易' : 'Recognized; contract unavailable') +
      (status ? `：${status}` : '')
    )
  }
  if (errors.includes('CONTRACT_CHECK_FAILED'))
    return zh
      ? '合约状态查询失败，未提交'
      : 'Contract check unavailable; not submitted'
  if (signal.status === 'execution_wait')
    return zh ? '等待核对' : 'Awaiting reconciliation'
  if (signal.status === 'retry_wait')
    return t('copytrade.retryWaiting', language)
  if (signal.action === 'OPEN' || signal.action === 'ADD') {
    const legs = (signal.order_legs || []).filter((o) =>
      o.role.startsWith('ENTRY')
    )
    if (legs.some((o) => ['UNKNOWN', 'SUBMITTING'].includes(o.status)))
      return zh ? '提交结果待核对' : 'Submission pending reconciliation'
    if (legs.length) {
      if (legs.every((o) => o.status === 'FILLED'))
        return zh ? '入场已成交' : 'Entry filled'
      if (legs.some((o) => o.executed_qty > 0))
        return zh ? '入场部分成交' : 'Entry partially filled'
      if (legs.some((o) => o.status === 'NEW' && o.order_type === 'LIMIT'))
        return zh ? '已提交限价／等待成交' : 'Limit submitted / awaiting fill'
      if (
        legs.every((o) =>
          ['CANCELED', 'EXPIRED', 'ABANDONED', 'REJECTED'].includes(o.status)
        )
      )
        return zh ? '入场已终止，未成交' : 'Entry ended without fills'
    }
  }
  return signal.status === 'executed'
    ? t('copytrade.processed', language)
    : signal.status
}
