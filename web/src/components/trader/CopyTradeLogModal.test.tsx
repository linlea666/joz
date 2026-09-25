import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
  cleanup,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CopyTradeLogModal } from './CopyTradeLogModal'
import { CopyTradeExecutionDetails } from './CopyTradeExecutionDetails'
import { api } from '../../lib/api'
import type { CopyTradeEvent, CopyTradeSignal } from '../../types'

vi.mock('../../lib/api', () => ({
  api: {
    getCopyTradeEvents: vi.fn(),
    getCopyTradeSignals: vi.fn(),
    getCopyTradeAIRun: vi.fn(),
  },
}))

const signal = (id: string, tradeState?: string) =>
  ({
    id,
    ai_run_id: 1,
    trader_id: 'trader',
    trade_context_id: `ctx-${id}`,
    symbol: 'LINK',
    action: 'OPEN',
    direction: 'LONG',
    status: 'executed',
    message_timestamp: '2026-09-18T07:01:53Z',
    trade_state: tradeState,
  }) as CopyTradeSignal

const decision = {
  id: 1,
  trace_id: 'sig',
  signal_id: 'sig',
  trader_id: 'trader',
  channel_id: '123',
  message_id: 'msg',
  level: 'info',
  event: 'copytrade.entry.decision',
  message: 'market intent converted to limit',
  occurred_at: '2026-09-18T07:02:09Z',
  duration_ms: 0,
  created_at: '2026-09-18T07:02:09Z',
  context_json: JSON.stringify({
    symbol: 'LINKUSDT',
    source_order_type: 'MARKET',
    decision: {
      direction: 'LONG',
      reference_price: 11.073,
      market_price: 11.08,
      adverse_deviation_pct: 0.0632168,
      threshold_pct: 0,
      limit_to_market_within_threshold: true,
      order_type: 'LIMIT',
      entry_price: 11.073,
      reason: 'adverse_tolerance_disabled',
    },
  }),
} satisfies CopyTradeEvent

beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(api.getCopyTradeEvents).mockResolvedValue([])
  vi.mocked(api.getCopyTradeSignals).mockResolvedValue([])
  vi.mocked(api.getCopyTradeAIRun).mockRejectedValue(
    new Error('historical AI run removed')
  )
})
afterEach(cleanup)

const openSignals = async (signals: CopyTradeSignal[]) => {
  vi.mocked(api.getCopyTradeSignals).mockResolvedValue(signals)
  render(
    <CopyTradeLogModal
      traderId="trader"
      traderName="Test"
      language="zh"
      onClose={() => {}}
    />
  )
  fireEvent.click(screen.getByRole('button', { name: '信号', exact: true }))
  await screen.findAllByText('已处理')
}

describe('copy-trading execution visibility', () => {
  it('shows independent action outcomes and actual split fills', () => {
    const detailed = {
      ...signal('split', 'OPEN'),
      instruction_results: [
        { index: 0, action: 'REDUCE', symbol: 'BTC', status: 'executed' },
        {
          index: 1,
          action: 'UPDATE_SL',
          symbol: 'ETH',
          status: 'skipped',
          skip_reason: 'NEEDS_CONTEXT',
          detail: 'target is not unique',
        },
      ],
      action_results: [
        {
          id: 'action',
          symbol: 'BTCUSDT',
          direction: 'LONG',
          action: 'OPEN',
          status: 'done',
          trade_state: 'OPEN',
        },
      ],
      order_legs: [
        {
          id: 'first',
          symbol: 'BTCUSDT',
          direction: 'LONG',
          role: 'ENTRY_1',
          order_type: 'MARKET',
          status: 'FILLED',
          quantity: 0.9,
          executed_qty: 0.9,
          avg_price: 101,
        },
        {
          id: 'second',
          symbol: 'BTCUSDT',
          direction: 'LONG',
          role: 'ENTRY_2',
          order_type: 'LIMIT',
          status: 'PARTIALLY_FILLED',
          quantity: 1,
          executed_qty: 0.2,
          avg_price: 100,
        },
      ],
    } as CopyTradeSignal
    render(
      <CopyTradeExecutionDetails
        events={[]}
        language="zh"
        expectsEntry={false}
        signal={detailed}
      />
    )
    expect(screen.getByText(/target is not unique/)).toBeInTheDocument()
    expect(screen.getByText(/ENTRY_1.*FILLED/)).toBeInTheDocument()
    expect(screen.getByText(/ENTRY_2.*PARTIALLY_FILLED/)).toBeInTheDocument()
    expect(screen.getByText(/0.2 \/ 1/)).toBeInTheDocument()
    expect(screen.getByText(/OPEN.*done.*已开仓/)).toBeInTheDocument()
  })

  it('separates processing from pending, open and expired trade states', async () => {
    await openSignals([
      signal('pending', 'ENTRY_PENDING'),
      signal('open', 'OPEN'),
      signal('expired', 'EXPIRED'),
      signal('multi'),
    ])
    expect(screen.getAllByText('已处理')).toHaveLength(4)
    expect(screen.getByText('关联交易: 等待进场成交')).toBeInTheDocument()
    expect(screen.getByText('关联交易: 已开仓')).toBeInTheDocument()
    expect(screen.getByText('关联交易: 进场超时撤单')).toBeInTheDocument()
    const rows = screen.getAllByRole('row')
    expect(within(rows[4]).queryByText(/关联交易/)).not.toBeInTheDocument()
    expect(screen.getAllByText('已处理')[0]).toHaveAttribute(
      'title',
      expect.stringContaining('不代表订单已成交')
    )
  })

  it('shows the actual zero threshold and lifecycle trace even without an AI record', async () => {
    const expired = {
      ...decision,
      id: 2,
      trace_id: 'reconcile-ctx-sig',
      signal_id: '',
      event: 'copytrade.trade.expired',
      message: 'LINKUSDT entry not filled within 240m, cancelled',
      context_json: '',
    }
    vi.mocked(api.getCopyTradeEvents).mockImplementation(
      async (_trader, _limit, _offset, _start, _end, trace) =>
        trace === 'sig'
          ? [decision]
          : trace === 'reconcile-ctx-sig'
            ? [expired]
            : []
    )
    await openSignals([signal('sig', 'EXPIRED')])
    fireEvent.click(screen.getByRole('button', { name: '查看 AI 与执行详情' }))
    expect(
      await screen.findByText(
        '阈值为 0，不容许不利价差，按参考价挂限价单等待。'
      )
    ).toBeInTheDocument()
    expect(screen.getByText('0%')).toBeInTheDocument()
    expect(screen.getByText('11.08')).toBeInTheDocument()
    expect(screen.getByText('市价')).toBeInTheDocument()
    expect(screen.getByText('限价')).toBeInTheDocument()
    expect(screen.getByText(expired.message)).toBeInTheDocument()
    expect(
      await screen.findByText('该信号没有 AI 调用记录。')
    ).toBeInTheDocument()
  })

  it('does not attach the last context of a multi-instruction signal and retries failed event reads', async () => {
    await openSignals([signal('multi')])
    vi.mocked(api.getCopyTradeEvents).mockRejectedValueOnce(
      new Error('temporary failure')
    )
    fireEvent.click(screen.getByRole('button', { name: '查看 AI 与执行详情' }))
    expect(
      await screen.findByText('执行详情加载失败，请收起后重新展开重试。')
    ).toBeInTheDocument()
    expect(
      vi
        .mocked(api.getCopyTradeEvents)
        .mock.calls.some((args) => args[5] === 'reconcile-ctx-multi')
    ).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: '收起详情' }))
    fireEvent.click(screen.getByRole('button', { name: '查看 AI 与执行详情' }))
    await waitFor(() =>
      expect(
        screen.queryByText('执行详情加载失败，请收起后重新展开重试。')
      ).not.toBeInTheDocument()
    )
    expect(
      await screen.findByText('没有记录进场决策快照，无法还原当时价差。')
    ).toBeInTheDocument()
  })

  it('does not manufacture decision values for old or malformed events', () => {
    render(
      <CopyTradeExecutionDetails
        events={[
          {
            ...decision,
            context_json: '{broken',
            message: 'legacy entry submitted',
          },
        ]}
        language="zh"
        expectsEntry
      />
    )
    expect(
      screen.getByText('没有记录进场决策快照，无法还原当时价差。')
    ).toBeInTheDocument()
    expect(screen.getByText('legacy entry submitted')).toBeInTheDocument()
    expect(screen.queryByText('0%')).not.toBeInTheDocument()
  })
})
