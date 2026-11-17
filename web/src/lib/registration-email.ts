// Keep the client policy aligned with signup.NormalizeWhitelist and net/mail bare addresses.
export function registrationEmail(value: string): boolean {
  if (new TextEncoder().encode(value).length > 254) return false
  const parts = value.split('@')
  if (parts.length !== 2) return false
  const [local, domain] = parts
  const atom = /^[a-z0-9!#$%&'*+\/=?^_`{|}~\-\u0080-\u{10ffff}]+$/iu
  const dotAtom = (part: string) => part.split('.').every(label => atom.test(label))
  if (!dotAtom(local)) return false
  if (domain.startsWith('[') && domain.endsWith(']')) {
    const ip = domain.slice(1, -1)
    if (!ip.includes(':')) return ip.split('.').length === 4 && ip.split('.').every(part => /^(0|[1-9][0-9]{0,2})$/.test(part) && Number(part) <= 255)
    try { return Boolean(new URL(`http://${domain}`).hostname) && !ip.includes('%') } catch { return false }
  }
  return dotAtom(domain)
}
function hostname(value: string): boolean {
  return value.length <= 253 && value.includes('.') && value.split('.').every(label => label.length <= 63 && /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label))
}
export function normalizeRegistrationWhitelist(entries: string[]): string[] {
  const result = [...new Set(entries.map(entry => entry.trim().toLowerCase()).filter(Boolean))]
  if (result.length > 200) throw new Error('注册邮箱白名单最多 200 项')
  for (const entry of result) {
    if (entry.includes('@') ? !registrationEmail(entry) : !hostname(entry.replace(/^\*\./, ''))) throw new Error(`白名单${entry.includes('@') ? '邮箱' : '域名'}格式不正确：${entry}`)
  }
  return result
}
export function registrationEmailAllowed(email: string, entries: string[]): boolean {
  if (!entries.length) return true
  email = email.trim().toLowerCase()
  const domain = email.slice(email.lastIndexOf('@') + 1)
  return entries.some(entry => entry.includes('@') ? entry === email : entry.startsWith('*.') ? domain.endsWith(entry.slice(1)) : domain === entry)
}
export function registrationDomainHint(domains: string[]): string | undefined {
  if (!domains.length) return undefined
  return `仅支持以下邮箱注册：${domains.map(domain => domain.startsWith('*.') ? `${domain.slice(2)} 的子域名` : domain).join('、')}`
}
