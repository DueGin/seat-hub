import assert from 'node:assert/strict'
import { test } from 'node:test'
import { isTicketImage, safeMarkdownLink } from './markdown-policy.ts'

test('仅显示当前工单已知的附件图片', () => {
  assert.equal(isTicketImage('/api/tickets/attachments/12', [12, 13]), true)
  assert.equal(isTicketImage('/api/tickets/attachments/13', [12]), false)
  for (const src of ['https://tracker.example/image.png', '//tracker.example/a', 'data:image/png;base64,a', '/api/tickets/attachments/12?x=1', '/api/tickets/attachments/12#x', '/api/tickets/attachments/0', '/api/tickets/attachments/01', '/api/tickets/attachments/12/other', '/api/tickets/attachments/9007199254740993', undefined]) assert.equal(isTicketImage(src), false, String(src))
})
test('链接仅接受站内路径和常规安全协议', () => {
  assert.equal(safeMarkdownLink('https://example.com/a'), 'https://example.com/a')
  assert.equal(safeMarkdownLink('/tickets/1'), '/tickets/1')
  for (const src of ['javascript:alert(1)', 'data:text/html,hello', '//tracker.example/a', undefined]) assert.equal(safeMarkdownLink(src), undefined)
})
