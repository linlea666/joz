import type { SystemStatus } from '../../types'

export function CopyTradeRecognitionStatus({
  status,
}: {
  status: SystemStatus
}) {
  return (
    <span title="仅统计当前保留的在线消息解释记录，含失败调用，内部重试不单列；不含识别回放，不是累计终身调用量。">
      {status.interpretation_model || status.ai_provider || '模型未知'} ·{' '}
      {status.recognition_stats_error ? (
        '识别统计暂不可用'
      ) : (
        <>
          AI 调用 {status.recognition_stats?.model_calls ?? '—'} · 规则处理{' '}
          {status.recognition_stats?.deterministic_runs ?? '—'}（保留记录）
        </>
      )}
    </span>
  )
}
