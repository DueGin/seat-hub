import assert from 'node:assert/strict'
import { test } from 'node:test'
import { hasUsableKeyScope, isScopeAccountAvailable, toKeyScopeInput } from './key-scope.ts'
import { mockRequest } from './mock.ts'

type Scope = { mode: 'all' | 'selected'; accounts: number[]; usable: boolean }
type MockKey = { id: number; name: string; status: string; scope: Scope }

async function request(path: string, method = 'GET', body?: unknown) {
  const response = await mockRequest(path, method, body)
  return { status: response.status, envelope: await response.json() }
}

test('账号选择仅接受运行中、待重新授权和已暂停状态', () => {
  for (const status of ['active', 'needs_reauth', 'suspended']) assert.equal(isScopeAccountAvailable({ status }), true)
  for (const status of ['provisioning', 'awaiting_auth', 'bootstrapping', 'failed', 'releasing', 'released', 'allocating', 'unknown', '']) assert.equal(isScopeAccountAvailable({ status }), false)
})

test('全部范围允许无账号，指定范围必须命中一个可用账号', () => {
  assert.equal(hasUsableKeyScope({ mode: 'all', accounts: [] }, []), true)
  assert.equal(hasUsableKeyScope({ mode: 'selected', accounts: [] }, [{ id: 1, status: 'active' }]), false)
  assert.equal(hasUsableKeyScope({ mode: 'selected', accounts: [1] }, [{ id: 2, status: 'active' }]), false)
  assert.equal(hasUsableKeyScope({ mode: 'selected', accounts: [1, 2] }, [{ id: 1, status: 'released' }, { id: 2, status: 'suspended' }]), true)
})

test('保存范围只发送勾选且可选的账号，清除已释放及缺失账号', () => {
  const accounts = [{ id: 1, status: 'active' }, { id: 2, status: 'released' }, { id: 3, status: 'failed' }, { id: 4, status: 'needs_reauth' }]
  assert.deepEqual(toKeyScopeInput({ mode: 'selected', accounts: [1, 2, 3, 4, 99] }, accounts), { mode: 'selected', accounts: [1, 4] })
  assert.deepEqual(toKeyScopeInput({ mode: 'all', accounts: [1] }, accounts), { mode: 'all', accounts: [] })
})

test('演示密钥展示全部、指定和无可用账号三种范围', async () => {
  const { envelope } = await request('/keys')
  const keys = envelope.data.items as MockKey[]
  assert.deepEqual(keys.find(key => key.id === 1)?.scope, { mode: 'all', accounts: [], usable: true })
  assert.deepEqual(keys.find(key => key.id === 2)?.scope, { mode: 'selected', accounts: [1, 4], usable: true })
  assert.deepEqual(keys.find(key => key.id === 3)?.scope, { mode: 'selected', accounts: [3], usable: false })
})

test('创建密钥默认全部账号，并接受指定账号', async () => {
  const all = await request('/keys', 'POST', { name: '默认范围' })
  assert.equal(all.status, 201)
  assert.deepEqual(all.envelope.data.key.scope, { mode: 'all', accounts: [], usable: true })
  const selected = await request('/keys', 'POST', { name: '指定范围', scope: { mode: 'selected', accounts: [1, 4] } })
  assert.equal(selected.status, 201)
  assert.deepEqual(selected.envelope.data.key.scope, { mode: 'selected', accounts: [1, 4], usable: true })
})

test('创建和修改指定范围拒绝空选或只有不可用账号', async () => {
  for (const [path, method] of [['/keys', 'POST'], ['/keys/1', 'PATCH']]) {
    for (const accounts of [[], [3]]) {
      const result = await request(path, method, { name: '无效范围', scope: { mode: 'selected', accounts } })
      assert.equal(result.status, 422)
      assert.equal(result.envelope.error.code, 'key_scope_empty')
      assert.equal(result.envelope.error.message, '所选账号当前都不可用，请至少选择一个可用账号')
    }
  }
  const { envelope } = await request('/keys')
  assert.equal((envelope.data.items as MockKey[]).find(key => key.id === 1)?.name, '开发环境')
})

test('范围拒绝越权账号、非法 ID 和未知模式', async () => {
  for (const [path, method] of [['/keys', 'POST'], ['/keys/1', 'PATCH']]) {
    for (const scope of [null, [], { mode: 'selected', accounts: [5] }, { mode: 'selected', accounts: ['1'] }, { mode: 'selected', accounts: [0] }, { mode: 'unknown', accounts: [1] }]) {
      const result = await request(path, method, { name: '无效范围', scope })
      assert.equal(result.status, 422)
      assert.equal(result.envelope.error.code, 'key_scope_invalid')
    }
  }
})

test('修改账号范围恢复可用性，普通更新保留范围', async () => {
  const selected = await request('/keys/3', 'PATCH', { scope: { mode: 'selected', accounts: [2, 3] } })
  assert.equal(selected.status, 200)
  assert.deepEqual(selected.envelope.data.key.scope, { mode: 'selected', accounts: [2, 3], usable: true })
  const renamed = await request('/keys/3', 'PATCH', { name: '重命名', status: 'disabled' })
  assert.deepEqual(renamed.envelope.data.key.scope, selected.envelope.data.key.scope)
  const all = await request('/keys/3', 'PATCH', { scope: { mode: 'all', accounts: [] } })
  assert.deepEqual(all.envelope.data.key.scope, { mode: 'all', accounts: [], usable: true })
})

test('账号释放后指定范围失效，管理员详情同步展示范围和上游用户', async () => {
  const created = await request('/keys', 'POST', { name: '释放回归', scope: { mode: 'selected', accounts: [4] } })
  const id = created.envelope.data.key.id
  assert.equal((await request('/admin/accounts/4/release', 'POST', {})).status, 200)
  const { envelope } = await request('/admin/users/1')
  assert.equal(envelope.data.user.vm2api_username, 'portal_1')
  assert.ok(envelope.data.user.vm2api_user_id)
  assert.ok(!('vm2api_pool_id' in envelope.data.user))
  assert.deepEqual((envelope.data.keys as MockKey[]).find(key => key.id === id)?.scope, { mode: 'selected', accounts: [4], usable: false })
  const { envelope: empty } = await request('/admin/users/3')
  assert.equal(empty.data.user.vm2api_username, '')
  assert.equal(empty.data.user.vm2api_user_id, '')
  const invalid = await request(`/keys/${id}`, 'PATCH', { scope: { mode: 'selected', accounts: [1, 4] } })
  assert.equal(invalid.status, 422)
  assert.equal(invalid.envelope.error.code, 'key_scope_invalid')
  const recovered = await request(`/keys/${id}`, 'PATCH', { scope: toKeyScopeInput({ mode: 'selected', accounts: [1, 4] }, envelope.data.accounts) })
  assert.equal(recovered.status, 200)
  assert.deepEqual(recovered.envelope.data.key.scope, { mode: 'selected', accounts: [1], usable: true })
})
