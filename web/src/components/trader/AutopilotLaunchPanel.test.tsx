import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter, useLocation } from 'react-router-dom'
import type { AIModel, Exchange } from '../../types'
import { AutopilotLaunchPanel } from './AutopilotLaunchPanel'

const mocks = vi.hoisted(() => ({
  getCurrentBeginnerWallet: vi.fn(),
  runLaunchPreflight: vi.fn(),
}))

vi.mock('../../lib/api', () => ({
  api: { getCurrentBeginnerWallet: mocks.getCurrentBeginnerWallet },
}))
vi.mock('../../lib/launch/preflight', () => ({
  runLaunchPreflight: mocks.runLaunchPreflight,
}))

function Location() {
  const location = useLocation()
  return <output>{location.pathname + location.search}</output>
}

function setup(connected = false, onOpenHyperliquidConfig?: () => void) {
  render(
    <MemoryRouter initialEntries={['/traders']}>
      <AutopilotLaunchPanel
        models={[
          {
            id: 'model',
            provider: 'claw402',
            enabled: true,
            walletAddress: '0xtest-wallet',
            balanceUsdc: '2',
          } as AIModel,
        ]}
        exchanges={
          connected
            ? [
                {
                  id: 'exchange',
                  exchange_type: 'hyperliquid',
                  enabled: true,
                  hyperliquidWalletAddr: '0xtest-account',
                  hyperliquidBuilderApproved: true,
                } as Exchange,
              ]
            : []
        }
        exchangeAccountStates={{}}
        isLoggedIn
        language="en"
        onRefresh={async () => {}}
        onOpenHyperliquidConfig={onOpenHyperliquidConfig}
      />
      <Location />
    </MemoryRouter>
  )
}

describe('autopilot setup entry', () => {
  beforeEach(() => {
    mocks.getCurrentBeginnerWallet.mockReset()
    mocks.getCurrentBeginnerWallet.mockResolvedValue(null)
    mocks.runLaunchPreflight.mockReset()
    mocks.runLaunchPreflight.mockResolvedValue({ checks: [] })
  })

  it.each([false, true])(
    'keeps wallet setup out of the page (connected: %s)',
    async (connected) => {
      setup(connected)
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled()
      )

      expect(screen.queryByText('Hyperliquid setup')).not.toBeInTheDocument()
      expect(
        screen.queryByRole('heading', {
          name: 'Connect Hyperliquid',
          exact: true,
        })
      ).not.toBeInTheDocument()
      expect(
        screen.queryByRole('button', { name: 'Connect wallet', exact: true })
      ).not.toBeInTheDocument()
      expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
      expect(
        screen.getByRole('heading', { name: 'Step 2 · Connect Hyperliquid' })
      ).toBeInTheDocument()
    }
  )

  it.each(['Open', 'Connect Hyperliquid'])(
    'opens the existing configuration dialog from %s',
    async (name) => {
      const open = vi.fn()
      setup(false, open)
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled()
      )
      fireEvent.click(screen.getByRole('button', { name, exact: true }))
      expect(open).toHaveBeenCalledOnce()
      expect(screen.getByRole('status')).toHaveTextContent('/traders')
    }
  )

  it.each(['Open', 'Connect Hyperliquid'])(
    'uses the existing setup route when %s has no dialog callback',
    async (name) => {
      setup()
      await waitFor(() =>
        expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled()
      )
      fireEvent.click(screen.getByRole('button', { name, exact: true }))
      expect(screen.getByRole('status')).toHaveTextContent(
        '/traders?setup=hyperliquid'
      )
    }
  )
})
