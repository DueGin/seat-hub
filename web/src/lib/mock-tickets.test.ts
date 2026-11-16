import assert from 'node:assert/strict'
import { test } from 'node:test'
import { ticketMock } from './mock-tickets.ts'
import type { MockKit } from './mock-kit.ts'
import type { User } from './types.ts'

test('演示工单支持隔离、已读、附件和关闭后重开', async () => {
  let actor: User = { id: 1, email: 'demo@example.com', role: 'admin', status: 'active', created_at: new Date().toISOString() }
  const kit: MockKit = { ok: (data, status = 200) => Response.json({ ok: true, data }, { status }), fail: (status, code, message) => { throw Response.json({ ok: false, error: { code, message } }, { status }) }, currentUser: () => actor, optionalUser: () => actor, users: () => [actor], iso: (offset = 0) => new Date(Date.now() + offset).toISOString() }
  async function request(path: string, method = 'GET', input = {}) {
    try { const url = new URL(path, 'https://example.com'); const response = await ticketMock(url.pathname.replace(/^\/api/, ''), method, url.searchParams, input, kit); assert.ok(response); return { status: response.status, ...(await response.json()) } } catch (error) { if (!(error instanceof Response)) throw error; return { status: error.status, ...(await error.json()) } }
  }
  let result = await request('/tickets/unread'); assert.equal(result.data.count, 1)
  await request('/tickets/1'); result = await request('/tickets/unread'); assert.equal(result.data.count, 0)
  const upload = await request('/tickets/attachments', 'POST', { name: '截图.png', size: 68, type: 'image/png' }); assert.equal(upload.status, 201)
  result = await request('/tickets', 'POST', { title: '测试问题', category: 'api', body: `![截图](${upload.data.url})`, attachment_ids: [upload.data.id] }); assert.equal(result.status, 201)
  const id = result.data.ticket.id
  result = await request(`/tickets/${id}/messages`, 'POST', { body: '重用', attachment_ids: [upload.data.id] }); assert.equal(result.status, 422)
  actor = { ...actor, id: 2, role: 'user' }
  for (const path of [`/tickets/${id}`, upload.data.url, '/admin/tickets']) assert.equal((await request(path)).status, 403)
  assert.equal((await request('/tickets')).data.total, 0)
  actor = { ...actor, id: 1, role: 'admin' }
  result = await request(`/admin/tickets/${id}/messages`, 'POST', { body: '请重试' }); assert.equal(result.data.ticket.status, 'replied')
  await request(`/tickets/${id}/close`, 'POST')
  result = await request(`/tickets/${id}/messages`, 'POST', { body: '还有问题' }); assert.equal(result.data.ticket.status, 'open')
  result = await request('/admin/tickets?status=open&category=api&q=测试&user_id=1'); assert.equal(result.data.total, 1); assert.equal(result.data.counts.open, 1)
})
