import { createContext, useContext, useState, ReactNode } from 'react'
import type { Language } from '../i18n/translations'

interface LanguageContextType {
  language: Language
  setLanguage: (lang: Language) => void
}

const LanguageContext = createContext<LanguageContextType | undefined>(
  undefined
)

const VALID_LANGUAGES: Language[] = ['en', 'zh', 'id']
const DEFAULT_LANGUAGE: Language = 'zh'

// A dedicated storage key: the legacy `language` key was force-written to 'en'
// on every visit while the UI was English-only, so its value carries no user
// intent and must not be read back.
const STORAGE_KEY = 'nofx_language'

export function LanguageProvider({ children }: { children: ReactNode }) {
  const [language, setLanguageState] = useState<Language>(() => {
    const stored = localStorage.getItem(STORAGE_KEY) as Language | null
    if (stored && VALID_LANGUAGES.includes(stored)) {
      return stored
    }
    localStorage.setItem(STORAGE_KEY, DEFAULT_LANGUAGE)
    return DEFAULT_LANGUAGE
  })

  const handleSetLanguage = (lang: Language) => {
    if (!VALID_LANGUAGES.includes(lang)) return
    localStorage.setItem(STORAGE_KEY, lang)
    setLanguageState(lang)
  }

  return (
    <LanguageContext.Provider
      value={{ language, setLanguage: handleSetLanguage }}
    >
      {children}
    </LanguageContext.Provider>
  )
}

export function useLanguage() {
  const context = useContext(LanguageContext)
  if (!context) {
    throw new Error('useLanguage must be used within LanguageProvider')
  }
  return context
}
