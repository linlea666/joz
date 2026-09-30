import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TraderConfigModal } from './TraderConfigModal'
import type { AIModel, Exchange, TraderConfigData, CopyTradingConfig } from '../../types'

vi.mock('../../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: 'zh' }),
}))
vi.mock('../../lib/httpClient', () => ({
  httpClient: {
    get: vi.fn().mockResolvedValue({ success: true, data: { strategies: [] } }),
  },
}))
vi.mock('../../lib/api', () => ({ api: {
  getCopyTradeProfiles: vi.fn().mockResolvedValue([
    { id: 'default', name: '默认', description: '通用解释', recommended_config: {} },
    { id: 'tyler_v1', name: 'TYLER', description: '特殊管理话术', recommended_config: { market_dual_price_mode: 'legacy', default_reduce_ratio: 50, channel_notes: 'TYLER 画像' } },
    { id: 'cmm_v1', name: 'CMM', description: '新信号卡与操作通知', recommended_config: { market_dual_price_mode: 'market_then_limit', default_reduce_ratio: 50, channel_notes: 'CMM 画像' } },
    { id: 'jonzi_v1', name: 'jonzi', description: '原卡编辑与中英重复', recommended_config: { market_dual_price_mode: 'legacy', default_reduce_ratio: 50, channel_notes: 'jonzi 画像' } },
  ]),
} }))

afterEach(cleanup)

function setup(
  exchangeType = 'binance',
  policy = 'legacy',
  riskMode = 'by_loss',
  entryTimeout?: number,
  extra: Partial<CopyTradingConfig> = {}
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
      entry_timeout_minutes: entryTimeout,
      ...extra,
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
    expect(screen.getByLabelText('作者风格预设')).toHaveValue('default')
    expect(screen.getByLabelText('单参考价市价信号的执行方式（市价 A）')).toHaveValue('legacy')
    await screen.findByRole('option', { name: 'TYLER' })
    fireEvent.change(screen.getByLabelText('作者风格预设'), {
      target: { value: 'tyler_v1' },
    })
    fireEvent.change(screen.getByLabelText('单参考价市价信号的执行方式（市价 A）'), {
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

  it('saves independent message rules without changing risk or the original split policy', async () => {
    const save = setup()
    const dual = screen.getByLabelText('双价格文案含义（市价 A—B）')
    const reduce = screen.getByLabelText('明确减仓但未注明比例（剩余仓位 %）')
    expect(dual).toHaveValue('legacy')
    expect(reduce).toHaveValue(50)
    fireEvent.change(dual, { target: { value: 'market_then_limit' } })
    fireEvent.change(reduce, { target: { value: '25' } })
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(JSON.parse(save.mock.calls[0][0].copy_trading_config)).toMatchObject(
      {
        market_dual_price_mode: 'market_then_limit',
        default_reduce_ratio: 25,
        entry_policy: 'legacy',
        risk_amount_usd: 15,
      }
    )
  })

  it.each(['0', '-1', '101'])(
    'rejects an invalid default reduction %s',
    (value) => {
      setup()
      fireEvent.change(
        screen.getByLabelText('明确减仓但未注明比例（剩余仓位 %）'),
        { target: { value } }
      )
      expect(screen.getByRole('button', { name: '保存修改' })).toBeDisabled()
    }
  )

  it.each([30, 0])('saves an explicit entry lifetime of %i minutes', async (minutes) => {
    const save = setup()
    const timeout = screen.getByLabelText('未成交挂单有效期（分钟）')
    expect(timeout).toHaveValue(240)
    fireEvent.change(timeout, { target: { value: String(minutes) } })
    if (minutes === 0) {
      expect(screen.getByRole('status')).toHaveTextContent('已关闭自动过期')
    }
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(JSON.parse(save.mock.calls[0][0].copy_trading_config)).toMatchObject({
      entry_timeout_minutes: minutes,
      risk_amount_usd: 15,
      altcoin_price_offset_pct: 0.2,
      open_signal_ttl_seconds: 300,
      management_signal_ttl_seconds: 1800,
    })
  })

  it.each([0, 60])('preserves a saved entry lifetime of %i minutes', async (minutes) => {
    const save = setup('binance', 'legacy', 'by_loss', minutes)
    expect(screen.getByLabelText('未成交挂单有效期（分钟）')).toHaveValue(minutes)
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(JSON.parse(save.mock.calls[0][0].copy_trading_config).entry_timeout_minutes).toBe(minutes)
  })

  it.each(['', '-1', '1.5', '1e309', '153722868'])(
    'blocks invalid lifetime %j rather than silently disabling expiry', (value) => {
      const save = setup()
      fireEvent.change(screen.getByLabelText('未成交挂单有效期（分钟）'), { target: { value } })
      expect(screen.getByRole('alert')).toBeInTheDocument()
      expect(screen.queryByRole('status')).not.toBeInTheDocument()
      const button = screen.getByRole('button', { name: '保存修改' })
      expect(button).toBeDisabled()
      fireEvent.click(button)
      expect(save).not.toHaveBeenCalled()
      fireEvent.change(screen.getByLabelText('未成交挂单有效期（分钟）'), { target: { value: '240' } })
      expect(button).toBeEnabled()
    }
  )

  it.each([
    ['okx', 'by_loss'],
    ['binance', 'fixed'],
  ])(
    'rejects saved split policy with %s / %s without silently changing it',
    async (exchange, riskMode) => {
      const save = setup(exchange, 'market_reference_split', riskMode)
      expect(screen.getByLabelText('单参考价市价信号的执行方式（市价 A）')).toHaveValue(
        'market_reference_split'
      )
      expect(screen.getByRole('alert')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '保存修改' })).toBeDisabled()
      fireEvent.change(screen.getByLabelText('单参考价市价信号的执行方式（市价 A）'), {
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


describe('author preset recommendations', () => {
  it.each(['cmm_v1', 'tyler_v1', 'jonzi_v1'])('applies %s only to editable recommended fields', async (profile) => {
    const save = setup('binance', 'market_reference_split')
    await screen.findByRole('option', { name: 'CMM' })
    fireEvent.change(screen.getByLabelText('作者风格预设'), { target: { value: profile } })
    expect(save).not.toHaveBeenCalled()
    expect(screen.getByLabelText('单参考价市价信号的执行方式（市价 A）')).toHaveValue('market_reference_split')
    expect(screen.getByLabelText('双价格文案含义（市价 A—B）')).toHaveValue(profile === 'cmm_v1' ? 'market_then_limit' : 'legacy')
    // The recommendation remains editable before save.
    fireEvent.change(screen.getByLabelText('明确减仓但未注明比例（剩余仓位 %）'), { target: { value: '25' } })
    expect(screen.getByText(/使用剩余仓位的 25%/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(save).toHaveBeenCalledOnce())
    const cfg = JSON.parse(save.mock.calls[0][0].copy_trading_config)
    expect(cfg).toMatchObject({ interpretation_profile: profile, default_reduce_ratio: 25, entry_policy: 'market_reference_split', risk_amount_usd: 15, altcoin_price_offset_pct: 0.2 })
    expect(cfg.channel_notes).toContain(profile === 'cmm_v1' ? 'CMM' : profile === 'tyler_v1' ? 'TYLER' : 'jonzi')
    expect(save.mock.calls[0][0].exchange_id).toBe('exchange')
  })

  it('does not apply recommendations when opening saved settings or switching to default', async () => {
    const save = setup('binance', 'market_reference_split', 'by_loss', 800, {
      interpretation_profile: 'cmm_v1', market_dual_price_mode: 'range', default_reduce_ratio: 35, channel_notes: '我的画像',
    })
    await screen.findByRole('option', { name: 'CMM' })
    expect(screen.getByLabelText('作者风格预设')).toHaveValue('cmm_v1')
    expect(screen.getByLabelText('双价格文案含义（市价 A—B）')).toHaveValue('range')
    expect(screen.getByDisplayValue('我的画像')).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('作者风格预设'), { target: { value: 'default' } })
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
    await waitFor(() => expect(save).toHaveBeenCalledOnce())
    expect(JSON.parse(save.mock.calls[0][0].copy_trading_config)).toMatchObject({ interpretation_profile: 'default', market_dual_price_mode: 'range', default_reduce_ratio: 35, channel_notes: '我的画像', entry_policy: 'market_reference_split', entry_timeout_minutes: 800 })
  })

  it('cancelling a preset edit does not save it', async () => {
    const save = setup()
    await screen.findByRole('option', { name: 'CMM' })
    fireEvent.change(screen.getByLabelText('作者风格预设'), { target: { value: 'cmm_v1' } })
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    expect(save).not.toHaveBeenCalled()
  })
})
