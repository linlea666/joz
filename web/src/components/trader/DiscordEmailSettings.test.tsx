import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { DiscordEmailSettings } from './DiscordEmailSettings'
import { api } from '../../lib/api'
vi.mock('../../lib/api', () => ({
  api: {
    getDiscordEmail: vi.fn(),
    saveDiscordEmail: vi.fn(),
    testDiscordAlertEmail: vi.fn(),
  },
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
    enabled: false,
    source: 'none',
    password_set: false,
    configured: false,
  })
  vi.mocked(api.saveDiscordEmail).mockResolvedValue(undefined)
})
it('configures mail without Discord and preserves disabled alerts', async () => {
  render(<DiscordEmailSettings language="zh" />)
  const sender = await screen.findByLabelText('发件邮箱／登录账号')
  await waitFor(() => expect(sender).toBeEnabled())
  fireEvent.change(sender, { target: { value: 'sender@example.com' } })
  expect(screen.getByLabelText('告警收件邮箱')).toHaveValue(
    'sender@example.com'
  )
  fireEvent.change(screen.getByLabelText('告警收件邮箱'), {
    target: { value: 'recipient@example.com' },
  })
  fireEvent.change(sender, { target: { value: 'other@example.com' } })
  expect(screen.getByLabelText('告警收件邮箱')).toHaveValue(
    'recipient@example.com'
  )
  fireEvent.change(screen.getByLabelText('邮箱授权码'), {
    target: { value: 'test-only-secret' },
  })
  fireEvent.click(screen.getByRole('button', { name: '保存邮件配置' }))
  await waitFor(() =>
    expect(api.saveDiscordEmail).toHaveBeenCalledWith(
      expect.objectContaining({
        host: 'smtp.163.com',
        port: 465,
        security: 'tls',
        enabled: false,
        recipient: 'recipient@example.com',
        password: 'test-only-secret',
      })
    )
  )
  await screen.findByText('配置已保存，之后的告警使用新配置。')
  expect(screen.getByLabelText('邮箱授权码')).toHaveValue('')
})
it('tests current unsaved form without saving or changing it on failure', async () => {
  vi.mocked(api.testDiscordAlertEmail).mockResolvedValue({
    ok: false,
    error: 'SMTP authentication failed',
  })
  render(<DiscordEmailSettings language="zh" />)
  await waitFor(() =>
    expect(screen.getByRole('button', { name: '发送测试邮件' })).toBeEnabled()
  )
  fireEvent.change(screen.getByLabelText('SMTP 服务器'), {
    target: { value: 'custom.example.com' },
  })
  fireEvent.change(screen.getByLabelText('邮箱授权码'), {
    target: { value: 'test-only-secret' },
  })
  fireEvent.click(screen.getByRole('button', { name: '发送测试邮件' }))
  await screen.findByRole('alert')
  expect(api.testDiscordAlertEmail).toHaveBeenCalledWith(
    undefined,
    expect.objectContaining({
      host: 'custom.example.com',
      password: 'test-only-secret',
    })
  )
  expect(api.saveDiscordEmail).not.toHaveBeenCalled()
  expect(screen.getByLabelText('邮箱授权码')).toHaveValue('test-only-secret')
})
it('shows environment origin and retains password by an empty input', async () => {
  vi.mocked(api.getDiscordEmail).mockResolvedValue({
    host: 'smtp.example.com',
    port: 587,
    security: 'starttls',
    user: 'sender@example.com',
    recipient: 'to@example.com',
    enabled: true,
    source: 'environment',
    password_set: true,
    configured: true,
  })
  render(<DiscordEmailSettings language="zh" />)
  await screen.findByText('来自服务器环境；保存后完整使用页面配置。')
  expect(screen.getByPlaceholderText('已设置；留空保留')).toHaveValue('')
  fireEvent.change(screen.getByLabelText('邮件服务'), {
    target: { value: '163' },
  })
  expect(screen.getByLabelText('SMTP 服务器')).toHaveValue('smtp.163.com')
  expect(screen.getByLabelText('端口')).toHaveValue(465)
  expect(screen.getByLabelText('连接方式')).toHaveValue('tls')
  expect(screen.getByLabelText('告警收件邮箱')).toHaveValue('to@example.com')
})
it('blocks save when loading/decrypting fails instead of overwriting with defaults', async () => {
  vi.mocked(api.getDiscordEmail).mockRejectedValue(
    new Error('credential decryption failed')
  )
  render(<DiscordEmailSettings language="zh" />)
  await screen.findByRole('alert')
  expect(screen.getByRole('button', { name: '保存邮件配置' })).toBeDisabled()
  expect(api.saveDiscordEmail).not.toHaveBeenCalled()
})
