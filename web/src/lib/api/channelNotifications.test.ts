import { expect, it, vi } from 'vitest'
import { discordApi } from './discord'
import { httpClient } from './helpers'
vi.mock('./helpers', () => ({
  API_BASE: '/api',
  httpClient: { put: vi.fn() },
  CryptoService: {},
}))
it('sends editable fields only, so new channels cannot send empty RFC3339 times or spoof generations', async () => {
  vi.mocked(httpClient.put).mockResolvedValue({
    success: true,
    data: { config: {}, channels: [] },
  })
  await discordApi.saveChannelNotifications({
    revision: 2,
    enabled: true,
    recipient: 'a@example.com',
    monitors: [
      {
        id: '',
        channel_id: '1539560125189062696',
        label: 'new',
        enabled: true,
        generation: 999,
        activated_at: '',
      },
    ],
  })
  expect(httpClient.put).toHaveBeenCalledWith(
    '/api/discord/channel-notifications',
    {
      revision: 2,
      enabled: true,
      recipient: 'a@example.com',
      monitors: [
        {
          id: '',
          channel_id: '1539560125189062696',
          label: 'new',
          enabled: true,
        },
      ],
    }
  )
})
