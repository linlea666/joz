import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { DiscordConfigModal } from './DiscordConfigModal'
import { api } from '../../lib/api'
import { toast } from 'sonner'
vi.mock('../../lib/api', () => ({
  api: {
    getDiscordConfig: vi.fn(),
    getDiscordEmail: vi.fn(),
    updateDiscordConfig: vi.fn(),
    deleteDiscordToken: vi.fn(),
  },
}))
vi.mock('sonner', () => ({
  toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() },
}))
afterEach(cleanup)
beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(api.getDiscordEmail).mockResolvedValue({
    host: '',
    port: 465,
    security: 'tls',
    user: '',
    recipient: '',
    enabled: true,
    source: 'none',
    password_set: false,
    configured: false,
  })
  vi.mocked(api.getDiscordConfig).mockResolvedValue({
    configured: true,
    token_masked: '****',
    run_mode: 'observe',
    enabled: true,
    monitor_enabled: true,
    alert_email: '',
    smtp_configured: false,
    collector: { state: 'disconnected', backlog: 2 },
    channels: [],
  })
})
it('defaults to observation, submits no polling settings and reports apply failure', async () => {
  vi.mocked(api.updateDiscordConfig).mockResolvedValue({
    saved: true,
    applied: false,
    apply_error: 'collector unavailable',
  })
  render(<DiscordConfigModal language="zh" onClose={() => {}} />)
  await screen.findByRole('option', { name: '仅采集验证' })
  expect(screen.getByLabelText('执行模式')).toHaveValue('observe')
  expect(screen.queryByText('轮询间隔（秒）')).not.toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(api.updateDiscordConfig).toHaveBeenCalled())
  const data = vi.mocked(api.updateDiscordConfig).mock.calls[0][0]
  expect(data.run_mode).toBe('observe')
  expect(data).not.toHaveProperty('poll_interval_seconds')
  expect(data).not.toHaveProperty('monitor_interval_seconds')
  expect(toast.warning).toHaveBeenCalledWith(
    expect.stringContaining('已保存但未生效')
  )
  expect(toast.success).not.toHaveBeenCalled()
})
it('requires explicit live selection and displays gap separately from connection', async () => {
  vi.mocked(api.getDiscordConfig).mockResolvedValue({
    configured: true,
    token_masked: '****',
    run_mode: 'observe',
    enabled: true,
    monitor_enabled: true,
    alert_email: '',
    smtp_configured: false,
    collector: {
      state: 'connected',
      backlog: 0,
      applied_version: 'v',
      desired_version: 'v',
    },
    channels: [
      { channel_id: '200', state: 'gap', last_error: 'history incomplete' },
    ],
  })
  vi.mocked(api.updateDiscordConfig).mockResolvedValue({
    saved: true,
    applied: true,
  })
  render(<DiscordConfigModal language="zh" onClose={() => {}} />)
  await screen.findByRole('option', { name: '仅采集验证' })
  expect(screen.getByText('gap: history incomplete')).toBeInTheDocument()
  fireEvent.change(screen.getByLabelText('执行模式'), {
    target: { value: 'live' },
  })
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() =>
    expect(api.updateDiscordConfig).toHaveBeenCalledWith(
      expect.objectContaining({ run_mode: 'live' })
    )
  )
})
