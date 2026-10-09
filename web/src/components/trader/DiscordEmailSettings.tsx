import { useEffect, useState } from 'react'
import { api } from '../../lib/api'
import type { EmailDraft, EmailSettings } from '../../types'
import type { Language } from '../../i18n/translations'

export function DiscordEmailSettings({ language }: { language: Language }) {
  const zh = language === 'zh'
  const [saved, setSaved] = useState<EmailSettings | null>(null)
  const [draft, setDraft] = useState<EmailDraft>({
    host: 'smtp.163.com',
    port: 465,
    security: 'tls',
    user: '',
    password: '',
    recipient: '',
    enabled: true,
  })
  const [busy, setBusy] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState('')
  const [status, setStatus] = useState('')
  const [autoRecipient, setAutoRecipient] = useState(true)
  const [preset, setPreset] = useState('163')
  const load = async () => {
    try {
      const result = await api.getDiscordEmail()
      setSaved(result)
      setDraft({
        host: result.host || 'smtp.163.com',
        port: result.port || 465,
        security: result.security || 'tls',
        user: result.user,
        password: '',
        recipient: result.recipient,
        enabled: result.enabled,
      })
      setAutoRecipient(!result.recipient)
      setPreset(
        !result.host ||
          (result.host === 'smtp.163.com' &&
            result.port === 465 &&
            result.security === 'tls')
          ? '163'
          : 'custom'
      )
      setLoaded(true)
      setError('')
    } catch (err) {
      setError(
        err instanceof Error ? err.message : 'Failed to load email settings'
      )
    }
  }
  useEffect(() => {
    void load()
  }, [])
  const update = (patch: Partial<EmailDraft>) => {
    setDraft((d) => ({ ...d, ...patch }))
    setStatus('')
    setError('')
  }
  const submit = async (test: boolean) => {
    setBusy(true)
    setError('')
    setStatus('')
    try {
      if (test) {
        const result = await api.testDiscordAlertEmail(undefined, draft)
        if (!result.ok) throw new Error(result.error || 'SMTP failed')
        setStatus(
          zh
            ? '测试发送成功：SMTP 已接受邮件，请检查收件箱及垃圾邮件。测试未保存配置。'
            : 'Test accepted by SMTP. Check inbox and spam; settings were not saved.'
        )
      } else {
        await api.saveDiscordEmail(draft)
        await load()
        setStatus(
          zh
            ? '配置已保存，之后的告警使用新配置。'
            : 'Saved. Future alerts use these settings.'
        )
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Email operation failed')
    } finally {
      setBusy(false)
    }
  }
  const fieldStyle = {
    background: '#F1ECE2',
    border: '1px solid rgba(26,24,19,0.14)',
    color: '#1A1813',
  }
  const inputClass = 'w-full px-3 py-2.5 rounded-xl text-sm mt-1'
  return (
    <fieldset disabled={busy || !loaded} className="space-y-4">
      <p className="text-xs text-stone-500">
        {saved?.source === 'environment'
          ? zh
            ? '来自服务器环境；保存后完整使用页面配置。'
            : 'Using server environment; saving replaces it with this configuration.'
          : saved?.source === 'database'
            ? zh
              ? '配置已保存'
              : 'Saved configuration'
            : zh
              ? '未配置'
              : 'Not configured'}
      </p>
      <label className="block text-sm">
        {zh ? '邮件服务' : 'Email service'}
        <select
          className={inputClass}
          style={fieldStyle}
          value={preset}
          onChange={(e) => {
            setPreset(e.target.value)
            if (e.target.value === '163')
              update({ host: 'smtp.163.com', port: 465, security: 'tls' })
          }}
        >
          <option value="163">163 {zh ? '邮箱' : 'Mail'}</option>
          <option value="custom">{zh ? '自定义 SMTP' : 'Custom SMTP'}</option>
        </select>
      </label>
      <label className="block text-sm">
        {zh ? 'SMTP 服务器' : 'SMTP host'}
        <input
          className={inputClass}
          style={fieldStyle}
          value={draft.host}
          onChange={(e) => {
            setPreset('custom')
            update({ host: e.target.value })
          }}
        />
      </label>
      <div className="grid grid-cols-2 gap-4">
        <label className="text-sm">
          {zh ? '端口' : 'Port'}
          <input
            type="number"
            min="1"
            max="65535"
            className={inputClass}
            style={fieldStyle}
            value={draft.port}
            onChange={(e) => {
              setPreset('custom')
              update({ port: Number(e.target.value) })
            }}
          />
        </label>
        <label className="text-sm">
          {zh ? '连接方式' : 'Connection'}
          <select
            className={inputClass}
            style={fieldStyle}
            value={draft.security}
            onChange={(e) => {
              setPreset('custom')
              update({ security: e.target.value as EmailDraft['security'] })
            }}
          >
            <option value="tls">TLS</option>
            <option value="starttls">STARTTLS</option>
          </select>
        </label>
      </div>
      <label className="block text-sm">
        {zh ? '发件邮箱／登录账号' : 'Sender / login email'}
        <input
          type="email"
          autoComplete="off"
          className={inputClass}
          style={fieldStyle}
          value={draft.user}
          onChange={(e) =>
            update({
              user: e.target.value,
              ...(autoRecipient ? { recipient: e.target.value } : {}),
            })
          }
        />
      </label>
      <label className="block text-sm">
        {zh ? '邮箱授权码' : 'SMTP authorization code'}
        <input
          type="password"
          autoComplete="new-password"
          className={inputClass}
          style={fieldStyle}
          value={draft.password}
          placeholder={
            saved?.password_set
              ? zh
                ? '已设置；留空保留'
                : 'Set; leave blank to keep'
              : zh
                ? '首次配置必填'
                : 'Required for initial setup'
          }
          onChange={(e) => update({ password: e.target.value })}
        />
      </label>
      <p className="text-xs text-stone-500">
        {zh
          ? '更换 SMTP 服务器或登录账号时，请填写新的授权码。'
          : 'Changing host or login requires a new authorization code.'}
      </p>
      <label className="block text-sm">
        {zh ? '告警收件邮箱' : 'Alert recipient'}
        <input
          type="email"
          className={inputClass}
          style={fieldStyle}
          value={draft.recipient}
          onChange={(e) => {
            setAutoRecipient(false)
            update({ recipient: e.target.value })
          }}
        />
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={draft.enabled}
          onChange={(e) => update({ enabled: e.target.checked })}
        />
        {zh ? '启用邮件告警' : 'Enable email alerts'}
      </label>
      <div className="flex gap-3 flex-wrap">
        <button
          type="button"
          className="px-4 py-2 rounded-xl bg-red-500 text-white disabled:opacity-50"
          onClick={() => void submit(false)}
        >
          {busy ? '…' : zh ? '保存邮件配置' : 'Save email settings'}
        </button>
        <button
          type="button"
          className="px-4 py-2 rounded-xl bg-emerald-700 text-white disabled:opacity-50"
          onClick={() => void submit(true)}
        >
          {zh ? '发送测试邮件' : 'Send test email'}
        </button>
      </div>
      <p className="text-xs text-stone-500">
        {zh
          ? '测试使用当前表单，不会保存。邮件设置独立生效，无须配置 Discord Token 或重启。'
          : 'Test uses the current form without saving. No Discord token or restart is needed.'}
      </p>
      {status && (
        <p role="status" className="text-sm text-emerald-700">
          {status}
        </p>
      )}
      {error && (
        <p role="alert" className="text-sm text-red-600">
          {error}
        </p>
      )}
    </fieldset>
  )
}
