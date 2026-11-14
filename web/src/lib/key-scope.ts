import type { Account, KeyScopeInput } from './types'

export function isScopeAccountAvailable(account: Pick<Account, 'status'>): boolean {
  return ['active', 'needs_reauth', 'suspended'].includes(account.status)
}

export function hasUsableKeyScope(scope: KeyScopeInput, accounts: Pick<Account, 'id' | 'status'>[]): boolean {
  return scope.mode === 'all' || accounts.some(account => scope.accounts.includes(account.id) && isScopeAccountAvailable(account))
}

export function toKeyScopeInput(scope: KeyScopeInput, accounts: Pick<Account, 'id' | 'status'>[]): KeyScopeInput {
  return { mode: scope.mode, accounts: scope.mode === 'all' ? [] : scope.accounts.filter(id => accounts.some(account => account.id === id && isScopeAccountAvailable(account))) }
}
