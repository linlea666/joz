import type { CopyTradingConfig } from '../../types'
import { t } from '../../i18n/translations'

export function entryPolicyLabel(
  policy: string | undefined,
  language: string
): string {
  const key = {
    legacy: 'entryLegacy',
    market_reference_split: 'entrySplit',
    market_reference_participate: 'entryParticipate',
  }[policy || '']
  return key
    ? t(`copytrade.${key}`, language === 'zh' ? 'zh' : 'en')
    : policy || (language === 'zh' ? '未知' : 'Unknown')
}

export function EntryPolicyThresholds({
  config,
  language,
}: {
  config: Partial<CopyTradingConfig>
  language: string
}) {
  const zh = language === 'zh'
  const value = (n: number | undefined) =>
    Number.isFinite(n) ? `${n}%` : zh ? '未提供' : 'Unknown'
  return (
    <span className="block text-xs text-nofx-text-muted mt-2">
      {zh ? '配置的偏移阈值' : 'Configured deviation thresholds'}
      ：BTC/ETH {value(config.major_price_offset_pct)} ·{' '}
      {zh ? '山寨币' : 'Altcoins'} {value(config.altcoin_price_offset_pct)}。
      <br />
      {config.entry_policy === 'market_reference_participate'
        ? zh
          ? '优先参与：有利或同价 → 全额市价；不利价格 → 市价、参考价限价各 50% 风险，不使用上述阈值拦截（阈值为 0 也一样）。50% 是风险预算，并非数量各半。只适用于单参考价市价信号；双价格仍保留首笔偏移检查。止损、止盈方向、消息有效期及其他风控继续生效。首笔成交并确认保护后才提交第二笔。'
          : 'Participation first: favorable/equal → full market; adverse → 50% risk market + 50% risk reference limit, without the offset gate (including zero thresholds). Risk shares are not equal quantities. Single-reference market signals only; explicit two-price entries retain their offset gate. Stop/target direction, message validity and other risk checks remain. The second leg follows confirmed first-leg fills and protection.'
        : zh
          ? '启用按偏移分配时：有利或同价 → 全额市价；不利且在阈值内 → 市价、参考价限价各 50% 风险；超阈值 → 全额参考价限价。50% 是风险预算，并非数量各半；阈值为 0 时，不利价格均转参考价限价。'
          : 'With split policy selected: favorable/equal → full market; adverse within threshold → 50% risk market + 50% risk reference limit; beyond → full reference limit. Risk shares are not equal quantities. Zero threshold sends all adverse prices to reference limit.'}
    </span>
  )
}
