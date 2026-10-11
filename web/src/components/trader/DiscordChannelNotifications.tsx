import { useEffect, useState } from 'react'
import { api } from '../../lib/api'
import type {
  ChannelNotificationConfig,
  ChannelNotificationState,
} from '../../types'
import type { Language } from '../../i18n/translations'

const input =
  'w-full rounded-lg border border-nofx-border bg-nofx-bg px-3 py-2 text-sm'
const button =
  'rounded-lg border border-nofx-border px-3 py-2 text-sm disabled:opacity-40'
const empty: ChannelNotificationConfig = {
  revision: 0,
  enabled: false,
  recipient: '',
  monitors: [],
}
function readableTime(value?: string) {
  return value && !value.startsWith('0001-')
    ? new Date(value).toLocaleString()
    : '—'
}
export function DiscordChannelNotifications({
  language,
}: {
  language: Language
}) {
  const zh = language === 'zh'
  const [draft, setDraft] = useState<ChannelNotificationConfig>(empty)
  const [status, setStatus] = useState<ChannelNotificationState>()
  const [batch, setBatch] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const result = await api.getChannelNotifications()
        if (active) {
          setStatus(result)
          setDraft(result.config)
        }
      } catch (e) {
        if (active) setError(String(e))
      }
    })()
    return () => {
      active = false
    }
  }, [])
  function add() {
    const ids = Array.from(new Set(batch.split(/[,，\s]+/).filter(Boolean)))
    if (
      ids.some(
        (id) => !/^\d{17,20}$/.test(id) || BigInt(id) > 18446744073709551615n
      )
    ) {
      setError(
        zh ? '请填写有效的纯数字频道 ID' : 'Enter valid numeric channel IDs'
      )
      return
    }
    const added = ids.filter(
      (id) => !draft.monitors.some((m) => m.channel_id === id)
    )
    if (draft.monitors.length + added.length > 20) {
      setError(zh ? '最多监控 20 个频道' : 'Maximum 20 channels')
      return
    }
    setDraft({
      ...draft,
      monitors: [
        ...draft.monitors,
        ...added.map((channel_id) => ({
          id: '',
          channel_id,
          label: '',
          enabled: true,
          generation: 0,
          activated_at: '',
        })),
      ],
    })
    setBatch('')
    setError('')
    setNotice(zh ? '已加入表单，保存后生效。' : 'Added to form; save to apply.')
  }
  async function save() {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const result = await api.saveChannelNotifications(draft)
      setDraft(result.config || draft)
      setStatus(result)
      setNotice(
        !draft.enabled
          ? zh
            ? '已保存，通知已关闭。'
            : 'Saved; notifications disabled.'
          : result.applied && !result.apply_error
            ? zh
              ? '已保存并生效。'
              : 'Saved and applied.'
            : `${zh ? '已保存；' : 'Saved; '}${result.apply_error || (zh ? '等待核验频道' : 'awaiting channel verification')}`
      )
    } catch (e) {
      setError(String(e))
    } finally {
      setBusy(false)
    }
  }
  async function test() {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const result = await api.testChannelNotificationEmail(
        draft.recipient.trim()
      )
      if (!result.ok) throw new Error(result.error)
      setNotice(
        zh
          ? 'SMTP 已接受测试邮件；测试未保存配置，请检查收件箱。'
          : 'SMTP accepted the test; settings were not saved. Check your inbox.'
      )
    } catch (e) {
      setError(String(e))
    } finally {
      setBusy(false)
    }
  }
  async function refresh() {
    setBusy(true)
    setError('')
    try {
      const result = await api.getChannelNotifications()
      if (!status) setDraft(result.config)
      setStatus(result)
    } catch (e) {
      setError(String(e))
    } finally {
      setBusy(false)
    }
  }
  const stateText = (value?: string) => {
    if (!value) return '—'
    if (!zh) return value
    return (
      (
        {
          ready: '就绪',
          waiting: '等待应用',
          recovering: '核验中',
          forbidden: '无访问权限',
          gap: '核验未完成',
          disabled: '已关闭',
          pending: '待发送',
          sending: '发送中',
          retry: '等待重试',
          sent: 'SMTP 已接受',
          failed: '发送失败',
          uncertain: '结果不明（不重发）',
          expired: '已过期',
          cancelled: '已取消',
          dropped: '队列已满，未发送',
        } as Record<string, string>
      )[value] || value
    )
  }
  return (
    <section
      className="mt-6 space-y-3 border-t border-nofx-border pt-5"
      aria-label={zh ? '频道消息邮件通知' : 'Channel message emails'}
    >
      <h4 className="font-semibold">
        {zh ? '频道消息邮件通知' : 'Channel message emails'}
      </h4>
      <p className="text-xs text-nofx-text-muted">
        {zh
          ? '每条实时新消息一封；不分析、不跟单，不补发历史、断线或重启积压。共用上方 SMTP，使用独立收件邮箱。'
          : 'One email per live new message. No AI, trading, history or outage catch-up. Shared SMTP, separate recipient.'}
      </p>
      <label className="flex items-center gap-2">
        <input
          type="checkbox"
          disabled={busy}
          checked={draft.enabled}
          onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
        />
        {zh ? '启用频道消息通知' : 'Enable channel emails'}
      </label>
      <label className="block text-sm">
        {zh ? '频道通知收件邮箱' : 'Channel notification recipient'}
        <input
          className={input}
          type="email"
          disabled={busy}
          value={draft.recipient}
          onChange={(e) => setDraft({ ...draft, recipient: e.target.value })}
        />
      </label>
      <p className="text-xs text-nofx-text-muted">
        {zh
          ? '与采集异常告警独立；观察或正常执行模式下均可通知。关闭全局消息采集后不再接收新消息。'
          : 'Independent of collection alerts and trading mode. Requires the global message collection switch.'}
      </p>
      {status && (
        <p className="text-xs">
          SMTP:{' '}
          {status.smtp_configured
            ? zh
              ? '已配置'
              : 'configured'
            : zh
              ? '未配置'
              : 'not configured'}{' '}
          ·{' '}
          {status.apply_error ||
            (status.applied
              ? zh
                ? '采集配置已应用'
                : 'Collection applied'
              : zh
                ? '等待应用'
                : 'Pending')}
        </p>
      )}
      <label className="block text-sm">
        {zh
          ? '批量添加频道 ID（逗号或换行分隔）'
          : 'Channel IDs (commas or newlines)'}
        <textarea
          className={input}
          rows={2}
          value={batch}
          onChange={(e) => setBatch(e.target.value)}
        />
      </label>
      <button
        type="button"
        className={button}
        disabled={busy || !batch.trim()}
        onClick={add}
      >
        {zh ? '添加到列表' : 'Add to list'}
      </button>
      <span className="ml-2 text-xs">{draft.monitors.length}/20</span>
      {draft.monitors.map((monitor, i) => {
        const state = status?.channels?.find((c) => c.monitor_id === monitor.id)
        return (
          <div
            key={monitor.id || monitor.channel_id}
            className="space-y-2 rounded-lg border border-nofx-border p-3"
          >
            <div className="flex flex-wrap items-center justify-between gap-2">
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  disabled={busy}
                  checked={monitor.enabled}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      monitors: draft.monitors.map((m, j) =>
                        j === i ? { ...m, enabled: e.target.checked } : m
                      ),
                    })
                  }
                />
                {monitor.channel_id}
              </label>
              <button
                type="button"
                className={button}
                disabled={busy}
                onClick={() =>
                  setDraft({
                    ...draft,
                    monitors: draft.monitors.filter((_, j) => i !== j),
                  })
                }
              >
                {zh ? '删除' : 'Remove'}
              </button>
            </div>
            <input
              className={input}
              aria-label={`${zh ? '频道备注' : 'Channel label'} ${monitor.channel_id}`}
              placeholder={zh ? '可选备注' : 'Optional label'}
              disabled={busy}
              value={monitor.label}
              onChange={(e) =>
                setDraft({
                  ...draft,
                  monitors: draft.monitors.map((m, j) =>
                    j === i ? { ...m, label: e.target.value } : m
                  ),
                })
              }
            />
            <p className="text-xs">
              {!monitor.enabled
                ? zh
                  ? '已停用'
                  : 'Disabled'
                : state
                  ? `${state.applied ? (zh ? '已就绪' : 'Ready') : stateText(state.state)} ${state.last_error || ''}`
                  : zh
                    ? '尚未保存'
                    : 'Not saved'}
            </p>
            <p className="text-xs">
              {zh ? '最后消息' : 'Last message'}:{' '}
              {readableTime(state?.last_message_at)}
              <br />
              {zh ? '最近邮件' : 'Last email'}:{' '}
              {stateText(state?.last_result?.status)}{' '}
              {readableTime(state?.last_result?.updated_at)}{' '}
              {state?.last_result?.error}
            </p>
            <a
              className="text-xs underline"
              href={`/logs?scope=system&component=channel_notify&channel_id=${monitor.channel_id}`}
              target="_blank"
              rel="noreferrer"
            >
              {zh ? '查看通知日志' : 'View email logs'}
            </a>
          </div>
        )
      })}
      <p className="text-xs text-nofx-text-muted">
        {zh
          ? '停用、删除或更换邮箱会取消尚未发送的通知；已开始发送的邮件无法撤回。列表修改保存后生效。通知记录保留 30 天；SMTP 接受邮件不保证收件箱到达。'
          : 'Disabling, removing or changing the recipient cancels pending mail. In-flight mail cannot be recalled. Save to apply changes.'}
      </p>
      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          className={button}
          disabled={busy || !status}
          onClick={() => void save()}
        >
          {zh ? '保存频道通知' : 'Save channel emails'}
        </button>
        <button
          type="button"
          className={button}
          disabled={busy || !draft.recipient.trim()}
          onClick={() => void test()}
        >
          {zh ? '测试通知邮箱（不保存）' : 'Test recipient (without saving)'}
        </button>
        <button
          type="button"
          className={button}
          disabled={busy}
          onClick={() => void refresh()}
        >
          {zh ? '刷新通知状态' : 'Refresh status'}
        </button>
      </div>
      {error && (
        <p role="alert" className="text-sm text-red-600">
          {error}
        </p>
      )}
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
    </section>
  )
}
