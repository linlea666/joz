import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { LanguageProvider, useLanguage } from './LanguageContext'
import { Header } from '../components/common/Header'

function LanguageProbe() {
  const { language, setLanguage } = useLanguage()
  return (
    <div>
      <div data-testid="lang">{language}</div>
      <button type="button" onClick={() => setLanguage('en')}>
        to-en
      </button>
    </div>
  )
}

describe('Chinese-default language policy', () => {
  beforeEach(() => localStorage.clear())

  it('defaults to Chinese and persists the default under the new storage key', async () => {
    render(
      <LanguageProvider>
        <LanguageProbe />
      </LanguageProvider>
    )

    expect(screen.getByTestId('lang').textContent).toBe('zh')
    await waitFor(() => expect(localStorage.getItem('nofx_language')).toBe('zh'))
  })

  it('ignores the legacy `language` key that was force-written while the UI was English-only', () => {
    localStorage.setItem('language', 'en')

    render(
      <LanguageProvider>
        <LanguageProbe />
      </LanguageProvider>
    )

    expect(screen.getByTestId('lang').textContent).toBe('zh')
  })

  it('respects a valid stored preference', () => {
    localStorage.setItem('nofx_language', 'en')

    render(
      <LanguageProvider>
        <LanguageProbe />
      </LanguageProvider>
    )

    expect(screen.getByTestId('lang').textContent).toBe('en')
  })

  it('falls back to Chinese when the stored preference is invalid', () => {
    localStorage.setItem('nofx_language', 'fr')

    render(
      <LanguageProvider>
        <LanguageProbe />
      </LanguageProvider>
    )

    expect(screen.getByTestId('lang').textContent).toBe('zh')
  })

  it('setLanguage switches the language and persists it', async () => {
    render(
      <LanguageProvider>
        <LanguageProbe />
      </LanguageProvider>
    )

    screen.getByRole('button', { name: 'to-en' }).click()

    await waitFor(() => expect(screen.getByTestId('lang').textContent).toBe('en'))
    expect(localStorage.getItem('nofx_language')).toBe('en')
  })

  it('does not render a language switcher in the header', () => {
    render(
      <LanguageProvider>
        <Header simple />
      </LanguageProvider>
    )

    expect(screen.queryByRole('button', { name: 'Chinese' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'EN' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'ID' })).toBeNull()
  })
})
