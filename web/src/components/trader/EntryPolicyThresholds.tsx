import type { CopyTradingConfig } from '../../types'

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
      {zh
        ? '启用按偏移分配时：有利或同价 → 全额市价；不利且在阈值内 → 市价、参考价限价各 50% 风险；超阈值 → 全额参考价限价。50% 是风险预算，并非数量各半；阈值为 0 时，不利价格均转参考价限价。'
        : 'With split policy selected: favorable/equal → full market; adverse within threshold → 50% risk market + 50% risk reference limit; beyond → full reference limit. Risk shares are not equal quantities. Zero threshold sends all adverse prices to reference limit.'}
    </span>
  )
}
