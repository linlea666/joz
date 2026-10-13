import type { CopyTradeReplayItem } from '../../types'
import { entryPolicyLabel } from './EntryPolicyThresholds'

export function CopyTradeReplayChecks({
  item,
  language,
}: {
  item: CopyTradeReplayItem
  language: string
}) {
  const zh = language === 'zh'
  const names = {
    passed: zh ? '通过' : 'Passed',
    failed: zh ? '未通过' : 'Failed',
    unavailable: zh ? '查询不可用' : 'Unavailable',
    needs_price: zh ? '缺少原始进场参考价' : 'Missing original entry price',
    not_checked: zh ? '未检查' : 'Not checked',
    not_required: zh ? '此项不适用' : 'Not required',
  }
  return (
    <div className="rounded border border-nofx-border p-2 space-y-2 text-nofx-text-muted">
      {item.processing_path && (
        <div>
          {item.processing_path === 'deterministic'
            ? zh
              ? '确定性规则'
              : 'Deterministic rules'
            : 'AI'}{' '}
          · {item.processing_model || (zh ? '模型未知' : 'Unknown model')}
        </div>
      )}
      {!item.evaluations?.length ? (
        <div>
          {zh
            ? '此记录历史版本结果：未保存分层检查证据；不能推断历史行情或合约状态。'
            : 'Legacy result: no layered checks saved; historical market and contract state are unknown.'}
        </div>
      ) : (
        <>
          <div>
            {zh
              ? '行情与合约检查使用回放时的当前状态，不代表发出信号时的状态，也不代表可以实盘成交。'
              : 'Market and contract checks use replay-time state, not historical state or live execution authorization.'}
          </div>
          {item.evaluations.map((ev, index) => (
            <div key={index} className="space-y-1">
              <div className="font-mono">
                {ev.action} {ev.canonical || ev.symbol} ·{' '}
                {new Date(ev.checked_at).toLocaleString()}
                {ev.market_price !== undefined &&
                  ` · ${zh ? '当前价格' : 'Current price'}: ${ev.market_price}`}
              </div>
              {(
                [
                  ['source', zh ? '来源证据' : 'Source evidence'],
                  ['parameters', zh ? '原文参数' : 'Author parameters'],
                  ['market', zh ? '当前行情' : 'Current market'],
                  ['contract', zh ? '当前合约' : 'Current contract'],
                ] as const
              ).map(([key, label]) => (
                <div key={key} className="break-words">
                  <span className="font-semibold">
                    {label}: {names[ev[key].status]}
                  </span>
                  {ev[key].code && ` · ${ev[key].code}`}
                  {ev[key].detail && ` — ${ev[key].detail}`}
                </div>
              ))}
              {ev.entry_check && (
                <div>
                  {zh ? '入场规则预览' : 'Entry policy preview'}:{' '}
                  {names[ev.entry_check.status]}
                  {ev.entry_check.detail && ` — ${ev.entry_check.detail}`}
                </div>
              )}
              {ev.entry_plan && (
                <div className="space-y-1">
                  <div>
                    {entryPolicyLabel(ev.entry_plan.entry_policy, language)}
                  </div>
                  <div>
                    {ev.entry_plan.decision.order_type} @{' '}
                    {ev.entry_plan.decision.entry_price}
                    {ev.entry_plan.second_limit
                      ? ` + LIMIT @ ${ev.entry_plan.second_limit} (${zh ? '各 50% 风险预算' : '50% risk each'})`
                      : ` (${zh ? '单笔' : 'one leg'})`}
                  </div>
                  <div>
                    {zh ? '不利偏移' : 'Adverse deviation'}:{' '}
                    {ev.entry_plan.decision.adverse_deviation_pct?.toFixed(4) ??
                      '—'}
                    %{' · '}
                    {zh ? '当次配置阈值' : 'Configured threshold'}:{' '}
                    {ev.entry_plan.decision.threshold_pct}%
                    {ev.entry_plan.decision.threshold_applied === false &&
                      (zh ? '（本策略不用于拦截）' : ' (not used as a gate)')}
                  </div>
                  <div>
                    {zh
                      ? '仅预览订单形态；未核验资金、数量、保护及实时执行条件，不会下单。'
                      : 'Order shape only; balance, quantity, protection and live execution gates are not evaluated. No orders are placed.'}
                  </div>
                </div>
              )}
            </div>
          ))}
        </>
      )}
    </div>
  )
}
