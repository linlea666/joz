import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import { copyTradeOutcome } from './copyTradeOutcome'
import { EntryPolicyThresholds } from './EntryPolicyThresholds'
import type { CopyTradeSignal, CopyTradeOrderLeg } from '../../types'

const signal = {
  action: 'OPEN',
  status: 'executed',
  error_message: '',
} as CopyTradeSignal
const leg = {
  id: '1',
  role: 'ENTRY_1',
  order_type: 'LIMIT',
  status: 'NEW',
  quantity: 2499,
  executed_qty: 0,
} as CopyTradeOrderLeg

describe('exchange outcomes', () => {
  it('does not equate processed, submitted, partially filled and filled', () => {
    expect(copyTradeOutcome(signal, 'zh')).toBe('已处理')
    expect(
      copyTradeOutcome(
        {
          ...signal,
          status: 'failed',
          error_message: 'order status=REJECTED',
          recovery_error: 'entry rejected',
        },
        'zh'
      )
    ).toBe('failed')
    expect(copyTradeOutcome({ ...signal, order_legs: [leg] }, 'zh')).toBe(
      '已提交限价／等待成交'
    )
    expect(
      copyTradeOutcome(
        {
          ...signal,
          order_legs: [{ ...leg, status: 'PARTIALLY_FILLED', executed_qty: 5 }],
        },
        'zh'
      )
    ).toBe('入场部分成交')
    expect(
      copyTradeOutcome(
        {
          ...signal,
          order_legs: [{ ...leg, status: 'FILLED', executed_qty: 2499 }],
        },
        'zh'
      )
    ).toBe('入场已成交')
    expect(
      copyTradeOutcome(
        { ...signal, order_legs: [{ ...leg, status: 'UNKNOWN' }] },
        'zh'
      )
    ).toBe('提交结果待核对')
  })
  it('does not flatten mixed instruction results into a global fill or rejection', () => {
    const mixed = {
      ...signal,
      error_message: 'CONTRACT_NOT_TRADING: status=SETTLING',
      order_legs: [{ ...leg, status: 'FILLED' }],
      instruction_results: [
        { index: 0, action: 'OPEN', symbol: 'BTC', status: 'executed' },
        { index: 1, action: 'OPEN', symbol: 'RAY', status: 'failed' },
      ],
    }
    expect(copyTradeOutcome(mixed, 'zh')).toContain('2 条指令')
    expect(copyTradeOutcome(mixed, 'zh')).not.toContain('入场已成交')
  })
  it('gives recovery and contract evidence priority and preserves missing evidence', () => {
    expect(
      copyTradeOutcome(
        {
          ...signal,
          recovery_error: 'FLAT_RECOVERY_PENDING: history unavailable',
          order_legs: [{ ...leg, status: 'FILLED' }],
        },
        'zh'
      )
    ).toContain('空仓已观察到')
    expect(
      copyTradeOutcome(
        {
          ...signal,
          error_message: 'contract capability: unsupported (status=SETTLING)',
        },
        'zh'
      )
    ).toBe('已识别，合约不可交易：SETTLING')
    expect(
      copyTradeOutcome(
        { ...signal, error_message: 'CONTRACT_CHECK_FAILED: timeout' },
        'zh'
      )
    ).toBe('合约状态查询失败，未提交')
    expect(
      copyTradeOutcome(
        { ...signal, status: 'failed', error_message: 'network error' },
        'zh'
      )
    ).toBe('failed')
  })
  it('explains source execution-gate skips, including the legacy boundary code', () => {
    const skipped = { ...signal, status: 'skipped' } as CopyTradeSignal
    expect(
      copyTradeOutcome(
        { ...skipped, skip_reason: 'BEFORE_TRADER_ACTIVATION' },
        'zh'
      )
    ).toContain('卡片发布早于交易员本次启用时间')
    expect(
      copyTradeOutcome(
        { ...skipped, skip_reason: 'BEFORE_TRADER_ACTIVATION' },
        'en'
      )
    ).toContain("before this trader's current activation")
    expect(
      copyTradeOutcome(
        { ...skipped, skip_reason: 'BEFORE_LIVE_BOUNDARY' },
        'zh'
      )
    ).toContain('实盘生效时间')
    expect(
      copyTradeOutcome(
        { ...skipped, skip_reason: 'BEFORE_EXECUTION_BOUNDARY' },
        'zh'
      )
    ).toContain('旧版记录')
    expect(
      copyTradeOutcome({ ...skipped, skip_reason: 'CONFIG_REVOKED' }, 'en')
    ).toContain('configuration changed')
    // Admission revocation carries the gate code in its detail.
    expect(
      copyTradeOutcome(
        {
          ...skipped,
          skip_reason: 'ENTRY_ADMISSION_REVOKED',
          instruction_results: [
            {
              index: 0,
              action: 'OPEN',
              symbol: 'BTC',
              status: 'skipped',
              detail: 'unsubmitted entry no longer authorized: CONFIG_REVOKED',
            },
          ],
        },
        'zh'
      )
    ).toContain('跟单配置已变更')
    // Unrelated skips and executed signals are unchanged.
    expect(copyTradeOutcome({ ...skipped, skip_reason: 'EXPIRED' }, 'zh')).toBe(
      'skipped'
    )
    expect(copyTradeOutcome(signal, 'zh')).toBe('已处理')
  })
  it('shows configured offsets, risk meaning, zero and unknown without inventing defaults', () => {
    const { rerender } = render(
      <EntryPolicyThresholds
        config={{ major_price_offset_pct: 0.2, altcoin_price_offset_pct: 0.5 }}
        language="zh"
      />
    )
    expect(screen.getByText(/BTC\/ETH 0.2% · 山寨币 0.5%/)).toBeInTheDocument()
    expect(screen.getByText(/并非数量各半/)).toBeInTheDocument()
    rerender(
      <EntryPolicyThresholds
        config={{ major_price_offset_pct: 0 }}
        language="zh"
      />
    )
    expect(screen.getByText(/BTC\/ETH 0% · 山寨币 未提供/)).toBeInTheDocument()
  })
})
