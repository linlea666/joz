import type {
  DiscordConfig,
  DiscordChannelPreviewMessage,
  CopyTradeEvent,
  CopyTradeSignal,
  CopyTradeContext,
  CopyTradeAIStat,
  CopyTradeReplayReport,
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
    offset = 0,
    startTimeMs?: number,
    endTimeMs?: number
  ): Promise<CopyTradeEvent[]> {
    let url = `${API_BASE}/copytrade/events?trader_id=${encodeURIComponent(traderId)}&limit=${limit}&offset=${offset}`
    if (startTimeMs) url += `&start_time=${startTimeMs}`
    if (endTimeMs) url += `&end_time=${endTimeMs}`
    const result = await httpClient.get<{ events: CopyTradeEvent[] }>(url)
    if (!result.success) throw new Error('Failed to fetch copy trade events')
    return result.data?.events ?? []
  },

  /**
   * Downloads events or signals of a time range as a CSV file.
   */
  async exportCopyTradeCSV(
    kind: 'events' | 'signals',
    traderId: string,
    startTimeMs?: number,
    endTimeMs?: number
  ): Promise<void> {
    let url = `${API_BASE}/copytrade/${kind}?trader_id=${encodeURIComponent(traderId)}&format=csv`
    if (startTimeMs) url += `&start_time=${startTimeMs}`
    if (endTimeMs) url += `&end_time=${endTimeMs}`

    const token = localStorage.getItem('auth_token')
    const resp = await fetch(url, {
      headers: token ? { Authorization: `Bearer ${token}` } : undefined,
    })
    if (!resp.ok) throw new Error(`Export failed (${resp.status})`)
    const blob = await resp.blob()
    const objectUrl = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = objectUrl
    a.download = `copytrade_${kind}_${new Date().toISOString().slice(0, 10)}.csv`
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(objectUrl)
  },

  async getCopyTradeSignals(
    traderId: string,
    limit = 50,
    startTimeMs?: number,
    endTimeMs?: number
  ): Promise<CopyTradeSignal[]> {
    let url = `${API_BASE}/copytrade/signals?trader_id=${encodeURIComponent(traderId)}&limit=${limit}`
    if (startTimeMs) url += `&start_time=${startTimeMs}`
    if (endTimeMs) url += `&end_time=${endTimeMs}`
    const result = await httpClient.get<{ signals: CopyTradeSignal[] }>(url)
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

  async startCopyTradeReplay(traderId: string, limit: number): Promise<void> {
    const result = await httpClient.post(`${API_BASE}/copytrade/replay`, {
      trader_id: traderId,
      limit,
    })
    if (!result.success) {
      throw new Error(result.message || 'Failed to start replay')
    }
  },

  async getCopyTradeReplay(
    traderId: string
  ): Promise<CopyTradeReplayReport | null> {
    const result = await httpClient.get<{
      report: CopyTradeReplayReport | null
    }>(`${API_BASE}/copytrade/replay?trader_id=${encodeURIComponent(traderId)}`)
    if (!result.success) throw new Error('Failed to fetch replay status')
    return result.data?.report ?? null
  },

  async getCopyTradeAIStats(days = 7): Promise<CopyTradeAIStat[]> {
    const result = await httpClient.get<{ stats: CopyTradeAIStat[] }>(
      `${API_BASE}/copytrade/ai-stats?days=${days}`
    )
    if (!result.success) throw new Error('Failed to fetch AI stats')
    return result.data?.stats ?? []
  },
}
