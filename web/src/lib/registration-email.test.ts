import assert from 'node:assert/strict'
import { test } from 'node:test'
import { normalizeRegistrationWhitelist, registrationEmailAllowed, registrationDomainHint, registrationEmail } from './registration-email.ts'

test('注册邮箱白名单匹配精确域名、子域名和完整地址', () => {
  const entries = normalizeRegistrationWhitelist([' QQ.com ', 'qq.COM', '', '*.edu.cn', 'Someone@gmail.com'])
  assert.deepEqual(entries, ['qq.com', '*.edu.cn', 'someone@gmail.com'])
  for (const [email, allowed] of [['a@qq.com', true], ['a@mail.qq.com', false], ['a@school.edu.cn', true], ['a@mail.school.edu.cn', true], ['a@edu.cn', false], ['a@notedu.cn', false], [' Someone@GMAIL.com ', true], ['other@gmail.com', false]] as const) assert.equal(registrationEmailAllowed(email, entries), allowed, email)
  assert.equal(registrationEmailAllowed('any@example.com', []), true)
  assert.equal(registrationDomainHint(['qq.com', '*.edu.cn']), '仅支持以下邮箱注册：qq.com、edu.cn 的子域名')
  assert.equal(registrationDomainHint([]), undefined)
  assert.equal(registrationEmail('a@example.com'), true)
  for (const value of ['bad', 'a@@b.com', 'Name <a@gmail.com>', 'a@gmail.com,b@gmail.com', 'a@b.com\nBcc:x@y.com']) assert.equal(registrationEmail(value), false)
})

test('演示注册支持 SMTP 门禁、验证码、白名单和一次消费', async () => {
  const values = new Map<string, string>()
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, value: { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => values.set(key, value) } })
  const { mockRequest } = await import('./mock.ts')
  const request = async (path: string, method = 'GET', body?: unknown) => {
    const response = await mockRequest(path, method, body)
    return { status: response.status, body: await response.json() }
  }
  const expectError = async (path: string, method: string, body: unknown, status: number, code: string) => {
    const result = await request(path, method, body)
    assert.equal(result.status, status, JSON.stringify(result.body))
    assert.equal(result.body.error.code, code)
  }
  await expectError('/admin/settings', 'PUT', { register_email_verification: true }, 422, 'invalid_settings')
  await expectError('/auth/register/code', 'POST', { email: 'a@qq.com' }, 409, 'verification_disabled')
  await request('/admin/settings/mail', 'PUT', { enabled: true, host: 'smtp.example.com', port: 587, security: 'starttls', from_address: 'sender@example.com', admin_emails: [] })
  const saved = await request('/admin/settings', 'PUT', { register_email_verification: true, register_email_whitelist: [' QQ.COM ', '*.edu.cn', 'secret@gmail.com'] })
  assert.equal(saved.status, 200)
  const config = await request('/public/config')
  assert.equal(config.body.data.register_email_verification, true)
  assert.deepEqual(config.body.data.register_email_domains, ['qq.com', '*.edu.cn'])
  assert.ok(!JSON.stringify(config.body).includes('secret'))
  await expectError('/auth/register/code', 'POST', { email: 'bad@gmail.com' }, 422, 'email_not_allowed')
  await expectError('/auth/register', 'POST', { email: 'a@qq.com', password: 'password-123' }, 422, 'code_required')
  const issued = await request('/auth/register/code', 'POST', { email: 'a@qq.com' })
  assert.equal(issued.status, 200)
  assert.equal(issued.body.data.cooldown_seconds, 60)
  await expectError('/auth/register/code', 'POST', { email: 'a@qq.com' }, 429, 'code_cooldown')
  await expectError('/auth/register', 'POST', { email: 'a@qq.com', password: 'password-123', code: '111111' }, 422, 'code_invalid')
  assert.equal((await request('/auth/register', 'POST', { email: 'a@qq.com', password: 'password-123', code: '123456' })).status, 201)
  await expectError('/auth/register', 'POST', { email: 'a@qq.com', password: 'password-123', code: '123456' }, 422, 'code_expired')
  await expectError('/auth/register/code', 'POST', { email: 'a@qq.com' }, 409, 'email_taken')
  assert.equal((await request('/auth/login', 'POST', { email: 'demo@example.com', password: 'demo123456' })).status, 200)
  assert.equal((await request('/admin/settings', 'PUT', { register_email_verification: false })).status, 200)
  await expectError('/auth/register', 'POST', { email: 'blocked@example.com', password: 'password-123' }, 422, 'email_not_allowed')
  const created = await request('/admin/users', 'POST', { email: 'admin-created@blocked.com', password: 'password-123' })
  assert.equal(created.status, 201)
})
