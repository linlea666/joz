import useSWR from 'swr'
import { api } from '../lib/api'

export function useCopyTradeProfiles(enabled: boolean) {
  const { data, error } = useSWR(enabled ? 'copytrade-author-presets' : null,
    () => api.getCopyTradeProfiles(), { revalidateOnFocus: false })
  return { profiles: data ?? [], error }
}
