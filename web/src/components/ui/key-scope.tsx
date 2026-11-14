import { useId } from 'react'
import type { Account, KeyScope, KeyScopeInput } from '@/lib/types'
import { hasUsableKeyScope, isScopeAccountAvailable } from '@/lib/key-scope'
import { Badge, Button, Notice } from '@/components/ui'

type ScopeAccount = Pick<Account, 'id' | 'label' | 'email_masked' | 'status'>

export function KeyScopeSummary({ scope, accounts }: { scope: KeyScope; accounts: ScopeAccount[] }) {
  return <div className="key-scope-summary">
    {scope.mode === 'all' ? <span title="包括以后新增的账号">全部账号</span> : <details>
      <summary>{scope.accounts.length} 个指定账号</summary>
      <ul>{scope.accounts.map(id => {
        const account = accounts.find(item => item.id === id)
        return <li key={id}>{account?.label || `账号 #${id}`}{account?.email_masked && <span className="muted mono"> · {account.email_masked}</span>}</li>
      })}</ul>
    </details>}
    {!scope.usable && <><div><Badge status="needs_reauth">无可用账号</Badge></div><p className="key-scope-hint text-amber">请重新选择可用账号后再调用。</p></>}
  </div>
}

export function KeyScopePicker({ value, onChange, accounts, disabled, loading, error, onRetry }: {
  value: KeyScopeInput
  onChange: (scope: KeyScopeInput) => void
  accounts: ScopeAccount[]
  disabled: boolean
  loading: boolean
  error: string | null
  onRetry: () => void
}) {
  const id = useId()
  const missingIds = value.accounts.filter(accountId => !accounts.some(account => account.id === accountId))
  const usable = hasUsableKeyScope(value, accounts)

  return <fieldset className="key-scope-field" disabled={disabled} aria-describedby={value.mode === 'all' || (!loading && !error) ? `${id}-hint` : undefined}>
    <legend>可调用的账号</legend>
    <div className="segmented segmented-2">
      <label className={value.mode === 'all' ? 'active' : ''}><input type="radio" name={id} value="all" checked={value.mode === 'all'} onChange={() => onChange({ ...value, mode: 'all' })} /><strong>全部账号</strong><small>包括以后新增的账号</small></label>
      <label className={value.mode === 'selected' ? 'active' : ''}><input type="radio" name={id} value="selected" checked={value.mode === 'selected'} onChange={() => onChange({ ...value, mode: 'selected' })} /><strong>指定账号</strong><small>仅调用勾选的账号</small></label>
    </div>
    {value.mode === 'selected' && <div className="key-scope-selection">
      {loading ? <p role="status" className="muted small">正在读取账号…</p> : error ? <div role="alert"><Notice><div><p>{error}</p><Button type="button" variant="ghost" onClick={onRetry}>重新读取账号</Button></div></Notice></div> : <>
        {accounts.length || missingIds.length ? <div className="key-scope-accounts" role="group" aria-label="选择账号">{accounts.map(account => {
          const available = isScopeAccountAvailable(account)
          return <label key={account.id} className={`key-scope-account ${available ? '' : 'unavailable'}`}>
            <input type="checkbox" checked={value.accounts.includes(account.id)} disabled={!available} onChange={(event) => onChange({ ...value, accounts: event.target.checked ? [...value.accounts, account.id] : value.accounts.filter(id => id !== account.id) })} />
            <span className="key-scope-account-text"><strong>{account.label || `账号 #${account.id}`}</strong><small className="mono">{account.email_masked || '暂无邮箱信息'}</small></span>
            <span className="key-scope-account-status"><Badge status={account.status} />{!available && <small>暂不可选</small>}</span>
          </label>
        })}{missingIds.map(accountId => <label key={accountId} className="key-scope-account unavailable"><input type="checkbox" checked disabled /><span className="key-scope-account-text"><strong>账号 #{accountId}</strong><small>账号已不可用</small></span><Badge status="released">不可用</Badge></label>)}</div> : <p className="muted small">暂无账号，可先添加账号，或选择全部账号。</p>}
        <p id={`${id}-hint`} className={`small ${usable ? 'muted' : 'text-amber'}`}>{usable ? '保存时会移除不可选账号，实际调用仍受账号状态与额度影响。' : '所选账号当前都不可用，请至少选择一个可用账号'}</p>
      </>}
    </div>}
    {value.mode === 'all' && <p id={`${id}-hint`} className="muted small">可调用你当前和以后新增的账号，不包含其他用户的账号。</p>}
  </fieldset>
}
