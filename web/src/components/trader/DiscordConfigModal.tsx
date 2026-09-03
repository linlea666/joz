import { useState, useEffect } from 'react'
import { MessageSquare, Trash2, PlugZap, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '../../lib/api'
import type { DiscordConfig } from '../../types'
import { t, type Language } from '../../i18n/translations'

interface DiscordConfigModalProps {
  onClose: () => void
  language: Language
}

// Global Discord token settings shared by all copy-trading traders.
export function DiscordConfigModal({ onClose, language }: DiscordConfigModalProps) {
  const [config, setConfig] = useState<DiscordConfig | null>(null)
  const [isLoading, setIsLoading] = useState(true)
  const [token, setToken] = useState('')
  const [pollInterval, setPollInterval] = useState(6)
  const [enabled, setEnabled] = useState(true)
  const [isSaving, setIsSaving] = useState(false)
  const [isTesting, setIsTesting] = useState(false)
  const [isClearing, setIsClearing] = useState(false)
  const [testChannelId, setTestChannelId] = useState('')
  const [isTestingChannel, setIsTestingChannel] = useState(false)
  const [channelPreview, setChannelPreview] = useState<string[]>([])

  const loadConfig = async () => {
    try {
      const cfg = await api.getDiscordConfig()
      setConfig(cfg)
      setPollInterval(cfg.poll_interval_seconds || 6)
      setEnabled(cfg.enabled)
    } catch {
      // First load may 404 before any config exists — keep defaults
    } finally {
      setIsLoading(false)
    }
  }

  useEffect(() => {
    loadConfig()
  }, [])

  const handleSave = async () => {
    if (isSaving) return
    setIsSaving(true)
    try {
      await api.updateDiscordConfig({
        token: token.trim(),
        poll_interval_seconds: pollInterval,
        enabled,
      })
      toast.success(t('discord.saved', language))
      setToken('')
      await loadConfig()
    } catch (err) {
      toast.error(
        err instanceof Error && err.message
          ? err.message
          : t('discord.saveFailed', language)
      )
    } finally {
      setIsSaving(false)
    }
  }

  const handleTest = async () => {
    if (isTesting) return
    setIsTesting(true)
    try {
      const res = await api.testDiscordConnection(token.trim() || undefined)
      if (res.ok) {
        toast.success(`${t('discord.testOk', language)}: ${res.username}`)
      } else {
        toast.error(`${t('discord.testFailed', language)}: ${res.error || ''}`)
      }
    } catch {
      toast.error(t('discord.testFailed', language))
    } finally {
      setIsTesting(false)
    }
  }

  const handleClear = async () => {
    if (isClearing) return
    if (!window.confirm(t('discord.clearConfirm', language))) return
    setIsClearing(true)
    try {
      await api.deleteDiscordToken()
      toast.success(t('discord.cleared', language))
      await loadConfig()
    } catch {
      toast.error(t('discord.clearFailed', language))
    } finally {
      setIsClearing(false)
    }
  }

  const handleTestChannel = async () => {
    if (isTestingChannel || !testChannelId.trim()) return
    setIsTestingChannel(true)
    setChannelPreview([])
    try {
      const res = await api.testDiscordChannel(testChannelId.trim())
      if (res.ok && res.messages) {
        setChannelPreview(
          res.messages.map(
            (m) =>
              `[${m.author_name}] ${m.content || (m.has_embeds ? '(embed)' : m.attachments > 0 ? '(image)' : '(empty)')}`
          )
        )
        toast.success(t('discord.channelOk', language))
      } else {
        toast.error(`${t('discord.channelFailed', language)}: ${res.error || ''}`)
      }
    } catch {
      toast.error(t('discord.channelFailed', language))
    } finally {
      setIsTestingChannel(false)
    }
  }

  return (
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50 p-4 overflow-y-auto backdrop-blur-sm">
      <div
        className="rounded-2xl w-full max-w-lg relative my-8 shadow-2xl"
        style={{ background: '#F7F4EC' }}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-6 pb-2">
          <div className="flex items-center gap-2">
            <MessageSquare className="w-6 h-6" style={{ color: '#5865F2' }} />
            <h3 className="text-xl font-bold" style={{ color: '#1A1813' }}>
              {t('discord.title', language)}
            </h3>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="p-2 rounded-lg hover:bg-black/5 transition-colors"
            style={{ color: '#8A8478' }}
          >
            ✕
          </button>
        </div>

        <div className="px-6 pb-6 space-y-5 pt-4">
          {isLoading ? (
            <div className="text-center py-8 text-sm" style={{ color: '#8A8478' }}>
              ...
            </div>
          ) : (
            <>
              {/* Current status */}
              <div
                className="p-3 rounded-xl flex items-center gap-3"
                style={{ background: '#F1ECE2', border: '1px solid rgba(26,24,19,0.14)' }}
              >
                <div
                  className={`w-2 h-2 rounded-full flex-shrink-0 ${config?.configured ? 'bg-nofx-success' : 'bg-nofx-text-muted'}`}
                />
                <div className="min-w-0 flex-1">
                  <div className="text-xs font-mono" style={{ color: '#8A8478' }}>
                    {t('discord.currentToken', language)}
                  </div>
                  <div className="text-sm font-mono truncate" style={{ color: '#1A1813' }}>
                    {config?.configured
                      ? config.token_masked
                      : t('discord.notConfigured', language)}
                  </div>
                </div>
                {config?.configured && (
                  <button
                    onClick={handleClear}
                    disabled={isClearing}
                    className="p-2 rounded-lg transition-colors hover:bg-black/5 disabled:opacity-50"
                    style={{ color: '#D6433A' }}
                    title={t('discord.clearToken', language)}
                  >
                    <Trash2 className="w-4 h-4" />
                  </button>
                )}
              </div>

              {/* Token input */}
              <div className="space-y-2">
                <label className="text-sm font-semibold" style={{ color: '#1A1813' }}>
                  {t('discord.tokenLabel', language)}
                </label>
                <input
                  type="password"
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  placeholder={
                    config?.configured
                      ? t('discord.tokenKeepPlaceholder', language)
                      : t('discord.tokenPlaceholder', language)
                  }
                  className="w-full px-4 py-3 rounded-xl font-mono text-sm"
                  style={{ background: '#F1ECE2', border: '1px solid rgba(26,24,19,0.14)', color: '#1A1813' }}
                />
                <div className="text-xs" style={{ color: '#8A8478' }}>
                  {t('discord.tokenHint', language)}
                </div>
              </div>

              {/* Poll interval + enable */}
              <div className="grid grid-cols-2 gap-4">
                <div className="space-y-2">
                  <label className="text-sm font-semibold" style={{ color: '#1A1813' }}>
                    {t('discord.pollInterval', language)}
                  </label>
                  <input
                    type="number"
                    min={3}
                    max={300}
                    value={pollInterval}
                    onChange={(e) => {
                      const v = Number(e.target.value)
                      setPollInterval(Number.isFinite(v) ? v : 6)
                    }}
                    className="w-full px-4 py-3 rounded-xl text-sm"
                    style={{ background: '#F1ECE2', border: '1px solid rgba(26,24,19,0.14)', color: '#1A1813' }}
                  />
                </div>
                <div className="space-y-2">
                  <label className="text-sm font-semibold" style={{ color: '#1A1813' }}>
                    {t('discord.enablePolling', language)}
                  </label>
                  <div className="flex gap-2">
                    <button
                      type="button"
                      onClick={() => setEnabled(true)}
                      className="flex-1 px-3 py-3 rounded-xl text-sm font-semibold"
                      style={{
                        background: enabled ? '#2E8B57' : '#E8E2D5',
                        color: enabled ? '#fff' : '#8A8478',
                      }}
                    >
                      {t('discord.on', language)}
                    </button>
                    <button
                      type="button"
                      onClick={() => setEnabled(false)}
                      className="flex-1 px-3 py-3 rounded-xl text-sm font-semibold"
                      style={{
                        background: !enabled ? '#D6433A' : '#E8E2D5',
                        color: !enabled ? '#fff' : '#8A8478',
                      }}
                    >
                      {t('discord.off', language)}
                    </button>
                  </div>
                </div>
              </div>

              {/* Safety note */}
              <div
                className="p-3 rounded-xl text-xs"
                style={{ background: 'rgba(224, 72, 59, 0.08)', border: '1px solid rgba(224, 72, 59, 0.2)', color: '#8A8478' }}
              >
                {t('discord.riskNote', language)}
              </div>

              {/* Save / Test */}
              <div className="flex gap-3">
                <button
                  onClick={handleTest}
                  disabled={isTesting || (!token.trim() && !config?.configured)}
                  className="flex-1 flex items-center justify-center gap-2 px-4 py-3 rounded-xl text-sm font-semibold transition-all hover:bg-black/5 disabled:opacity-50"
                  style={{ background: '#E8E2D5', color: '#1A1813' }}
                >
                  <PlugZap className="w-4 h-4" />
                  {isTesting ? '...' : t('discord.testConnection', language)}
                </button>
                <button
                  onClick={handleSave}
                  disabled={isSaving || (!token.trim() && !config?.configured)}
                  className="flex-1 px-4 py-3 rounded-xl text-sm font-bold transition-all hover:scale-[1.02] disabled:opacity-50"
                  style={{ background: '#5865F2', color: '#fff' }}
                >
                  {isSaving ? t('discord.saving', language) : t('discord.save', language)}
                </button>
              </div>

              {/* Channel test */}
              {config?.configured && (
                <div className="space-y-2 pt-2" style={{ borderTop: '1px solid rgba(26,24,19,0.1)' }}>
                  <label className="text-sm font-semibold" style={{ color: '#1A1813' }}>
                    {t('discord.testChannelLabel', language)}
                  </label>
                  <div className="flex gap-2">
                    <input
                      type="text"
                      value={testChannelId}
                      onChange={(e) => setTestChannelId(e.target.value)}
                      placeholder="1385639865194315949"
                      className="flex-1 px-4 py-2.5 rounded-xl font-mono text-sm"
                      style={{ background: '#F1ECE2', border: '1px solid rgba(26,24,19,0.14)', color: '#1A1813' }}
                    />
                    <button
                      onClick={handleTestChannel}
                      disabled={isTestingChannel || !testChannelId.trim()}
                      className="px-4 py-2.5 rounded-xl text-sm font-bold disabled:opacity-50"
                      style={{ background: '#2E8B57', color: '#fff' }}
                    >
                      {isTestingChannel ? '...' : <RefreshCw className="w-4 h-4" />}
                    </button>
                  </div>
                  {channelPreview.length > 0 && (
                    <div
                      className="p-3 rounded-xl space-y-1 max-h-40 overflow-y-auto"
                      style={{ background: '#F1ECE2', border: '1px solid rgba(26,24,19,0.14)' }}
                    >
                      {channelPreview.map((line, i) => (
                        <div key={i} className="text-xs font-mono truncate" style={{ color: '#1A1813' }}>
                          {line}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}

              {/* Poller channel status */}
              {config?.channels && config.channels.length > 0 && (
                <div className="space-y-2">
                  <div className="text-xs font-semibold uppercase tracking-wide" style={{ color: '#8A8478' }}>
                    {t('discord.channelStatus', language)}
                  </div>
                  {config.channels.map((ch) => (
                    <div
                      key={ch.channel_id}
                      className="p-2.5 rounded-xl flex items-center justify-between text-xs font-mono"
                      style={{ background: '#F1ECE2', border: '1px solid rgba(26,24,19,0.14)' }}
                    >
                      <span style={{ color: '#1A1813' }}>{ch.channel_id}</span>
                      <span style={{ color: ch.last_error ? '#D6433A' : '#2E8B57' }}>
                        {ch.last_error
                          ? t('discord.channelError', language)
                          : `${ch.subscribers} ${t('discord.subscribers', language)}`}
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}
