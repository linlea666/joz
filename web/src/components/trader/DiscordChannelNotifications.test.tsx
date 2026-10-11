import { beforeEach, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { DiscordChannelNotifications } from './DiscordChannelNotifications'
import { api } from '../../lib/api'
import type { ChannelNotificationState } from '../../types'
vi.mock('../../lib/api', () => ({
  api: {
    getChannelNotifications: vi.fn(),
    saveChannelNotifications: vi.fn(),
    testChannelNotificationEmail: vi.fn(),
  },
}))
const state: ChannelNotificationState = {
  config: { enabled: false, revision: 0, recipient: '', monitors: [] },
  smtp_configured: true,
  collection_enabled: true,
  credential_configured: true,
  applied: true,
  channels: [],
}
beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(api.getChannelNotifications).mockResolvedValue(
    structuredClone(state)
  )
})
it('adds deduplicated channels locally and saves only through independent API', async () => {
  render(<DiscordChannelNotifications language="zh" />)
  await waitFor(() => expect(screen.getByText('保存频道通知')).toBeEnabled())
  fireEvent.change(screen.getByLabelText('批量添加频道 ID（逗号或换行分隔）'), {
    target: {
      value: '1539560125189062696,1539560125189062696\n1531557737429991525',
    },
  })
  fireEvent.click(screen.getByText('添加到列表'))
  expect(screen.getByText('2/20')).toBeInTheDocument()
  expect(api.saveChannelNotifications).not.toHaveBeenCalled()
  fireEvent.change(screen.getByLabelText('频道通知收件邮箱'), {
    target: { value: 'new@example.com' },
  })
  fireEvent.click(screen.getByLabelText('启用频道消息通知'))
  vi.mocked(api.saveChannelNotifications).mockImplementation(
    async (config) => ({ ...state, config })
  )
  fireEvent.click(screen.getByText('保存频道通知'))
  await waitFor(() =>
    expect(api.saveChannelNotifications).toHaveBeenCalledWith(
      expect.objectContaining({
        enabled: true,
        recipient: 'new@example.com',
        monitors: expect.arrayContaining([
          expect.objectContaining({ channel_id: '1539560125189062696' }),
        ]),
      })
    )
  )
})
it('tests draft recipient without saving or enabling notification', async () => {
  vi.mocked(api.testChannelNotificationEmail).mockResolvedValue({ ok: true })
  render(<DiscordChannelNotifications language="zh" />)
  await waitFor(() => expect(screen.getByText('保存频道通知')).toBeEnabled())
  fireEvent.change(screen.getByLabelText('频道通知收件邮箱'), {
    target: { value: 'test@example.com' },
  })
  fireEvent.click(screen.getByText('测试通知邮箱（不保存）'))
  await screen.findByText('SMTP 已接受测试邮件；测试未保存配置，请检查收件箱。')
  expect(api.testChannelNotificationEmail).toHaveBeenCalledWith(
    'test@example.com'
  )
  expect(api.saveChannelNotifications).not.toHaveBeenCalled()
})
it('removes only from draft and keeps draft on status refresh', async () => {
  vi.mocked(api.getChannelNotifications).mockResolvedValue({
    ...state,
    config: {
      ...state.config,
      revision: 2,
      monitors: [
        {
          id: 'm',
          channel_id: '1539560125189062696',
          label: '',
          enabled: true,
          generation: 1,
          activated_at: '',
        },
      ],
    },
  })
  render(<DiscordChannelNotifications language="zh" />)
  await screen.findByText('1539560125189062696')
  fireEvent.click(screen.getByText('删除'))
  expect(screen.getByText('0/20')).toBeInTheDocument()
  fireEvent.click(screen.getByText('刷新通知状态'))
  await waitFor(() =>
    expect(api.getChannelNotifications).toHaveBeenCalledTimes(2)
  )
  expect(screen.getByText('0/20')).toBeInTheDocument()
  expect(api.saveChannelNotifications).not.toHaveBeenCalled()
})
it('blocks save when loading fails', async () => {
  vi.mocked(api.getChannelNotifications).mockRejectedValue(
    new Error('DB unavailable')
  )
  render(<DiscordChannelNotifications language="zh" />)
  await screen.findByRole('alert')
  expect(screen.getByText('保存频道通知')).toBeDisabled()
})
