import type {
  DiscordConfig,
  DiscordChannelPreviewMessage,
  CopyTradeEvent,
  CopyTradeSignal,
  CopyTradeContext,
  CopyTradeAIStat,
} from '../../types'
import { API_BASE, httpClient } from './helpers'

export const discordApi = {
  async getDiscordConfig(): Promise<DiscordConfig> {
    const result = await httpClient.get<DiscordConfig>(`${API_BASE}/discord`)
    if (!result.success) throw new Error('Failed to fetch Discord config')
    return result.data!
  },

  async updateDiscordConfig(params: {
    token?: string
    poll_interval_seconds?: number
    enabled?: boolean
  }): Promise<void> {
    const result = await httpClient.post(`${API_BASE}/discord`, params)
    if (!result.success) {
      throw new Error(result.message || 'Failed to save Discord config')
    }
  },

  async deleteDiscordToken(): Promise<void> {
    const result = await httpClient.delete(`${API_BASE}/discord/token`)
    if (!result.success) throw new Error('Failed to clear Discord token')
  },

  async testDiscordConnection(token?: string): Promise<{
    ok: boolean
    username?: string
    user_id?: string
    error?: string
  }> {
    const result = await httpClient.post<{
      ok: boolean
      username?: string
      user_id?: string
      error?: string
    }>(`${API_BASE}/discord/test`, { token: token ?? '' })
    if (!result.success) throw new Error('Failed to test Discord connection')
    return result.data!
  },

  async testDiscordChannel(channelId: string): Promise<{
    ok: boolean
    messages?: DiscordChannelPreviewMessage[]
    error?: string
  }> {
    const result = await httpClient.post<{
      ok: boolean
      messages?: DiscordChannelPreviewMessage[]
      error?: string
    }>(`${API_BASE}/discord/test-channel`, { channel_id: channelId })
    if (!result.success) throw new Error('Failed to test Discord channel')
    return result.data!
  },

  // --- Copy trading observability ---

  async getCopyTradeEvents(
    traderId: string,
    limit = 100,
    offset = 0
  ): Promise<CopyTradeEvent[]> {
    const result = await httpClient.get<{ events: CopyTradeEvent[] }>(
      `${API_BASE}/copytrade/events?trader_id=${encodeURIComponent(traderId)}&limit=${limit}&offset=${offset}`
    )
    if (!result.success) throw new Error('Failed to fetch copy trade events')
    return result.data?.events ?? []
  },

  async getCopyTradeEventsByTrace(traceId: string): Promise<CopyTradeEvent[]> {
    const result = await httpClient.get<{ events: CopyTradeEvent[] }>(
      `${API_BASE}/copytrade/events?trace_id=${encodeURIComponent(traceId)}`
    )
    if (!result.success) throw new Error('Failed to fetch trace events')
    return result.data?.events ?? []
  },

  async getCopyTradeSignals(
    traderId: string,
    limit = 50
  ): Promise<CopyTradeSignal[]> {
    const result = await httpClient.get<{ signals: CopyTradeSignal[] }>(
      `${API_BASE}/copytrade/signals?trader_id=${encodeURIComponent(traderId)}&limit=${limit}`
    )
    if (!result.success) throw new Error('Failed to fetch copy trade signals')
    return result.data?.signals ?? []
  },

  async getCopyTradeContexts(traderId: string): Promise<CopyTradeContext[]> {
    const result = await httpClient.get<{ contexts: CopyTradeContext[] }>(
      `${API_BASE}/copytrade/contexts?trader_id=${encodeURIComponent(traderId)}`
    )
    if (!result.success) throw new Error('Failed to fetch copy trade contexts')
    return result.data?.contexts ?? []
  },

  async getCopyTradeAIStats(days = 7): Promise<CopyTradeAIStat[]> {
    const result = await httpClient.get<{ stats: CopyTradeAIStat[] }>(
      `${API_BASE}/copytrade/ai-stats?days=${days}`
    )
    if (!result.success) throw new Error('Failed to fetch AI stats')
    return result.data?.stats ?? []
  },
}
