import { getAuthHeaders, handleJSONResponse } from './helpers'

export interface LogEntry {
  id: number
  scope: string
  event_id?: string
  component: string
  level: string
  event: string
  message: string
  context_json: string
  occurred_at: string
  trader_id?: string
  channel_id?: string
  logical_channel_id?: string
  message_id?: string
  source_event_id?: string
  signal_id?: string
  trace_id?: string
  delivery_id?: number
  duration_ms: number
}
export interface LogPage {
  start?: string
  end?: string
  events: LogEntry[]
  next_cursor: number
  upper: number
  refreshed_at: string
  write_failures: number
}
export interface LogSettings {
  system_days: number
  retention: Record<string, number>
  soft_limit_bytes: number
  stats: {
    count: number
    logical_bytes: number
    earliest?: string
    write_failures: number
  }
}
export interface CleanupPreview {
  ticket: string
  count: number
  cutoff: string
  expires_at: string
}
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  return handleJSONResponse<T>(
    await fetch(`/api/logs/${path}`, { ...init, headers: getAuthHeaders() })
  )
}
// Count RFC4180 records, respecting embedded newlines and escaped quotes.
export function csvRecordCount(text: string): number {
  let quoted = false,
    records = 0
  for (let i = 0; i < text.length; i++) {
    if (text[i] === '"') {
      if (quoted && text[i + 1] === '"') i++
      else quoted = !quoted
    } else if (text[i] === '\n' && !quoted) records++
  }
  if (quoted) throw new Error('导出 CSV 不完整')
  if (text.length && !text.endsWith('\n')) records++
  return records
}
export const logsApi = {
  events: (params: URLSearchParams, signal?: AbortSignal) =>
    request<LogPage>(`events?${params}`, { signal }),
  trace: (params: URLSearchParams) =>
    request<Record<string, unknown>>(`trace?${params}`),
  settings: () => request<LogSettings>('settings'),
  saveDays: (days: number) =>
    request<LogSettings>('settings', {
      method: 'PUT',
      body: JSON.stringify({ system_days: days }),
    }),
  preview: (cutoff: string) =>
    request<CleanupPreview>('cleanup/preview', {
      method: 'POST',
      body: JSON.stringify({ cutoff }),
    }),
  cleanup: (ticket: string) =>
    request<{ removed: number }>('cleanup', {
      method: 'POST',
      body: JSON.stringify({ ticket }),
    }),
  async export(params: URLSearchParams) {
    const res = await fetch(`/api/logs/export?${params}`, {
      headers: getAuthHeaders(),
    })
    if (!res.ok) {
      await handleJSONResponse(res)
      return
    }
    const blob = await res.blob()
    // Verify a completed, parseable record count before offering the download.
    const expected = Number(res.headers.get('X-Log-Count'))
    const text = await blob.text()
    const actual =
      params.get('format') === 'jsonl'
        ? text.trim().split('\n').filter(Boolean).length
        : csvRecordCount(text) - 1
    if (
      !res.headers.has('X-Log-Count') ||
      !Number.isFinite(expected) ||
      actual !== expected
    )
      throw new Error('导出期间记录发生变化或输出不完整，请重试')
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download =
      /filename="([^"]+)"/.exec(
        res.headers.get('Content-Disposition') || ''
      )?.[1] || `logs.${params.get('format')}`
    link.click()
    URL.revokeObjectURL(url)
  },
}
