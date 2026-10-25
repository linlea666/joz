import { describe, expect, it, vi } from 'vitest'
import { DEFAULT_COPY_TRADING_CONFIG } from '../../types'
import type { CopyTradingConfig } from '../../types'
import {
  applyNumericInputs,
  numericInputsFromConfig,
  parseTPRatios,
  presetOverwritesNotes,
  validateCopyTradeForm,
  waitForSourceApplied,
} from './copyTradeForm'

const base: CopyTradingConfig = {
  ...DEFAULT_COPY_TRADING_CONFIG,
  primary_channel_id: '1385639865194315949',
}

function check(
  cfg: Partial<CopyTradingConfig> = {},
  inputs: Partial<ReturnType<typeof numericInputsFromConfig>> = {}
) {
  const c = { ...base, ...cfg }
  return validateCopyTradeForm(c, { ...numericInputsFromConfig(c), ...inputs })
}

describe('validateCopyTradeForm mirrors the backend Validate', () => {
  it('accepts the defaults', () => {
    expect(check()).toEqual({})
  })

  it('keeps blank numeric inputs blank and requires them', () => {
    const errors = check(
      {},
      {
        risk_amount_usd: '',
        max_position_notional_usd: ' ',
        max_open_positions: '',
        major_leverage: '',
        altcoin_leverage: '',
      }
    )
    expect(errors).toMatchObject({
      risk_amount_usd: 'required',
      max_position_notional_usd: 'required',
      max_open_positions: 'required',
      major_leverage: 'required',
      altcoin_leverage: 'required',
    })
  })

  it('explains that an empty offset threshold is not 0', () => {
    expect(
      check({}, { major_price_offset_pct: '', altcoin_price_offset_pct: '' })
    ).toEqual({
      major_price_offset_pct: 'offsetRequired',
      altcoin_price_offset_pct: 'offsetRequired',
    })
    expect(check({}, { major_price_offset_pct: '0' })).toEqual({})
    expect(check({}, { altcoin_price_offset_pct: '-0.1' })).toEqual({
      altcoin_price_offset_pct: 'nonNegative',
    })
  })

  it.each([
    ['risk_amount_usd', '0', 'positive'],
    ['risk_amount_usd', '-5', 'positive'],
    ['max_position_notional_usd', '0', 'positive'],
    ['max_open_positions', '0', 'positiveInteger'],
    ['max_open_positions', '1.5', 'positiveInteger'],
    ['major_leverage', '0', 'leverageRange'],
    ['major_leverage', '126', 'leverageRange'],
    ['altcoin_leverage', '2.5', 'leverageRange'],
  ] as const)('%s=%s → %s', (field, value, issue) => {
    expect(check({}, { [field]: value })[field]).toBe(issue)
  })

  it('has no upper bound on open positions, like the backend', () => {
    expect(check({}, { max_open_positions: '50' })).toEqual({})
  })

  it('accepts leverage at the bounds', () => {
    expect(check({}, { major_leverage: '1', altcoin_leverage: '125' })).toEqual(
      {}
    )
  })

  it('limits the percentage risk mode to 100', () => {
    expect(check({ risk_mode: 'percent' }, { risk_amount_usd: '100' })).toEqual(
      {}
    )
    expect(
      check({ risk_mode: 'percent' }, { risk_amount_usd: '101' })
        .risk_amount_usd
    ).toBe('percentMax')
    expect(check({ risk_mode: 'fixed' }, { risk_amount_usd: '500' })).toEqual(
      {}
    )
  })

  it.each(['', '100', '50,30,20', '33.3, 33.3, 33.3'])(
    'accepts TP ratios %j',
    (ratios) => {
      expect(check({ default_tp_ratios: ratios })).toEqual({})
    }
  )

  it.each(['25,25,25,25', '60,50', '0,50', '101', 'a,b', '50,,20'])(
    'rejects TP ratios %j',
    (ratios) => {
      expect(check({ default_tp_ratios: ratios }).default_tp_ratios).toBe(
        'tpRatios'
      )
    }
  )

  it('requires Chroma sources and an author filter', () => {
    expect(check({ source_mode: 'chroma' })).toEqual({
      source_channel_ids: 'chromaSources',
      source_author_ids: 'chromaAuthors',
    })
    expect(
      check({
        source_mode: 'chroma',
        source_channel_ids: ['201'],
        source_author_names: ['JONZi'],
      })
    ).toEqual({})
    expect(
      check({
        source_mode: 'chroma',
        source_channel_ids: ['201'],
        source_author_ids: ['77'],
      })
    ).toEqual({})
    // Blank entries left while typing do not count as a filter.
    expect(
      check({
        source_mode: 'chroma',
        source_channel_ids: ['201', ''],
        source_author_names: [' '],
      }).source_author_ids
    ).toBe('chromaAuthors')
  })

  it('rejects malformed channel IDs', () => {
    expect(check({ primary_channel_id: '' }).primary_channel_id).toBe(
      'required'
    )
    expect(check({ primary_channel_id: 'abc' }).primary_channel_id).toBe(
      'channelNumeric'
    )
    expect(check({ primary_channel_id: '0' }).primary_channel_id).toBe(
      'channelNumeric'
    )
    expect(
      check({ source_channel_ids: ['201', '201'] }).source_channel_ids
    ).toBe('sourceChannels')
    expect(check({ source_channel_ids: ['x1'] }).source_channel_ids).toBe(
      'sourceChannels'
    )
  })
})

describe('numeric input helpers', () => {
  it('round-trips config numbers and applies edited text', () => {
    const inputs = numericInputsFromConfig(base)
    expect(inputs.max_position_notional_usd).toBe('30000')
    const applied = applyNumericInputs(base, {
      ...inputs,
      risk_amount_usd: '12.5',
      major_price_offset_pct: '0',
    })
    expect(applied.risk_amount_usd).toBe(12.5)
    expect(applied.major_price_offset_pct).toBe(0)
    expect(applied.altcoin_leverage).toBe(base.altcoin_leverage)
  })

  it('mirrors Go ParseTPRatios', () => {
    expect(parseTPRatios('')).toEqual([50, 30, 20])
    expect(parseTPRatios('70,30')).toEqual([70, 30])
    expect(parseTPRatios('70,40')).toBeNull()
  })
})

describe('presetOverwritesNotes', () => {
  const presets = ['TYLER 画像', 'CMM 画像']
  it('asks only when user-written notes would be replaced', () => {
    expect(presetOverwritesNotes('', 'CMM 画像', presets)).toBe(false)
    expect(presetOverwritesNotes('我的画像', undefined, presets)).toBe(false)
    expect(presetOverwritesNotes('CMM 画像', 'CMM 画像', presets)).toBe(false)
    expect(presetOverwritesNotes('TYLER 画像', 'CMM 画像', presets)).toBe(false)
    expect(presetOverwritesNotes('我的画像', 'CMM 画像', presets)).toBe(true)
  })
})

describe('waitForSourceApplied', () => {
  it('polls until the route is applied', async () => {
    const read = vi
      .fn()
      .mockResolvedValueOnce({ source_applied: false })
      .mockRejectedValueOnce(new Error('reloading'))
      .mockResolvedValueOnce({ source_applied: true })
    await expect(
      waitForSourceApplied('t1', read, { attempts: 5, intervalMs: 1 })
    ).resolves.toBe(true)
    expect(read).toHaveBeenCalledTimes(3)
    expect(read).toHaveBeenCalledWith('t1')
  })

  it('gives up after the attempt budget', async () => {
    const read = vi.fn().mockResolvedValue({ source_applied: false })
    await expect(
      waitForSourceApplied('t1', read, { attempts: 3, intervalMs: 1 })
    ).resolves.toBe(false)
    expect(read).toHaveBeenCalledTimes(3)
  })
})
