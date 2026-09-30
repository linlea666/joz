import { t, type Language } from '../../i18n/translations'
import { useCopyTradeProfiles } from '../../hooks/useCopyTradeProfiles'

export function CopyTradeRulesSnapshot({ snapshot, language }: { snapshot: string; language: Language }) {
  const { profiles } = useCopyTradeProfiles(true)
  let rules: Record<string, unknown> = {}
  try { rules = JSON.parse(snapshot) ?? {} } catch { /* historical unknown */ }
  const id = typeof rules.interpretation_profile === 'string' ? rules.interpretation_profile : ''
  return <details>
    <summary>{t('copytrade.rulesSnapshot', language)} · {profiles.find(p => p.id === id)?.name || id || (language === 'zh' ? '未知' : 'Unknown')}</summary>
    <pre className="whitespace-pre-wrap break-all text-xs">{snapshot}</pre>
  </details>
}
