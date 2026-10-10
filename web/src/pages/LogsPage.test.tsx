import {
  render,
  screen,
  fireEvent,
  waitFor,
  cleanup,
} from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, afterEach, describe, it, expect, vi } from 'vitest'
import { LogsPage } from './LogsPage'
import { logsApi } from '../lib/api/logs'
vi.mock('../lib/api/logs', () => ({
  logsApi: {
    events: vi.fn(),
    settings: vi.fn(),
    saveDays: vi.fn(),
    preview: vi.fn(),
    cleanup: vi.fn(),
    trace: vi.fn(),
    export: vi.fn(),
  },
}))
const settings = {
  system_days: 30,
  retention: { events: 90, signals: 90, ai: 30 },
  soft_limit_bytes: 512 << 20,
  stats: { count: 1, logical_bytes: 100, write_failures: 0 },
}
const row = {
  id: 11,
  scope: 'system',
  component: 'collector',
  level: 'error',
  event: 'gateway.error',
  message: 'Gateway task failed',
  context_json: '{"code":"GATEWAY_TASK_FAILED"}',
  occurred_at: new Date().toISOString(),
  duration_ms: 0,
}
beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(logsApi.events).mockResolvedValue({
    events: [row],
    next_cursor: 10,
    upper: 11,
    refreshed_at: new Date().toISOString(),
    write_failures: 0,
  })
  vi.mocked(logsApi.settings).mockResolvedValue(settings)
})
afterEach(cleanup)
describe('日志中心', () => {
  it('显示系统事件并在历史页暂停自动刷新', async () => {
    render(
      <MemoryRouter>
        <LogsPage />
      </MemoryRouter>
    )
    expect(await screen.findByText('Gateway task failed')).toBeInTheDocument()
    fireEvent.click(screen.getByText('更早记录'))
    await waitFor(() =>
      expect(screen.getByText(/历史页：自动刷新已暂停/)).toBeInTheDocument()
    )
    const params = vi.mocked(logsApi.events).mock.calls.at(-1)![0]
    expect(params.get('before')).toBe('10')
    expect(params.get('upper')).toBe('11')
  })
  it('清理必须预览后确认，传递固定凭据', async () => {
    vi.mocked(logsApi.preview).mockResolvedValue({
      ticket: 'bounded-preview',
      count: 2,
      cutoff: new Date().toISOString(),
      expires_at: new Date().toISOString(),
    })
    vi.mocked(logsApi.cleanup).mockResolvedValue({ removed: 2 })
    render(
      <MemoryRouter>
        <LogsPage />
      </MemoryRouter>
    )
    fireEvent.click(screen.getByText('保留设置与历史清理'))
    fireEvent.click(screen.getByText('预览清理'))
    expect(logsApi.cleanup).not.toHaveBeenCalled()
    fireEvent.click(await screen.findByText('确认清理 2 条系统日志'))
    await waitFor(() =>
      expect(logsApi.cleanup).toHaveBeenCalledWith('bounded-preview')
    )
  })
  it('交易员入口保留筛选，查询失败不得显示连接正常', async () => {
    vi.mocked(logsApi.events).mockRejectedValue(new Error('db unavailable'))
    render(
      <MemoryRouter initialEntries={['/logs?scope=trade&trader_id=mine']}>
        <LogsPage />
      </MemoryRouter>
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('db unavailable')
    expect(vi.mocked(logsApi.events).mock.calls[0][0].get('trader_id')).toBe(
      'mine'
    )
    expect(screen.queryByText('连接正常')).not.toBeInTheDocument()
  })
})
