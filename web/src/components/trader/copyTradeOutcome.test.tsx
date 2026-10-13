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
