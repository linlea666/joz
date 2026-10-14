import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { vi, it, expect } from 'vitest'
import { RecoveryReview } from './RecoveryReview'
import { CopyTradeTiming } from './CopyTradeTiming'
import type { CopyTradeSignal, CopyTradeAIRun } from '../../types'
const review = vi.fn()
vi.mock('../../lib/api', () => ({
  api: { reviewCopyRecovery: (...args: unknown[]) => review(...args) },
}))
it('requires preview and a reason before local closure', async () => {
  review
    .mockResolvedValueOnce({
      id: 'ticket',
      expires_at: new Date(Date.now() + 60000).toISOString(),
      evidence_json: '{"position_quantity":0}',
    })
    .mockResolvedValueOnce({ confirmed_at: 'now' })
  const done = vi.fn()
  render(<RecoveryReview contextId="context" onComplete={done} />)
  fireEvent.click(screen.getByText('核对并结束本地恢复'))
  await screen.findByRole('dialog')
  expect(review).toHaveBeenCalledWith('context', undefined)
  expect(screen.getByText('确认结束本地恢复')).toBeDisabled()
  fireEvent.change(screen.getByLabelText('核对原因'), {
    target: { value: '已核对' },
  })
  fireEvent.click(screen.getByText('确认结束本地恢复'))
  await waitFor(() => expect(done).toHaveBeenCalled())
  expect(review).toHaveBeenLastCalledWith('context', {
    ticket: 'ticket',
    reason: '已核对',
  })
})
it('does not present last-attempt duration as end-to-end latency', () => {
  const signal = {
    status: 'executed',
    end_to_end_ms: 529000,
    total_ms: 131000,
  } as CopyTradeSignal
  const run = {
    provider: 'ai',
    attempts: [
      { id: 1, attempt: 1, status: 'failed', duration_ms: 120000 },
      { id: 2, attempt: 2, status: 'failed', duration_ms: 120000 },
      { id: 3, attempt: 3, status: 'completed', duration_ms: 119000 },
    ],
  } as CopyTradeAIRun
  render(<CopyTradeTiming signal={signal} run={run} />)
  expect(screen.getByText(/529.0 秒/)).toBeInTheDocument()
  expect(screen.getByText(/359.0 秒/)).toBeInTheDocument()
})
