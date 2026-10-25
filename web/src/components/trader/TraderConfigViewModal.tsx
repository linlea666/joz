import {
  EntryPolicyThresholds,
  entryPolicyLabel,
} from './EntryPolicyThresholds'
import { useCopyTradeProfiles } from '../../hooks/useCopyTradeProfiles'
import type { CopyTradingConfig, Exchange, TraderConfigData } from '../../types'
import { DEFAULT_COPY_TRADING_CONFIG } from '../../types'
import { t } from '../../i18n/translations'
import { useLanguage } from '../../contexts/LanguageContext'
import { PunkAvatar, getTraderAvatar } from '../common/PunkAvatar'
import { getShortName } from './utils'

/** Effective values: the backend applies the same defaults to absent fields. */
function parseEffectiveCopyConfig(raw?: string): CopyTradingConfig | null {
  try {
    return { ...DEFAULT_COPY_TRADING_CONFIG, ...JSON.parse(raw || '{}') }
  } catch {
    return null // show no guessed settings
  }
}

type ViewTraderData = TraderConfigData & {
  /** Exchange type reported by the public config endpoint. */
  exchange?: string
}

/** Exchange name for display; never a truncated account UUID. */
function exchangeDisplayName(
  traderData: ViewTraderData,
  exchanges: Exchange[] = []
): string {
  const account = exchanges.find((e) => e.id === traderData.exchange_id)
  if (account) {
    const base = (account.name || account.exchange_type || '').toUpperCase()
    return account.account_name ? `${base} - ${account.account_name}` : base
  }
  if (traderData.exchange) return traderData.exchange.toUpperCase()
  return '—'
}

interface TraderConfigViewModalProps {
  isOpen: boolean
  onClose: () => void
  traderData?: ViewTraderData | null
  /** The viewer's exchange accounts, used to name the bound account. */
  exchanges?: Exchange[]
}

export function TraderConfigViewModal({
  isOpen,
  onClose,
  traderData,
  exchanges,
}: TraderConfigViewModalProps) {
  const { language } = useLanguage()
  const isCopyTrading = traderData?.trader_type === 'copy_trading'
  const { profiles } = useCopyTradeProfiles(isOpen && isCopyTrading)
  if (!isOpen || !traderData) return null

  const copyConfig = isCopyTrading
    ? parseEffectiveCopyConfig(traderData.copy_trading_config)
    : null

  const InfoRow = ({
    label,
    value,
  }: {
    label: string
    value: string | number | boolean
  }) => (
    <div className="flex justify-between items-start py-2 border-b border-nofx-gold/20 last:border-b-0">
      <span className="text-sm text-nofx-text-muted font-medium">{label}</span>
      <span className="text-sm text-nofx-text font-mono text-right">
        {typeof value === 'boolean'
          ? value
            ? t('traderConfigView.yes', language)
            : t('traderConfigView.no', language)
          : value}
      </span>
    </div>
  )

  const riskModeLabel = (mode: CopyTradingConfig['risk_mode']) =>
    t(
      `copytrade.${({ by_loss: 'riskByLoss', percent: 'riskPercent', fixed: 'riskFixed' } as const)[mode] ?? 'riskByLoss'}`,
      language
    )

  const notional = (cfg: CopyTradingConfig) =>
    cfg.max_position_notional_usd > 0
      ? `$${cfg.max_position_notional_usd.toLocaleString()}`
      : `$${DEFAULT_COPY_TRADING_CONFIG.max_position_notional_usd.toLocaleString()}${t('copytrade.defaultValueSuffix', language)}`

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black bg-opacity-50 backdrop-blur-sm">
      <div
        className="bg-nofx-bg-lighter border border-nofx-gold/20 rounded-xl shadow-2xl max-w-2xl w-full mx-4 max-h-[90vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-6 border-b border-nofx-gold/20 bg-nofx-bg-lighter">
          <div className="flex items-center gap-3">
            <PunkAvatar
              seed={getTraderAvatar(
                traderData.trader_id || '',
                traderData.trader_name
              )}
              size={48}
              className="rounded-lg"
            />
            <div>
              <h2 className="text-xl font-bold text-nofx-text">
                {t('traderConfigView.traderConfig', language)}
              </h2>
              <p className="text-sm text-nofx-text-muted mt-1">
                {t('traderConfigView.configInfo', language, {
                  name: traderData.trader_name,
                })}
              </p>
            </div>
          </div>
          <div className="flex items-center gap-2">
            {/* Running Status */}
            <div
              className="px-3 py-1 rounded-full text-xs font-bold flex items-center gap-1"
              style={
                traderData.is_running
                  ? { background: 'rgba(46, 139, 87, 0.1)', color: '#2E8B57' }
                  : { background: 'rgba(214, 67, 58, 0.1)', color: '#D6433A' }
              }
            >
              <span>{traderData.is_running ? '●' : '○'}</span>
              {traderData.is_running
                ? t('traderConfigView.running', language)
                : t('traderConfigView.stopped', language)}
            </div>
            <button
              onClick={onClose}
              className="w-8 h-8 rounded-lg text-nofx-text-muted hover:text-nofx-text hover:bg-nofx-bg-deeper transition-colors flex items-center justify-center"
            >
              ✕
            </button>
          </div>
        </div>

        {/* Content */}
        <div className="p-6 space-y-6">
          {/* Basic Info */}
          <div className="bg-nofx-bg border border-nofx-gold/20 rounded-lg p-5">
            <h3 className="text-lg font-semibold text-nofx-text mb-4 flex items-center gap-2">
              {'🤖 ' + t('traderConfigView.basicInfo', language)}
            </h3>
            <div className="space-y-3">
              <InfoRow
                label={t('traderConfigView.traderName', language)}
                value={traderData.trader_name}
              />
              <InfoRow
                label={
                  isCopyTrading
                    ? t('copytrade.parseModel', language)
                    : t('traderConfigView.aiModel', language)
                }
                value={getShortName(traderData.ai_model || '').toUpperCase()}
              />
              <InfoRow
                label={t('traderConfigView.exchange', language)}
                value={exchangeDisplayName(traderData, exchanges)}
              />
              {typeof traderData.initial_balance === 'number' && (
                <InfoRow
                  label={t('traderConfigView.initialBalance', language)}
                  value={`$${traderData.initial_balance.toLocaleString()}`}
                />
              )}
              {/* Copy trading neither scans nor sets the margin mode. */}
              {!isCopyTrading && (
                <>
                  <InfoRow
                    label={t('traderConfigView.marginMode', language)}
                    value={
                      traderData.is_cross_margin
                        ? t('traderConfigView.crossMargin', language)
                        : t('traderConfigView.isolatedMargin', language)
                    }
                  />
                  <InfoRow
                    label={t('traderConfigView.scanIntervalLabel', language)}
                    value={t('traderConfigView.scanInterval', language, {
                      minutes: traderData.scan_interval_minutes || 15,
                    })}
                  />
                </>
              )}
            </div>
          </div>

          {copyConfig && (
            <div
              className="bg-nofx-bg border border-nofx-gold/20 rounded-lg p-5"
              data-testid="copy-config-view"
            >
              <h3 className="text-lg font-semibold text-nofx-text mb-4">
                {t('copytrade.viewTitle', language)}
              </h3>
              <InfoRow
                label={t('copytrade.sourceMode', language)}
                value={copyConfig.source_mode || 'channel'}
              />
              <InfoRow
                label={t('copytrade.sourceChannels', language)}
                value={(copyConfig.source_channel_ids?.length
                  ? copyConfig.source_channel_ids
                  : [copyConfig.primary_channel_id]
                ).join(', ')}
              />
              <InfoRow
                label={t('copytrade.authorIds', language)}
                value={(copyConfig.source_author_ids || []).join(', ') || '—'}
              />
              <InfoRow
                label={t('copytrade.authorNames', language)}
                value={(copyConfig.source_author_names || []).join(', ') || '—'}
              />
              <p className="text-xs text-nofx-text-muted mt-1 mb-2">
                {t('copytrade.authorFilterBoth', language)}
              </p>
              <InfoRow
                label={t('copytrade.interpretationProfile', language)}
                value={
                  profiles.find(
                    (p) =>
                      p.id === (copyConfig.interpretation_profile || 'default')
                  )?.name ||
                  copyConfig.interpretation_profile ||
                  t('copytrade.profileDefault', language)
                }
              />
              <InfoRow
                label={t('copytrade.riskMode', language)}
                value={riskModeLabel(copyConfig.risk_mode)}
              />
              <InfoRow
                label={
                  copyConfig.risk_mode === 'percent'
                    ? t('copytrade.riskAmountPct', language)
                    : t('copytrade.riskAmountUsd', language)
                }
                value={
                  copyConfig.risk_mode === 'percent'
                    ? `${copyConfig.risk_amount_usd}%`
                    : `$${copyConfig.risk_amount_usd}`
                }
              />
              <InfoRow
                label={t('copytrade.majorLeverage', language)}
                value={`${copyConfig.major_leverage}x`}
              />
              <InfoRow
                label={t('copytrade.altLeverage', language)}
                value={`${copyConfig.altcoin_leverage}x`}
              />
              <InfoRow
                label={t('copytrade.maxNotional', language)}
                value={notional(copyConfig)}
              />
              <InfoRow
                label={t('copytrade.maxPositions', language)}
                value={copyConfig.max_open_positions}
              />
              <InfoRow
                label={t('copytrade.tpRatios', language)}
                value={copyConfig.default_tp_ratios || '50,30,20'}
              />
              <InfoRow
                label={t('copytrade.entryTimeout', language)}
                value={
                  copyConfig.entry_timeout_minutes === 0
                    ? t('copytrade.entryTimeoutNever', language)
                    : t('copytrade.minutesValue', language, {
                        minutes: copyConfig.entry_timeout_minutes,
                      })
                }
              />
              <InfoRow
                label={t('copytrade.majorOffset', language)}
                value={`${copyConfig.major_price_offset_pct}%`}
              />
              <InfoRow
                label={t('copytrade.altOffset', language)}
                value={`${copyConfig.altcoin_price_offset_pct}%`}
              />
              <InfoRow
                label={t('copytrade.entryPolicy', language)}
                value={entryPolicyLabel(
                  copyConfig.entry_policy || 'legacy',
                  language
                )}
              />
              <EntryPolicyThresholds config={copyConfig} language={language} />
              <InfoRow
                label={t('copytrade.dualPriceLabel', language)}
                value={t(
                  `copytrade.${({ legacy: 'dualPriceLegacy', range: 'dualPriceRange', market_then_limit: 'dualPriceSplit', reject: 'dualPriceReject' } as const)[copyConfig.market_dual_price_mode || 'legacy']}`,
                  language
                )}
              />
              <InfoRow
                label={t('copytrade.reduceDefault', language)}
                value={`${copyConfig.default_reduce_ratio ?? 50}%`}
              />
              <InfoRow
                label={t('copytrade.paused', language)}
                value={!!copyConfig.paused}
              />
              <InfoRow
                label={t('copytrade.dupProtection', language)}
                value={t('copytrade.alwaysOn', language)}
              />
              <p className="text-xs text-nofx-text-muted mt-2">
                {t('copytrade.rulesHint', language)}
              </p>
            </div>
          )}

          {/* Strategy Info - only show if strategy is bound */}
          {traderData.strategy_id && (
            <div className="bg-nofx-bg border border-nofx-gold/20 rounded-lg p-5">
              <h3 className="text-lg font-semibold text-nofx-text mb-4 flex items-center gap-2">
                {'📋 ' + t('traderConfigView.strategyUsed', language)}
              </h3>
              <div className="space-y-3">
                <InfoRow
                  label={t('traderConfigView.strategyName', language)}
                  value={traderData.strategy_name || traderData.strategy_id}
                />
              </div>
            </div>
          )}
        </div>

        {/* Footer */}
        <div className="flex justify-end p-6 border-t border-nofx-gold/20 bg-nofx-bg-lighter">
          <button
            onClick={onClose}
            className="px-6 py-3 bg-nofx-bg-deeper text-nofx-text rounded-lg hover:bg-nofx-bg transition-all duration-200 border border-nofx-gold/20"
          >
            {t('traderConfigView.close', language)}
          </button>
        </div>
      </div>
    </div>
  )
}
