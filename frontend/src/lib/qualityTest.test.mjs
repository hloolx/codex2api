import assert from 'node:assert/strict'
import test from 'node:test'
import { extractQualityTestHTML, qualityTestPreviewDocument, isQualityTestActive, qualityTestPlanTone, qualityTestStatusKey, clampQualityTestFrameHeight, QUALITY_TEST_SIZE_SCRIPT, qualityTestFilterQuery } from './qualityTest.ts'
import { readClaudeTestEvents } from './claudeConnectionTest.ts'

test('quality preview extracts documents and SVG without rendering explanatory prose', () => {
  const html = '<!DOCTYPE html><html><body><svg><text>鹈鹕</text></svg></body></html>'
  assert.equal(extractQualityTestHTML(html), html)
  assert.equal(extractQualityTestHTML(`Here is the result:\n\`\`\`html\n${html}\n\`\`\`\nDone.`), html)
  assert.equal(extractQualityTestHTML(`Example:\n${html}\nExplanations.`), html)
  assert.equal(extractQualityTestHTML('```svg\n<svg viewBox="0 0 10 10"><circle r="3" /></svg>\n```'), '<svg viewBox="0 0 10 10"><circle r="3" /></svg>')
  assert.equal(extractQualityTestHTML('```html\n<html><body>partial'), '<html><body>partial')
  assert.equal(extractQualityTestHTML('No HTML was generated.'), '')
  assert.equal(extractQualityTestHTML(''), '')
})

test('preview policy is installed before untrusted content, allowing inline animation only', () => {
  const malicious = '<html><head><script src="https://example.invalid/x.js"></script></head><body><svg /></body></html>'
  const document = qualityTestPreviewDocument(malicious)
  assert.ok(document.indexOf('Content-Security-Policy') < document.indexOf(malicious))
  for (const policy of ["default-src 'none'", "script-src 'unsafe-inline'", "style-src 'unsafe-inline'", "connect-src 'none'", "frame-src 'none'", "base-uri 'none'", "form-action 'none'"]) assert.ok(document.includes(policy))
  assert.ok(document.includes(malicious))
  assert.ok(document.indexOf(QUALITY_TEST_SIZE_SCRIPT) > document.indexOf(malicious))
})

test('frame height from the sandboxed preview is clamped and rejects garbage', () => {
  assert.equal(clampQualityTestFrameHeight(812.4), 812)
  assert.equal(clampQualityTestFrameHeight(12), 320)
  assert.equal(clampQualityTestFrameHeight(99999), 1400)
  for (const junk of [undefined, null, '', 'tall', NaN, -5, 0, Infinity]) assert.equal(clampQualityTestFrameHeight(junk), undefined)
})

test('quality stream preserves split Unicode, whitespace and diagnostics arriving after completion', async () => {
  const html = '<html>\n  <svg><text>鹈鹕</text></svg>\n</html>'
  const wire = [{ type: 'content', text: html.slice(0, 6) }, { type: 'content', text: '\n  ' }, { type: 'content', text: html.slice(9) }, { type: 'test_complete', success: true }, { type: 'diagnostics', codex_diagnostics: { model: 'test', duration_ms: 23, usage: { reasoning_output_tokens: 42 } } }].map((event) => `data: ${JSON.stringify(event)}\r\n\r\n`).join('')
  const bytes = new TextEncoder().encode(wire)
  const stream = new ReadableStream({ start(controller) { for (let i = 0; i < bytes.length; i += 2) controller.enqueue(bytes.slice(i, i + 2)); controller.close() } })
  const events = []
  assert.equal(await readClaudeTestEvents(stream, (event) => events.push(event)), true)
  assert.equal(events.filter((event) => event.type === 'content').map((event) => event.text).join(''), html)
  assert.equal(events.at(-1).codex_diagnostics.usage.reasoning_output_tokens, 42)
})

test('stopping tasks still occupy a slot and subscription variants keep their display family', () => {
  for (const status of ['running', 'cancelling']) assert.equal(isQualityTestActive({ status }), true)
  for (const status of ['completed', 'error', 'stopped', 'interrupted']) assert.equal(isQualityTestActive({ status }), false)
  assert.equal(isQualityTestActive(null), false)
  assert.equal(qualityTestPlanTone('Pro'), 'pro')
  assert.equal(qualityTestPlanTone('plus'), 'plus')
  assert.equal(qualityTestPlanTone('business'), 'team')
  assert.equal(qualityTestPlanTone('team'), 'team')
  assert.equal(qualityTestPlanTone('enterprise'), 'enterprise')
  assert.equal(qualityTestPlanTone('free'), 'free')
  assert.equal(qualityTestPlanTone('custom'), 'other')
})

test('history filter query only carries the active filters and keeps the model-default effort sentinel', () => {
  assert.equal(qualityTestFilterQuery(2), 'page=2&page_size=20')
  assert.equal(qualityTestFilterQuery(1, { plan: 'pro', model: 'gpt-5.5', effort: 'default', account_id: 7, preset: 'builtin:clock' }), 'page=1&page_size=20&plan=pro&model=gpt-5.5&effort=default&account_id=7&preset=builtin%3Aclock')
  assert.equal(qualityTestFilterQuery(1, { plan: '', model: '', effort: '', account_id: 0 }), 'page=1&page_size=20')
})

test('quality status key separates broken upstream streams from rejections', () => {
  assert.equal(qualityTestStatusKey(null), 'idle')
  assert.equal(qualityTestStatusKey({ status: 'error' }), 'error')
  assert.equal(qualityTestStatusKey({ status: 'error', interrupted: true }), 'streamInterrupted')
  assert.equal(qualityTestStatusKey({ status: 'completed', interrupted: true }), 'completed')
  assert.equal(qualityTestStatusKey({ status: 'interrupted' }), 'interrupted')
})
