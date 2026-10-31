import { useState } from 'react'
import type { FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Plus, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import { api, isMock, safeExternalUrl } from '@/lib/api'
import { date, dateTime, number, usd } from '@/lib/format'
import type { ApiKey, DailyUsage, ModelUsage, PublicConfig } from '@/lib/types'
import {
  Badge, Button, Card, CopyButton, EmptyState, Field, FieldInput, Modal,
  Notice, PageHeader, QueryState, StatCard,
} from '@/components/ui'

type KeyStats = { daily: DailyUsage[]; by_model: ModelUsage[] }
type KeyAction = { type: 'toggle' | 'delete'; key: ApiKey }
type ExampleTab = 'curl' | 'claude' | 'openai'

function shellQuote(value: string) {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

function connectionExamples(baseUrl: string): Record<ExampleTab, string> {
  return {
    curl: `export PORTAL_API_KEY='替换为你的 API Key'

# List available models first.
curl ${shellQuote(`${baseUrl}/v1/models`)} \\
  -H "Authorization: Bearer $PORTAL_API_KEY"

curl ${shellQuote(`${baseUrl}/v1/chat/completions`)} \\
  -H "Authorization: Bearer $PORTAL_API_KEY" \\
  -H 'Content-Type: application/json' \\
  -d '{"model":"替换为可用模型","messages":[{"role":"user","content":"你好"}]}'`,
    claude: `export ANTHROPIC_BASE_URL=${shellQuote(baseUrl)}
export ANTHROPIC_AUTH_TOKEN='替换为你的 API Key'
claude`,
    openai: `# pip install openai
# Set PORTAL_API_KEY in your environment before running.
import os
from openai import OpenAI

client = OpenAI(
    api_key=os.environ["PORTAL_API_KEY"],
    base_url=${JSON.stringify(`${baseUrl}/v1`)},
)

response = client.chat.completions.create(
    model="替换为可用模型",
    messages=[{"role": "user", "content": "你好"}],
)
print(response.choices[0].message.content)`,
  }
}

export function KeysPage() {
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState('')
  const [created, setCreated] = useState<{ name: string; secret: string } | null>(null)
  const [action, setAction] = useState<KeyAction | null>(null)
  const [statsKey, setStatsKey] = useState<ApiKey | null>(null)
  const [tab, setTab] = useState<ExampleTab>('curl')

  const keysQuery = useQuery({ queryKey: ['keys'], queryFn: () => api.get<{ items: ApiKey[] }>('/keys') })
  const configQuery = useQuery({ queryKey: ['public-config'], queryFn: () => api.get<PublicConfig>('/public/config') })
  const statsQuery = useQuery({
    queryKey: ['key-stats', statsKey?.id],
    queryFn: () => {
      if (!statsKey) throw new Error('请先选择一个密钥')
      return api.get<KeyStats>(`/keys/${statsKey.id}/stats`)
    },
    enabled: statsKey !== null,
  })

  const createKey = useMutation({
    mutationFn: async (keyName: string) => {
      const result = await api.post<{ key: ApiKey; secret: string }>('/keys', { name: keyName })
      setCreated({ name: result.key.name, secret: result.secret })
      return result.key
    },
    onSuccess: () => {
      setCreateOpen(false)
      setName('')
      void queryClient.invalidateQueries({ queryKey: ['keys'] })
      void queryClient.invalidateQueries({ queryKey: ['overview'] })
    },
  })
  const changeKey = useMutation({
    mutationFn: async (next: KeyAction) => {
      if (next.type === 'delete') await api.delete(`/keys/${next.key.id}`)
      else await api.patch(`/keys/${next.key.id}`, { status: next.key.status === 'active' ? 'disabled' : 'active' })
    },
    onSuccess: (_, next) => {
      setAction(null)
      toast.success(next.type === 'delete' ? '密钥已删除' : next.key.status === 'active' ? '密钥已停用' : '密钥已启用')
      void queryClient.invalidateQueries({ queryKey: ['keys'] })
      void queryClient.invalidateQueries({ queryKey: ['overview'] })
      if (next.type === 'delete') queryClient.removeQueries({ queryKey: ['key-stats', next.key.id] })
    },
  })

  const keys = keysQuery.data?.items ?? []
  const keyLimitReached = keys.length >= 20
  const baseUrl = (safeExternalUrl(configQuery.data?.base_url) ?? window.location.origin).replace(/\/+$/, '')
  const examples = connectionExamples(baseUrl)
  const exampleTabs: { id: ExampleTab; label: string }[] = [
    { id: 'curl', label: 'curl' }, { id: 'claude', label: 'Claude Code' }, { id: 'openai', label: 'OpenAI SDK' },
  ]
  const totals = (statsQuery.data?.daily ?? []).reduce(
    (total, item) => ({ requests: total.requests + item.requests, tokens: total.tokens + item.tokens, cost_usd: total.cost_usd + item.cost_usd }),
    { requests: 0, tokens: 0, cost_usd: 0 },
  )

  function openCreate() {
    createKey.reset()
    setName('')
    setCreateOpen(true)
  }

  function submitCreate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (name.trim() && !createKey.isPending) createKey.mutate(name.trim())
  }

  function openAction(next: KeyAction) {
    changeKey.reset()
    setAction(next)
  }

  return <div className="stack">
    <PageHeader title="API 密钥" description="为你的应用创建访问凭证，统一接入模型服务。" actions={
      <Button onClick={openCreate} disabled={keysQuery.isPending || keysQuery.isError || keyLimitReached}><Plus size={16} />创建密钥</Button>
    } />
    {isMock && <Notice>当前为演示环境，密钥与调用示例仅供体验，不能用于真实请求。</Notice>}
    {keyLimitReached && <Notice>已达到 20 个密钥的上限。请删除不再使用的密钥后再创建。</Notice>}

    <Card>
      <div className="section-heading"><div><h2>我的密钥</h2><p className="muted">明文仅在创建时显示一次，请妥善保管。</p></div><Button variant="ghost" aria-label="刷新密钥列表" disabled={keysQuery.isFetching} onClick={() => { void keysQuery.refetch() }}><RefreshCw size={16} /></Button></div>
      <QueryState query={keysQuery}>
        {!keys.length ? <EmptyState title="还没有 API 密钥" description="创建你的第一个密钥，即可在客户端中使用模型服务。" action={<Button onClick={openCreate}><KeyRound size={16} />创建密钥</Button>} /> : <div className="table-wrap"><table className="data-table">
          <thead><tr><th>名称 / 前缀</th><th>状态</th><th>创建时间</th><th>最近使用</th><th>操作</th></tr></thead>
          <tbody>{keys.map((key) => <tr key={key.id}>
            <td><strong>{key.name}</strong><div className="mono muted">{key.prefix}…</div></td>
            <td><Badge status={key.status}>{key.status === 'active' ? '已启用' : '已停用'}</Badge></td>
            <td>{dateTime(key.created_at)}</td><td>{key.last_used_at ? dateTime(key.last_used_at) : '尚未使用'}</td>
            <td><div className="row"><Button variant="ghost" onClick={() => setStatsKey(key)}>用量</Button><Button variant="secondary" disabled={changeKey.isPending} onClick={() => openAction({ type: 'toggle', key })}>{key.status === 'active' ? '停用' : '启用'}</Button><Button variant="danger" disabled={changeKey.isPending} onClick={() => openAction({ type: 'delete', key })}>删除</Button></div></td>
          </tr>)}</tbody>
        </table></div>}
      </QueryState>
    </Card>

    <Card className="stack">
      <div className="section-heading"><div><h2>接入你的应用</h2><p className="muted">使用网关地址和 API Key，选择适合你的客户端。</p></div></div>
      {configQuery.isError && <Notice><p>站点配置暂时不可用，以下示例使用当前站点地址。请确认地址后再接入。</p><Button variant="ghost" onClick={() => { void configQuery.refetch() }}>重试读取配置</Button></Notice>}
      {configQuery.isPending && <p role="status" className="muted">正在读取网关配置，暂时显示当前站点地址…</p>}
      <div className="toolbar"><div><span className="muted">网关地址</span><p className="mono">{baseUrl}</p></div><CopyButton value={baseUrl} label="复制地址" /></div>
      <div className="tabs" role="tablist" aria-label="客户端接入示例">{exampleTabs.map((item) => <button key={item.id} id={`key-example-tab-${item.id}`} type="button" className={tab === item.id ? 'active' : ''} role="tab" aria-selected={tab === item.id} aria-controls="key-example-panel" onClick={() => setTab(item.id)}>{item.label}</button>)}</div>
      <div id="key-example-panel" role="tabpanel" aria-labelledby={`key-example-tab-${tab}`} className="stack">
        <p className="muted">请将示例中的密钥和模型占位文字替换为实际值。可用模型可通过 curl 示例中的模型列表接口查询。</p>
        <pre className="code-block"><code>{examples[tab]}</code></pre>
        <div className="row"><CopyButton value={examples[tab]} label="复制示例" /></div>
      </div>
      <p className="muted">密钥继承你的可用资源与并发额度；请避免在浏览器前端代码或公开仓库中暴露密钥。</p>
    </Card>

    <Modal open={createOpen} title="创建 API 密钥" onClose={() => { if (!createKey.isPending) setCreateOpen(false) }}>
      <form className="stack" onSubmit={submitCreate}>
        <Field label="密钥名称" hint="用应用名称区分不同密钥，例如：开发环境、Claude Code。"><FieldInput autoFocus required value={name} onChange={(event) => setName(event.target.value)} placeholder="为这个密钥命名" disabled={createKey.isPending} /></Field>
        <Notice>创建后会展示一次完整密钥。关闭窗口后无法再次查看，请及时复制并安全保存。</Notice>
        {createKey.isError && <p role="alert">{createKey.error.message}</p>}
        <div className="toolbar"><Button type="button" variant="secondary" disabled={createKey.isPending} onClick={() => setCreateOpen(false)}>取消</Button><Button type="submit" disabled={!name.trim() || createKey.isPending || keyLimitReached}>{createKey.isPending ? '正在创建…' : '创建密钥'}</Button></div>
      </form>
    </Modal>

    <Modal open={created !== null} title="密钥已创建，请立即保存" onClose={() => setCreated(null)}>
      {created && <div className="stack">
        <Notice>这是「{created.name}」的完整密钥，仅在本次窗口中显示。关闭后无法找回，请勿分享给他人。</Notice>
        <pre className="code-block"><code>{created.secret}</code></pre>
        <div className="toolbar"><CopyButton value={created.secret} label="复制完整密钥" /><Button onClick={() => setCreated(null)}>我已保存，关闭</Button></div>
      </div>}
    </Modal>

    <Modal open={action !== null} title={action?.type === 'delete' ? '删除密钥' : action?.key.status === 'active' ? '停用密钥' : '启用密钥'} onClose={() => { if (!changeKey.isPending) setAction(null) }}>
      {action && <div className="stack">
        <p>确认{action.type === 'delete' ? '删除' : action.key.status === 'active' ? '停用' : '启用'}「{action.key.name}」（<span className="mono">{action.key.prefix}…</span>）？</p>
        <Notice>{action.type === 'delete' ? '删除后此密钥将失效且无法恢复，使用它的应用需要更新配置。' : action.key.status === 'active' ? '停用后，使用此密钥的新请求将被拒绝。之后可以重新启用。' : '启用后，应用可再次使用此密钥发起请求。'}</Notice>
        {changeKey.isError && <p role="alert">{changeKey.error.message}</p>}
        <div className="toolbar"><Button variant="secondary" disabled={changeKey.isPending} onClick={() => setAction(null)}>取消</Button><Button variant={action.type === 'delete' ? 'danger' : 'primary'} disabled={changeKey.isPending} onClick={() => changeKey.mutate(action)}>{changeKey.isPending ? '正在处理…' : '确认操作'}</Button></div>
      </div>}
    </Modal>

    <Modal open={statsKey !== null} title={`密钥用量${statsKey ? ` · ${statsKey.name}` : ''}`} wide onClose={() => setStatsKey(null)}>
      <QueryState query={statsQuery}>
        <div className="stack">
          <p className="muted">最近 30 天的请求与费用统计，费用以美元展示。</p>
          {!statsQuery.data?.daily.length && !statsQuery.data?.by_model.length ? <EmptyState title="这个密钥暂无用量" description="使用此密钥发起请求后，可在这里查看最近 30 天的统计。" /> : <>
            <div className="metric-grid"><StatCard label="请求数" value={number(totals.requests)} /><StatCard label="令牌数" value={number(totals.tokens)} /><StatCard label="费用" value={usd(totals.cost_usd)} /></div>
            <h3>按模型统计</h3>
            {statsQuery.data?.by_model.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>模型</th><th>请求数</th><th>令牌数</th><th>费用</th></tr></thead><tbody>{statsQuery.data.by_model.map((item) => <tr key={item.model}><td className="mono">{item.model}</td><td>{number(item.requests)}</td><td>{number(item.tokens)}</td><td>{usd(item.cost_usd)}</td></tr>)}</tbody></table></div> : <p className="muted">暂无模型统计。</p>}
            <h3>每日用量</h3>
            {statsQuery.data?.daily.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>日期</th><th>请求数</th><th>令牌数</th><th>费用</th></tr></thead><tbody>{statsQuery.data.daily.map((item) => <tr key={item.date}><td>{date(`${item.date}T00:00:00`)}</td><td>{number(item.requests)}</td><td>{number(item.tokens)}</td><td>{usd(item.cost_usd)}</td></tr>)}</tbody></table></div> : <p className="muted">暂无每日统计。</p>}
          </>}
        </div>
      </QueryState>
    </Modal>
  </div>
}
