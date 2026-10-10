import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { CopyTradeReplayChecks } from './CopyTradeReplayChecks'
import { CopyTradeRecognitionStatus } from './CopyTradeRecognitionStatus'
import type { CopyTradeReplayItem, SystemStatus } from '../../types'

const base: CopyTradeReplayItem = {
  message_id: 'one',
  timestamp: '',
  author: '',
  excerpt: '',
  image_count: 0,
  images_sent: 0,
  llm_ms: 0,
  verdict: 'SKIP',
}
describe('recognition evidence display', () => {
  it('separates valid parameters, failed current market and unavailable contracts', () => {
    render(
      <CopyTradeReplayChecks
        language="zh"
        item={{
          ...base,
          processing_path: 'deterministic',
          processing_model: 'deterministic:tyler_v1',
          evaluations: [
            {
              action: 'OPEN',
              symbol: 'RAY',
              checked_at: '2026-10-10T10:00:00Z',
              market_price: 2,
              source: { status: 'passed' },
              parameters: { status: 'passed' },
              market: { status: 'failed', detail: 'TP crossed' },
              contract: {
                status: 'unavailable',
                code: 'CONTRACT_CHECK_FAILED',
              },
            },
          ],
        }}
      />
    )
    expect(screen.getByText('原文参数: 通过')).toBeInTheDocument()
    expect(screen.getByText('当前行情: 未通过')).toBeInTheDocument()
    expect(screen.getByText('当前合约: 查询不可用')).toBeInTheDocument()
    expect(screen.getByText(/确定性规则/)).toBeInTheDocument()
    expect(screen.getByText(/不代表发出信号时的状态/)).toBeInTheDocument()
  })
  it('does not infer old replay evidence', () => {
    render(<CopyTradeReplayChecks language="zh" item={base} />)
    expect(screen.getByText(/未保存分层检查证据/)).toBeInTheDocument()
  })
  it('labels retained AI calls and rule runs without a cycle count', () => {
    render(
      <CopyTradeRecognitionStatus
        status={
          {
            interpretation_model: 'gpt-test',
            recognition_stats: {
              model_calls: 3,
              deterministic_runs: 2,
              recognition_runs: 5,
            },
          } as SystemStatus
        }
      />
    )
    expect(screen.getByText(/AI 调用 3/)).toBeInTheDocument()
    expect(screen.getByText(/规则处理 2/)).toBeInTheDocument()
    expect(screen.getByText(/保留记录/)).toBeInTheDocument()
  })
})
