import type { CopyTradeAIRun, CopyTradeSignal } from '../../types'
export function CopyTradeTiming({
  signal,
  run,
}: {
  signal: CopyTradeSignal
  run?: CopyTradeAIRun
}) {
  const attempts = run?.attempts
  const durations = attempts?.filter(
    (a) => a.status === 'completed' || a.status === 'failed'
  )
  const completedMs = durations?.reduce((sum, a) => sum + a.duration_ms, 0)
  let rulesVersion: number | undefined
  try {
    rulesVersion = JSON.parse(run?.rules_snapshot_json || '{}').version
  } catch {
    /* Historical evidence may be absent. */
  }
  return (
    <div className="text-xs p-3 border border-nofx-gold/20 rounded space-y-2">
      <p>
        处理阶段：
        {(
          {
            interpreting: '解释中',
            retry_wait: '等待重试',
            failed: '处理失败',
            executed: '已执行',
            skipped: '已跳过',
          } as Record<string, string>
        )[signal.status] || signal.status}
      </p>
      <p>
        端到端（首次接收到最终结果）：
        {signal.end_to_end_ms == null
          ? '未知／尚未完成'
          : `${(signal.end_to_end_ms / 1000).toFixed(1)} 秒`}
      </p>
      <p>
        首次排队 {signal.queue_ms ?? '未知'} ms · 最近尝试媒体{' '}
        {signal.media_download_ms} ms · 提示词 {signal.prompt_build_ms} ms ·
        执行 {signal.exchange_submit_ms} ms
      </p>
      <p>
        重试等待累计：
        {signal.retry_wait_ms == null
          ? '未知／无记录'
          : `${(signal.retry_wait_ms / 1000).toFixed(1)} 秒`}
      </p>
      {run?.provider === 'deterministic' ? (
        <p>规则处理：管理规则 v{rulesVersion ?? '未知'}（不计入 AI 调用）</p>
      ) : attempts?.length ? (
        <>
          <p>
            已完成 AI 调用合计：{((completedMs || 0) / 1000).toFixed(1)}{' '}
            秒；规则及未完成尝试不计入。
          </p>
          {signal.end_to_end_ms != null && completedMs != null && (
            <p>
              其余阶段合计（排队、重试等待及处理）：
              {(Math.max(0, signal.end_to_end_ms - completedMs) / 1000).toFixed(
                1
              )}{' '}
              秒
            </p>
          )}
          <table className="w-full">
            <thead>
              <tr>
                <th>尝试</th>
                <th>模型</th>
                <th>状态</th>
                <th>耗时</th>
              </tr>
            </thead>
            <tbody>
              {attempts.map((a) => (
                <tr key={a.id}>
                  <td>{a.attempt}</td>
                  <td>{a.model}</td>
                  <td>
                    {a.status}
                    <p>{a.error}</p>
                  </td>
                  <td>
                    {a.status === 'running' || a.status === 'interrupted'
                      ? '未完成'
                      : `${(a.duration_ms / 1000).toFixed(1)} 秒`}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      ) : (
        <p>旧结果或尚无尝试记录：分层耗时未知。</p>
      )}
    </div>
  )
}
