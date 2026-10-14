import { useState } from 'react'
import { api } from '../../lib/api'
import type { RecoveryReviewTicket } from '../../types'

export function RecoveryReview({
  contextId,
  onComplete,
}: {
  contextId: string
  onComplete: () => void
}) {
  const [ticket, setTicket] = useState<RecoveryReviewTicket | null>(null)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const perform = async (confirm: boolean) => {
    setBusy(true)
    setError('')
    try {
      const result = await api.reviewCopyRecovery(
        contextId,
        confirm && ticket ? { ticket: ticket.id, reason } : undefined
      )
      if (confirm) {
        setTicket(null)
        onComplete()
      } else setTicket(result)
    } catch (e) {
      setError(e instanceof Error ? e.message : '核对失败')
      setTicket(null)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-2 max-w-md whitespace-normal">
      <button
        type="button"
        disabled={busy}
        onClick={() => perform(false)}
        className="text-nofx-gold hover:underline disabled:opacity-50"
      >
        核对并结束本地恢复
      </button>
      {error && (
        <p role="alert" className="text-nofx-danger">
          {error}
        </p>
      )}
      {ticket && (
        <div
          role="dialog"
          aria-label="确认本地恢复收尾"
          className="border border-nofx-gold/30 rounded p-3 space-y-2"
        >
          <p>
            已核验空仓且无相关挂单。仅结束本地恢复，保留原请求结果未知的审计；不会下单、撤单或平仓。
          </p>
          <p>
            预览有效至 {new Date(ticket.expires_at).toLocaleTimeString()}
            ，确认时再次核验。
          </p>
          <details>
            <summary>查询证据</summary>
            <pre className="whitespace-pre-wrap break-all">
              {ticket.evidence_json}
            </pre>
          </details>
          <input
            aria-label="核对原因"
            placeholder="填写核对原因"
            value={reason}
            maxLength={300}
            onChange={(e) => setReason(e.target.value)}
            className="w-full p-2 rounded bg-nofx-bg-deeper"
          />
          <button
            type="button"
            disabled={busy || !reason.trim()}
            onClick={() => perform(true)}
            className="text-nofx-danger disabled:opacity-50"
          >
            确认结束本地恢复
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={() => setTicket(null)}
            className="ml-3"
          >
            取消
          </button>
        </div>
      )}
    </div>
  )
}
