import type { CopyTradingConfig } from '../../types'

// Form-side mirror of copytrader.CopyTradingConfig.Validate (Go). Keep the two
// in sync: the backend stays authoritative, this only prevents a save that the
// backend would reject (or, worse, a blank input silently becoming 0).

/** Numeric copy-trading fields edited as raw text so that blank stays blank. */
export const COPY_NUMERIC_FIELDS = [
  'risk_amount_usd',
  'max_position_notional_usd',
  'max_open_positions',
  'major_leverage',
  'altcoin_leverage',
  'major_price_offset_pct',
  'altcoin_price_offset_pct',
] as const

export type CopyNumericField = (typeof COPY_NUMERIC_FIELDS)[number]
export type CopyNumericInputs = Record<CopyNumericField, string>

/** Validation issue → i18n key under `copytrade.validation.*`. */
export type CopyValidationIssue =
  | 'required'
  | 'positive'
  | 'percentMax'
  | 'leverageRange'
  | 'positiveInteger'
  | 'offsetRequired'
  | 'nonNegative'
  | 'tpRatios'
  | 'channelNumeric'
  | 'sourceChannels'
  | 'chromaSources'
  | 'chromaAuthors'

export type CopyValidationField =
  | CopyNumericField
  | 'default_tp_ratios'
  | 'primary_channel_id'
  | 'source_channel_ids'
  | 'source_author_ids'

export type CopyValidationErrors = Partial<
  Record<CopyValidationField, CopyValidationIssue>
>

export const MAX_LEVERAGE = 125
export const MAX_SOURCE_CHANNELS = 20

export function numericInputsFromConfig(
  cfg: CopyTradingConfig
): CopyNumericInputs {
  const out = {} as CopyNumericInputs
  for (const f of COPY_NUMERIC_FIELDS) {
    const v = cfg[f]
    out[f] = typeof v === 'number' && Number.isFinite(v) ? String(v) : ''
  }
  return out
}

/** Strict decimal parse: '' / '1e' / 'abc' → null instead of 0 or NaN. */
export function parseDecimal(raw: string): number | null {
  const s = raw.trim()
  if (s === '') return null
  const n = Number(s)
  return Number.isFinite(n) ? n : null
}

/**
 * Mirrors Go ParseTPRatios: empty means the default 50,30,20; otherwise at
 * most 3 comma-separated values, each in (0,100], summing to at most 100.
 */
export function parseTPRatios(raw: string): number[] | null {
  const s = raw.trim()
  if (s === '') return [50, 30, 20]
  const parts = s.split(',')
  if (parts.length > 3) return null
  let total = 0
  const ratios: number[] = []
  for (const p of parts) {
    const v = parseDecimal(p)
    if (v === null || v <= 0 || v > 100) return null
    ratios.push(v)
    total += v
  }
  if (total > 100 + 1e-9) return null
  return ratios
}

const DISCORD_ID = /^\d+$/

function isDiscordID(id: string): boolean {
  return DISCORD_ID.test(id) && /[1-9]/.test(id)
}

export function validateCopyTradeForm(
  cfg: CopyTradingConfig,
  inputs: CopyNumericInputs
): CopyValidationErrors {
  const errors: CopyValidationErrors = {}
  const num = (f: CopyNumericField) => parseDecimal(inputs[f])

  const risk = num('risk_amount_usd')
  if (risk === null) errors.risk_amount_usd = 'required'
  else if (risk <= 0) errors.risk_amount_usd = 'positive'
  else if (cfg.risk_mode === 'percent' && risk > 100)
    errors.risk_amount_usd = 'percentMax'

  const notional = num('max_position_notional_usd')
  if (notional === null) errors.max_position_notional_usd = 'required'
  else if (notional <= 0) errors.max_position_notional_usd = 'positive'

  const maxPositions = num('max_open_positions')
  if (maxPositions === null) errors.max_open_positions = 'required'
  else if (!Number.isSafeInteger(maxPositions) || maxPositions <= 0)
    errors.max_open_positions = 'positiveInteger'

  for (const f of ['major_leverage', 'altcoin_leverage'] as const) {
    const v = num(f)
    if (v === null) errors[f] = 'required'
    else if (!Number.isInteger(v) || v < 1 || v > MAX_LEVERAGE)
      errors[f] = 'leverageRange'
  }

  for (const f of [
    'major_price_offset_pct',
    'altcoin_price_offset_pct',
  ] as const) {
    const v = num(f)
    if (v === null) errors[f] = 'offsetRequired'
    else if (v < 0) errors[f] = 'nonNegative'
  }

  if (parseTPRatios(cfg.default_tp_ratios ?? '') === null)
    errors.default_tp_ratios = 'tpRatios'

  const primary = (cfg.primary_channel_id ?? '').trim()
  if (primary === '') errors.primary_channel_id = 'required'
  else if (!isDiscordID(primary)) errors.primary_channel_id = 'channelNumeric'

  const sources = (cfg.source_channel_ids ?? []).filter(Boolean)
  if (
    sources.some((id) => !isDiscordID(id)) ||
    new Set(sources).size !== sources.length ||
    sources.length > MAX_SOURCE_CHANNELS
  )
    errors.source_channel_ids = 'sourceChannels'

  if (cfg.source_mode === 'chroma') {
    const authorIDs = (cfg.source_author_ids ?? []).filter(Boolean)
    const authorNames = (cfg.source_author_names ?? [])
      .map((n) => n.trim())
      .filter(Boolean)
    if (sources.length === 0 && !errors.source_channel_ids)
      errors.source_channel_ids = 'chromaSources'
    if (authorIDs.length === 0 && authorNames.length === 0)
      errors.source_author_ids = 'chromaAuthors'
  }

  return errors
}

/** Applies validated numeric inputs; call only when validation passed. */
export function applyNumericInputs(
  cfg: CopyTradingConfig,
  inputs: CopyNumericInputs
): CopyTradingConfig {
  const out = { ...cfg }
  for (const f of COPY_NUMERIC_FIELDS) {
    const v = parseDecimal(inputs[f])
    if (v !== null) out[f] = v
  }
  return out
}

export interface StatusReader {
  (traderId: string): Promise<{ source_applied?: boolean } | undefined>
}

/**
 * Polls the trader status endpoint until the restarted engine's Discord route
 * is applied. Returns false on timeout; errors count as "not yet".
 */
export async function waitForSourceApplied(
  traderId: string,
  readStatus: StatusReader,
  { attempts = 6, intervalMs = 1000 } = {}
): Promise<boolean> {
  for (let i = 0; i < attempts; i++) {
    if (i > 0) await new Promise((r) => setTimeout(r, intervalMs))
    try {
      const status = await readStatus(traderId)
      if (status?.source_applied === true) return true
    } catch {
      /* the engine may still be reloading */
    }
  }
  return false
}

/**
 * The saved channel profile would be replaced by a preset's recommendation.
 * An empty profile, or one still equal to another preset's untouched
 * recommendation, carries nothing the user wrote and needs no confirmation.
 */
export function presetOverwritesNotes(
  currentNotes: string | undefined,
  nextNotes: string | undefined,
  presetNotes: Array<string | undefined>
): boolean {
  const current = (currentNotes ?? '').trim()
  if (current === '' || nextNotes === undefined) return false
  if (current === nextNotes.trim()) return false
  return !presetNotes.some((n) => (n ?? '').trim() === current)
}
