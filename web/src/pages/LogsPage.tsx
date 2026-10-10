import { useCallback, useEffect, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  logsApi,
  type CleanupPreview,
  type LogEntry,
  type LogPage,
  type LogSettings,
} from '../lib/api/logs'
import { EventList } from '../components/logs/EventList'

const inputClass =
  'rounded-lg border border-nofx-border bg-nofx-bg px-3 py-2 text-sm w-full'
const buttonClass =
  'rounded-lg border border-nofx-border px-3 py-2 text-sm disabled:opacity-40 hover:bg-nofx-gold/10'
function localTime(date: Date) {
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000)
    .toISOString()
    .slice(0, 16)
}
export function LogsPage() {
  const [search] = useSearchParams()
  const [scope, setScope] = useState(
    search.get('scope') === 'trade' ? 'trade' : 'system'
  )
  const [filters, setFilters] = useState<Record<string, string>>({
    trader_id: search.get('trader_id') || '',
    start: '',
    end: '',
    level: '',
    component: '',
    channel_id: '',
    event: '',
    correlation_id: '',
    q: '',
  })
  const [applied, setApplied] = useState(filters)
  const [page, setPage] = useState<LogPage>()
  const [cursor, setCursor] = useState<{
    before: number
    upper: number
    start?: string
    end?: string
  }>()
  const [auto, setAuto] = useState(true)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [settings, setSettings] = useState<LogSettings>()
  const [cutoff, setCutoff] = useState(
    localTime(new Date(Date.now() - 30 * 86400000))
  )
  const [preview, setPreview] = useState<CleanupPreview>()
  const [notice, setNotice] = useState('')
  const [trace, setTrace] = useState<Record<string, unknown>>()
  const controller = useRef<AbortController>()
  const params = useCallback(() => {
    const p = new URLSearchParams({ scope })
    for (const [k, v] of Object.entries(applied))
      if (v)
        p.set(k, k === 'start' || k === 'end' ? new Date(v).toISOString() : v)
    if (cursor) {
      p.set('before', String(cursor.before))
      p.set('upper', String(cursor.upper))
      if (cursor.start) p.set('start', cursor.start)
      if (cursor.end) p.set('end', cursor.end)
    }
    return p
  }, [scope, applied, cursor])
  const refresh = useCallback(async () => {
    controller.current?.abort()
    const next = new AbortController()
    controller.current = next
    try {
      const result = await logsApi.events(params(), next.signal)
      if (!next.signal.aborted) {
        setPage(result)
        setError('')
      }
    } catch (e) {
      if (!next.signal.aborted) setError(String(e))
    }
  }, [params])
  useEffect(() => {
    void refresh()
    return () => controller.current?.abort()
  }, [refresh])
  useEffect(() => {
    if (!auto || cursor) return
    const timer = setInterval(() => void refresh(), 5000)
    return () => clearInterval(timer)
  }, [auto, cursor, refresh])
  useEffect(() => {
    logsApi
      .settings()
      .then(setSettings)
      .catch((e) => setError(String(e)))
  }, [])
  const action = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError('')
    try {
      await fn()
    } catch (e) {
      setError(String(e))
    } finally {
      setBusy(false)
    }
  }
  const showTrace = (row: LogEntry) =>
    void action(async () => {
      const p = new URLSearchParams()
      if (row.signal_id) p.set('signal_id', row.signal_id)
      else if (row.source_event_id)
        p.set('source_event_id', row.source_event_id)
      else p.set('context_id', row.trace_id!.slice('reconcile-'.length))
      setTrace(await logsApi.trace(p))
    })
  const traceLabels: Record<string, string> = {
    sources: '1. 来源事件与不可变消息修订',
    deliveries: '2. 交易员投递与规则快照',
    signals: '3. 解释、风控与阶段耗时',
    ai_runs: '4. AI 请求与回复',
    contexts: '5. 目标交易与执行计划',
    actions: '6. 执行动作',
    orders: '7. 订单与成交',
    events: '8. 执行及恢复事件',
  }
  return (
    <main className="mx-auto max-w-7xl px-4 py-8 text-nofx-text">
      <h1 className="text-2xl font-bold">日志中心</h1>
      <p className="mt-2 text-sm text-nofx-text-muted">
        按事件追踪采集、解释、执行和恢复。查询本机已保存记录，不向 Discord
        拉取消息。
      </p>
      <div className="my-5 flex flex-wrap items-center gap-3">
        {['system', 'trade'].map((tab) => (
          <button
            key={tab}
            aria-pressed={scope === tab}
            className={`${buttonClass} ${scope === tab ? 'bg-nofx-gold/15 font-bold' : ''}`}
            onClick={() => {
              setScope(tab)
              setCursor(undefined)
              setPage(undefined)
            }}
          >
            {tab === 'system' ? '系统日志' : '交易日志'}
          </button>
        ))}
        <label className="ml-auto flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={auto}
            onChange={(e) => setAuto(e.target.checked)}
          />
          自动刷新（5 秒）
        </label>
        <button className={buttonClass} onClick={() => void refresh()}>
          刷新
        </button>
        {['csv', 'jsonl'].map((format) => (
          <button
            key={format}
            disabled={busy}
            className={buttonClass}
            onClick={() =>
              void action(async () => {
                const p = params()
                p.delete('before')
                p.delete('upper')
                p.set('format', format)
                await logsApi.export(p)
              })
            }
          >
            导出 {format.toUpperCase()}
          </button>
        ))}
      </div>
      <form
        className="mb-5 grid gap-3 sm:grid-cols-2 lg:grid-cols-5"
        onSubmit={(e) => {
          e.preventDefault()
          setApplied({ ...filters })
          setCursor(undefined)
          setPage(undefined)
        }}
      >
        {Object.entries({
          start: '开始时间（默认近 24 小时）',
          end: '结束时间（默认现在）',
          trader_id: '交易员 ID',
          channel_id: '来源频道 ID',
          level: '级别',
          component: '模块',
          event: '事件名称',
          correlation_id: '关联 ID',
          q: '摘要关键词',
        }).map(([key, label]) => (
          <label key={key} className="text-xs text-nofx-text-muted">
            {label}
            {key === 'level' ? (
              <select
                aria-label={label}
                className={inputClass}
                value={filters[key]}
                onChange={(e) =>
                  setFilters({ ...filters, [key]: e.target.value })
                }
              >
                <option value="">全部</option>
                {['info', 'success', 'warn', 'error'].map((x) => (
                  <option key={x}>{x}</option>
                ))}
              </select>
            ) : (
              <input
                aria-label={label}
                className={inputClass}
                type={
                  key === 'start' || key === 'end' ? 'datetime-local' : 'text'
                }
                value={filters[key]}
                onChange={(e) =>
                  setFilters({ ...filters, [key]: e.target.value })
                }
              />
            )}
          </label>
        ))}
        <button className={`${buttonClass} self-end`} type="submit">
          应用筛选
        </button>
      </form>
      {error && (
        <p role="alert" className="mb-4 rounded-lg bg-red-100 p-3 text-red-800">
          {error}
        </p>
      )}
      {notice && (
        <p
          role="status"
          className="mb-4 rounded-lg bg-green-100 p-3 text-green-800"
        >
          {notice}
        </p>
      )}
      {(page?.write_failures || 0) > 0 && (
        <p role="alert" className="mb-3 text-red-700">
          日志记录不完整：本次运行有 {page?.write_failures}{' '}
          次持久化失败，请核对采集状态与磁盘。
        </p>
      )}
      <p className="mb-3 text-xs text-nofx-text-muted">
        {cursor
          ? '历史页：自动刷新已暂停'
          : auto
            ? '自动刷新已开启'
            : '自动刷新已暂停'}{' '}
        · 最后成功刷新：
        {page ? new Date(page.refreshed_at).toLocaleTimeString() : '等待查询'} ·
        页面刷新状态不代表 Gateway 连接正常
      </p>
      <EventList events={page?.events || []} onTrace={showTrace} />
      <div className="my-4 flex gap-3">
        <button
          disabled={!cursor}
          className={buttonClass}
          onClick={() => setCursor(undefined)}
        >
          回到最新
        </button>
        <button
          disabled={!page?.next_cursor}
          className={buttonClass}
          onClick={() => {
            if (page)
              setCursor({
                before: page.next_cursor,
                upper: page.upper,
                start: page.start,
                end: page.end,
              })
          }}
        >
          更早记录
        </button>
      </div>
      {scope === 'system' && (
        <details className="mt-8 rounded-xl border border-nofx-border p-4">
          <summary className="cursor-pointer font-bold">
            保留设置与历史清理
          </summary>
          <div className="mt-4 space-y-4 text-sm">
            <p>
              系统日志 {settings?.system_days ?? '—'} 天；交易事件{' '}
              {settings?.retention.events ?? '—'} 天；信号{' '}
              {settings?.retention.signals ?? '—'} 天；AI 原文{' '}
              {settings?.retention.ai ?? '—'}{' '}
              天。活跃交易和待确认动作的证据受保护。
            </p>
            <p>
              系统日志逻辑容量：
              {((settings?.stats.logical_bytes || 0) / 1048576).toFixed(2)} MiB
              / 512 MiB；最早可用：
              {settings?.stats.earliest
                ? new Date(settings.stats.earliest).toLocaleString()
                : '无记录'}
              。容量上限可能缩短保留时间。
            </p>
            <label>
              系统日志保留天数{' '}
              <select
                aria-label="系统日志保留天数"
                className={buttonClass}
                disabled={!settings || busy}
                value={settings?.system_days || 30}
                onChange={(e) =>
                  void action(async () => {
                    setSettings(await logsApi.saveDays(Number(e.target.value)))
                    setNotice('保留设置已保存，按小时清理；不重连采集器。')
                  })
                }
              >
                {[7, 30, 90].map((x) => (
                  <option key={x} value={x}>
                    {x} 天
                  </option>
                ))}
              </select>
            </label>
            <div className="flex flex-wrap gap-3 items-end">
              <label>
                清理此时间之前的系统日志
                <input
                  aria-label="清理截止时间"
                  type="datetime-local"
                  className={inputClass}
                  value={cutoff}
                  onChange={(e) => {
                    setCutoff(e.target.value)
                    setPreview(undefined)
                  }}
                />
              </label>
              <button
                className={buttonClass}
                disabled={busy || !cutoff}
                onClick={() =>
                  void action(async () =>
                    setPreview(
                      await logsApi.preview(new Date(cutoff).toISOString())
                    )
                  )
                }
              >
                预览清理
              </button>
            </div>
            {preview && (
              <div className="rounded-lg border border-orange-300 p-3">
                <p>
                  将删除 {new Date(preview.cutoff).toLocaleString()} 前的{' '}
                  {preview.count}{' '}
                  条系统日志。仅清理预览边界内记录，不影响交易、订单、去重和采集缓冲。
                </p>
                <button
                  className={`${buttonClass} mt-2 text-red-700`}
                  disabled={busy}
                  onClick={() =>
                    void action(async () => {
                      const r = await logsApi.cleanup(preview.ticket)
                      setPreview(undefined)
                      setNotice(
                        `已清理 ${r.removed} 条系统日志；数据库文件不会立即缩小。`
                      )
                      setSettings(await logsApi.settings())
                      await refresh()
                    })
                  }
                >
                  确认清理 {preview.count} 条系统日志
                </button>
                <button
                  className={`${buttonClass} ml-2`}
                  onClick={() => setPreview(undefined)}
                >
                  取消
                </button>
              </div>
            )}
          </div>
        </details>
      )}
      {trace && (
        <section
          role="dialog"
          aria-modal="true"
          aria-label="完整链路"
          className="fixed inset-0 z-50 overflow-auto bg-black/50 p-4 md:p-12"
        >
          <div className="mx-auto max-w-5xl rounded-2xl bg-nofx-bg p-6">
            <div className="flex justify-between">
              <h2 className="text-xl font-bold">完整链路</h2>
              <button
                autoFocus
                className={buttonClass}
                onClick={() => setTrace(undefined)}
              >
                关闭链路
              </button>
            </div>
            <p className="my-3 text-sm">{String(trace.evidence_note || '')}</p>
            {trace.truncated === true && (
              <p role="alert">
                关联记录过多，部分阶段仅显示前 500 条；请按信号缩小查询范围。
              </p>
            )}
            {Object.entries(traceLabels).map(([key, label]) => (
              <details
                key={key}
                open={key === 'signals'}
                className="my-3 rounded-lg border border-nofx-border p-3"
              >
                <summary className="cursor-pointer font-bold">{label}</summary>
                <pre className="mt-3 max-h-96 overflow-auto whitespace-pre-wrap break-all text-xs">
                  {JSON.stringify(trace[key] || [], null, 2)}
                </pre>
              </details>
            ))}
          </div>
        </section>
      )}
    </main>
  )
}
