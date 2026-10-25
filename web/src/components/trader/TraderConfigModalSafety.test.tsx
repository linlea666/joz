import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { TraderConfigModal } from './TraderConfigModal'
import { TraderConfigViewModal } from './TraderConfigViewModal'
import type {
  AIModel,
  CopyTradingConfig,
  Exchange,
  TraderConfigData,
} from '../../types'

const confirmToast = vi.hoisted(() => vi.fn())

vi.mock('../../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: 'zh' }),
}))
vi.mock('../../lib/notify', () => ({ confirmToast }))
vi.mock('../../lib/httpClient', () => ({
  httpClient: {
    get: vi.fn().mockResolvedValue({ success: true, data: { strategies: [] } }),
  },
}))
vi.mock('../../lib/api', () => ({
  api: {
    getCopyTradeProfiles: vi.fn().mockResolvedValue([
      {
        id: 'default',
        name: '默认',
        description: '通用解释',
        recommended_config: {},
      },
      {
        id: 'tyler_v1',
        name: 'TYLER',
        description: '特殊管理话术',
        recommended_config: {
          market_dual_price_mode: 'legacy',
          default_reduce_ratio: 50,
          channel_notes: 'TYLER 画像',
        },
      },
      {
        id: 'cmm_v1',
        name: 'CMM',
        description: '新信号卡与操作通知',
        recommended_config: {
          market_dual_price_mode: 'market_then_limit',
          default_reduce_ratio: 50,
          channel_notes: 'CMM 画像',
        },
      },
    ]),
  },
}))

beforeEach(() => {
  confirmToast.mockReset()
  confirmToast.mockResolvedValue(true)
})
afterEach(cleanup)

const SAVE = { name: '保存修改' }

function setup(
  extra: Partial<CopyTradingConfig> = {},
  trader: Partial<TraderConfigData> = {}
) {
  const save = vi.fn().mockResolvedValue(undefined)
  render(
    <TraderConfigModal
      isOpen
      isEditMode
      onClose={() => {}}
      onSave={save}
      traderData={
        {
          trader_id: 'copy-1',
          trader_name: 'TYLER',
          ai_model: 'model',
          exchange_id: 'exchange',
          trader_type: 'copy_trading',
          strategy_id: '',
          is_cross_margin: false,
          scan_interval_minutes: 7,
          copy_trading_config: JSON.stringify({
            primary_channel_id: '123',
            risk_amount_usd: 15,
            ...extra,
          }),
          ...trader,
        } as TraderConfigData
      }
      availableModels={[
        { id: 'model', name: 'Model', enabled: true } as AIModel,
      ]}
      availableExchanges={[
        {
          id: 'exchange',
          name: 'Binance',
          exchange_type: 'binance',
          enabled: true,
        } as Exchange,
      ]}
    />
  )
  return save
}

async function savedConfig(save: ReturnType<typeof vi.fn>) {
  await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
  return JSON.parse(save.mock.calls[0][0].copy_trading_config)
}

describe('copy-trading form fields', () => {
  it('shows duplicate protection as always on and always submits it on', async () => {
    const save = setup({ duplicate_open_protection: false })
    expect(
      screen.queryByRole('button', { name: /重复开仓保护/ })
    ).not.toBeInTheDocument()
    expect(screen.getByTestId('copy-dup-protection')).toHaveTextContent(
      '始终开启'
    )
    fireEvent.click(screen.getByRole('button', SAVE))
    expect((await savedConfig(save)).duplicate_open_protection).toBe(true)
  })

  it('hides margin mode and scan interval, keeping the stored values', async () => {
    const save = setup()
    expect(screen.queryByText('保证金模式')).not.toBeInTheDocument()
    expect(screen.queryByText(/扫描间隔/)).not.toBeInTheDocument()
    expect(screen.getByText(/请在交易所中设置/)).toBeInTheDocument()
    expect(screen.getByText('解析模型（需支持图片）*')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', SAVE))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(save.mock.calls[0][0]).toMatchObject({
      is_cross_margin: false,
      scan_interval_minutes: 7,
    })
  })

  it.each([
    ['单仓最大名义价值（USD）', '', '必填。'],
    ['单仓最大名义价值（USD）', '0', '必须大于 0。'],
    ['BTC/ETH 杠杆', '126', '请输入 1 至 125 之间的整数。'],
    ['最大同时持仓数', '0', '请输入不小于 1 的整数。'],
    ['风险金额（USD）', '', '必填。'],
    ['BTC/ETH 市价偏移阈值（%）', '', '必填。0 表示不容许不利价差。'],
  ])('blocks %s = %j', (label, value, message) => {
    const save = setup()
    fireEvent.change(screen.getByLabelText(label), { target: { value } })
    expect(screen.getByRole('alert')).toHaveTextContent(message)
    const button = screen.getByRole('button', SAVE)
    expect(button).toBeDisabled()
    fireEvent.click(button)
    expect(save).not.toHaveBeenCalled()
  })

  it('keeps a cleared offset blank instead of saving 0, and saves an explicit 0', async () => {
    const save = setup({ major_price_offset_pct: 0.5 })
    const offset = screen.getByLabelText('BTC/ETH 市价偏移阈值（%）')
    fireEvent.change(offset, { target: { value: '' } })
    expect(offset).toHaveValue(null)
    expect(screen.getByRole('button', SAVE)).toBeDisabled()
    fireEvent.change(offset, { target: { value: '0' } })
    fireEvent.click(screen.getByRole('button', SAVE))
    expect((await savedConfig(save)).major_price_offset_pct).toBe(0)
  })

  it('shows the effective default cap for a legacy 0 notional', async () => {
    const save = setup({ max_position_notional_usd: 0 })
    expect(screen.getByLabelText('单仓最大名义价值（USD）')).toHaveValue(30000)
    fireEvent.click(screen.getByRole('button', SAVE))
    expect((await savedConfig(save)).max_position_notional_usd).toBe(30000)
  })

  it('allows more than 20 open positions like the backend', async () => {
    const save = setup()
    fireEvent.change(screen.getByLabelText('最大同时持仓数'), {
      target: { value: '30' },
    })
    fireEvent.click(screen.getByRole('button', SAVE))
    expect((await savedConfig(save)).max_open_positions).toBe(30)
  })

  it('limits percentage risk to 100 and validates TP ratios', () => {
    setup({ risk_mode: 'percent', risk_amount_usd: 150 })
    expect(screen.getByRole('alert')).toHaveTextContent('不能超过 100')
    cleanup()
    setup({ default_tp_ratios: '60,50' })
    expect(screen.getByRole('alert')).toHaveTextContent('总和不超过 100')
    expect(screen.getByRole('button', SAVE)).toBeDisabled()
  })

  it('requires a Chroma author filter', () => {
    setup({ source_mode: 'chroma', source_channel_ids: ['201'] })
    expect(screen.getByRole('alert')).toHaveTextContent('作者 ID 或作者名称')
    expect(screen.getByRole('button', SAVE)).toBeDisabled()
  })
})

describe('saving a running copy trader', () => {
  it('asks for confirmation and does not save when cancelled', async () => {
    confirmToast.mockResolvedValue(false)
    const save = setup({}, { is_running: true })
    fireEvent.click(screen.getByRole('button', SAVE))
    await waitFor(() => expect(confirmToast).toHaveBeenCalledTimes(1))
    const [message, options] = confirmToast.mock.calls[0]
    expect(message).toContain('重启跟单引擎')
    expect(message).toContain('重放')
    expect(message).toContain('不能再开仓')
    expect(options.okText).toBe('保存并重启')
    await Promise.resolve()
    expect(save).not.toHaveBeenCalled()
  })

  it('saves after confirmation', async () => {
    const save = setup({}, { is_running: true })
    fireEvent.click(screen.getByRole('button', SAVE))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(confirmToast).toHaveBeenCalledTimes(1)
  })

  it('does not ask for a stopped trader', async () => {
    const save = setup({}, { is_running: false })
    fireEvent.click(screen.getByRole('button', SAVE))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(confirmToast).not.toHaveBeenCalled()
  })
})

describe('switching author presets', () => {
  it('asks before replacing a user-written profile and keeps it when cancelled', async () => {
    confirmToast.mockResolvedValue(false)
    const save = setup({ channel_notes: '我的画像' })
    await screen.findByRole('option', { name: 'CMM' })
    fireEvent.change(screen.getByLabelText('作者风格预设'), {
      target: { value: 'cmm_v1' },
    })
    await waitFor(() => expect(confirmToast).toHaveBeenCalledTimes(1))
    expect(confirmToast.mock.calls[0][1].title).toBe('覆盖频道画像？')
    await Promise.resolve()
    expect(screen.getByLabelText('作者风格预设')).toHaveValue('default')
    expect(screen.getByDisplayValue('我的画像')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', SAVE))
    expect(await savedConfig(save)).toMatchObject({
      interpretation_profile: 'default',
      channel_notes: '我的画像',
    })
  })

  it('replaces the profile after confirmation', async () => {
    setup({ channel_notes: '我的画像' })
    await screen.findByRole('option', { name: 'CMM' })
    fireEvent.change(screen.getByLabelText('作者风格预设'), {
      target: { value: 'cmm_v1' },
    })
    expect(await screen.findByDisplayValue('CMM 画像')).toBeInTheDocument()
    expect(confirmToast).toHaveBeenCalledTimes(1)
  })

  it('does not ask when the profile is empty or an untouched preset', async () => {
    setup({ channel_notes: 'TYLER 画像', interpretation_profile: 'tyler_v1' })
    await screen.findByRole('option', { name: 'CMM' })
    fireEvent.change(screen.getByLabelText('作者风格预设'), {
      target: { value: 'cmm_v1' },
    })
    expect(screen.getByDisplayValue('CMM 画像')).toBeInTheDocument()
    expect(confirmToast).not.toHaveBeenCalled()
  })
})

describe('TraderConfigViewModal', () => {
  it('shows the effective copy-trading parameters and the exchange name', async () => {
    render(
      <TraderConfigViewModal
        isOpen
        onClose={() => {}}
        exchanges={[
          {
            id: '6f1c2d3e-aaaa-bbbb-cccc-000000000000',
            name: 'Binance',
            exchange_type: 'binance',
            account_name: '主账户',
            enabled: true,
          } as Exchange,
        ]}
        traderData={
          {
            trader_id: 'copy-1',
            trader_name: 'TYLER',
            ai_model: 'model',
            exchange_id: '6f1c2d3e-aaaa-bbbb-cccc-000000000000',
            trader_type: 'copy_trading',
            is_cross_margin: true,
            scan_interval_minutes: 15,
            initial_balance: 1000,
            is_running: true,
            show_in_competition: true,
            copy_trading_config: JSON.stringify({
              primary_channel_id: '123',
              source_author_ids: ['77'],
              source_author_names: ['JONZi'],
              risk_mode: 'fixed',
              risk_amount_usd: 25,
              major_leverage: 15,
              max_position_notional_usd: 12000,
              max_open_positions: 8,
              default_tp_ratios: '60,40',
              entry_timeout_minutes: 0,
              major_price_offset_pct: 0,
              paused: true,
            }),
          } as TraderConfigData
        }
      />
    )
    const view = screen.getByTestId('copy-config-view')
    for (const text of [
      '固定保证金',
      '$25',
      '15x',
      '10x', // altcoin leverage default
      '$12,000',
      '8',
      '60,40',
      '0（不自动过期）',
      '0%',
      '1%', // altcoin offset default
      '77',
      'JONZi',
      '始终开启',
    ]) {
      expect(view).toHaveTextContent(text)
    }
    expect(screen.getByText('作者 ID')).toBeInTheDocument()
    expect(screen.getByText('作者名称（精确匹配）')).toBeInTheDocument()
    expect(screen.getByText(/须同时满足/)).toBeInTheDocument()
    expect(screen.getByText('BINANCE - 主账户')).toBeInTheDocument()
    expect(screen.queryByText('6F1C2D3E')).not.toBeInTheDocument()
    expect(screen.queryByText('保证金模式')).not.toBeInTheDocument()
    expect(screen.queryByText('扫描间隔')).not.toBeInTheDocument()
  })

  it('names the exchange from public data and tolerates a missing balance', () => {
    render(
      <TraderConfigViewModal
        isOpen
        onClose={() => {}}
        traderData={
          {
            trader_id: 'scan-1',
            trader_name: 'Scan',
            ai_model: 'deepseek',
            exchange: 'okx',
            is_running: false,
          } as unknown as TraderConfigData
        }
      />
    )
    expect(screen.getByText('OKX')).toBeInTheDocument()
    expect(screen.getByText('保证金模式')).toBeInTheDocument()
  })
})
