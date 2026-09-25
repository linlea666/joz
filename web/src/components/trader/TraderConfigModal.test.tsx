import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TraderConfigModal } from './TraderConfigModal'
import type { AIModel, Exchange, TraderConfigData } from '../../types'

vi.mock('../../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: 'zh' }),
}))
vi.mock('../../lib/httpClient', () => ({
  httpClient: {
    get: vi.fn().mockResolvedValue({ success: true, data: { strategies: [] } }),
  },
}))
vi.mock('../../lib/api', () => ({ api: {} }))

afterEach(cleanup)

function setup(
  exchangeType = 'binance',
  policy = 'legacy',
  riskMode = 'by_loss'
) {
  const save = vi.fn().mockResolvedValue(undefined)
  const trader = {
    trader_name: 'TYLER',
    ai_model: 'model',
    exchange_id: 'exchange',
    trader_type: 'copy_trading',
    strategy_id: '',
    is_cross_margin: true,
    copy_trading_config: JSON.stringify({
      primary_channel_id: '123',
      entry_policy: policy,
      risk_mode: riskMode,
      risk_amount_usd: 15,
      altcoin_price_offset_pct: 0.2,
      auto_breakeven_after_tp: true,
    }),
  } as TraderConfigData
  render(
    <TraderConfigModal
      isOpen
      isEditMode
      onClose={() => {}}
      onSave={save}
      traderData={trader}
      availableModels={[
        { id: 'model', name: 'Model', enabled: true } as AIModel,
      ]}
      availableExchanges={[
        {
          id: 'exchange',
          name: 'Account',
          exchange_type: exchangeType,
          enabled: true,
        } as Exchange,
      ]}
    />
  )
  return save
}

describe('copy-trading opt-in settings', () => {
  it('keeps existing risk settings and requires explicit profile and split selection', async () => {
    const save = setup()
    expect(screen.getByLabelText('消息解读规则')).toHaveValue('default')
    expect(screen.getByLabelText('进场策略')).toHaveValue('legacy')
    fireEvent.change(screen.getByLabelText('消息解读规则'), {
      target: { value: 'tyler_v1' },
    })
    fireEvent.change(screen.getByLabelText('进场策略'), {
      target: { value: 'market_reference_split' },
    })
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    const cfg = JSON.parse(save.mock.calls[0][0].copy_trading_config)
    expect(cfg).toMatchObject({
      interpretation_profile: 'tyler_v1',
      entry_policy: 'market_reference_split',
      risk_amount_usd: 15,
      altcoin_price_offset_pct: 0.2,
      auto_breakeven_after_tp: true,
    })
  })

  it.each([
    ['okx', 'by_loss'],
    ['binance', 'fixed'],
  ])(
    'rejects saved split policy with %s / %s without silently changing it',
    async (exchange, riskMode) => {
      const save = setup(exchange, 'market_reference_split', riskMode)
      expect(screen.getByLabelText('进场策略')).toHaveValue(
        'market_reference_split'
      )
      expect(screen.getByRole('alert')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '保存修改' })).toBeDisabled()
      fireEvent.change(screen.getByLabelText('进场策略'), {
        target: { value: 'legacy' },
      })
      fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
      await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
      expect(
        JSON.parse(save.mock.calls[0][0].copy_trading_config).risk_mode
      ).toBe(riskMode)
    }
  )
})
