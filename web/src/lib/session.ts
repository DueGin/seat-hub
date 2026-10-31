import { useQuery } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'
import type { PublicConfig, User } from '@/lib/types'

export function useSession() {
  return useQuery({ queryKey: ['session'], queryFn: async () => { try { return (await api.get<{ user: User }>('/auth/me')).user } catch (error) { if (error instanceof ApiError && error.status === 401) return null; throw error } }, retry: false, staleTime: 60_000 })
}
export function useConfig() { return useQuery({ queryKey: ['public-config'], queryFn: () => api.get<PublicConfig>('/public/config'), staleTime: 60_000 }) }
