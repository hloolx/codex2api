import assert from 'node:assert/strict'
import { mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { createStateFixture } from './state-management-fixture.mjs'

const base = process.env.IPV6_UI_URL || 'http://127.0.0.1:41486'
const output = new URL('../../.impeccable/ipv6-review/', import.meta.url)
await mkdir(output, { recursive: true })
const browser = await chromium.launch({ headless: true })
try {
  for (const mobile of [false, true]) for (const theme of ['light', 'dark']) {
    const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 1050 }, colorScheme: theme, reducedMotion: 'reduce' })
    const fixture = createStateFixture()
    let fail = false
    let config = { enabled: true, source_ips: [], cooldown_seconds: 600, max_attempts: 3, retry_5xx: true, revision: 1 }
    const errors = []
    const state = () => ({ config, local_ips: ['2604:2dc0:100:3240::3999', '2604:2dc0:100:3240::399a', '2604:2dc0:100:3240::399b'], bindings: [{ account_id: 101, ip: '2604:2dc0:100:3240::3999', changed_at: 1791472800, reason: 'http_429', rotations: 2 }], cooldowns: [{ ip: '2604:2dc0:100:3240::399a', until: 1791473400 }], account_names: { 101: 'example-ipv6@example.test' }, resin_enabled: false })
    await context.addInitScript(({ theme }) => { localStorage.setItem('lang', 'zh'); localStorage.setItem('theme', theme) }, { theme })
    await context.route('**/*', async route => {
      const url = new URL(route.request().url())
      if (url.origin !== base) return route.abort()
      if (!url.pathname.startsWith('/api/')) return route.continue()
      const body = route.request().postDataJSON()
      if (url.pathname === '/api/admin/ipv6-egress') {
        if (route.request().method() === 'PUT') {
          if (fail) return route.fulfill({ status: 409, json: { error: 'IPv6 配置保存失败，请刷新后重试' } })
          config = { ...body, revision: config.revision + 1 }
        }
        return route.fulfill({ json: state() })
      }
      if (url.pathname === '/api/admin/proxies') return route.fulfill({ json: { proxies: [] } })
      if (url.pathname.includes('/proxy-risk')) return route.fulfill({ json: { profiles: [], scores: [], jobs: [] } })
      return route.fulfill({ json: await fixture(url, route.request().method(), body || {}) })
    })
    const page = await context.newPage()
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(`${base}/admin/proxies`)
    const panel = page.getByRole('region', { name: '主机 IPv6 独立出口' })
    await panel.getByText('example-ipv6@example.test', { exact: true }).waitFor()
    assert.equal(await panel.getByText('地址池 3 · 可分配 1 · 冷却中 1').count(), 1)
    await panel.getByLabel('地址冷却（秒）').fill('900')
    await panel.getByLabel('单次最多尝试次数').click()
    await panel.getByRole('button', { name: '保存出口设置' }).click()
    await panel.getByText('设置已生效').waitFor()
    assert.equal(config.cooldown_seconds, 900)
    await panel.scrollIntoViewIfNeeded()
    await page.evaluate(async () => { await document.fonts.ready; await Promise.allSettled(document.getAnimations().filter(a => a.effect?.getComputedTiming().iterations !== Infinity).map(a => a.finished)) })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'page overflow')
    await page.screenshot({ path: fileURLToPath(new URL(`${mobile ? 'mobile' : 'desktop'}-${theme}.png`, output)), animations: 'disabled' })
    if (mobile) {
      await panel.getByText('example-ipv6@example.test', { exact: true }).scrollIntoViewIfNeeded()
      await page.evaluate(() => window.scrollBy(0, 160))
      await page.screenshot({ path: fileURLToPath(new URL(`mobile-${theme}-bindings.png`, output)), animations: 'disabled' })
    }
    await panel.getByLabel('IPv6 地址池', { exact: true }).fill('unfinished-address')
    fail = true
    const toggle = panel.getByRole('switch', { name: '启用主机 IPv6 出口' })
    await toggle.click()
    await panel.getByRole('alert').waitFor()
    assert.equal(await toggle.getAttribute('aria-checked'), 'true')
    if (!mobile && theme === 'light') { await page.waitForTimeout(10500); assert.match(await panel.getByRole('alert').innerText(), /配置保存失败/); await page.screenshot({ path: fileURLToPath(new URL('save-error-after-poll.png', output)), animations: 'disabled' }) }
    fail = false
    await toggle.click()
    await panel.getByText('未启用', { exact: true }).waitFor()
    assert.equal(config.enabled, false)
    assert.deepEqual(config.source_ips, [])
    assert.equal(await panel.getByLabel('IPv6 地址池', { exact: true }).inputValue(), 'unfinished-address')
    assert.equal(await panel.getByText('设置已生效', { exact: true }).count(), 0)
    if (!mobile && theme === 'light') await page.screenshot({ path: fileURLToPath(new URL('disabled-with-unsaved-draft.png', output)), animations: 'disabled' })
    assert.deepEqual(errors, [])
    await context.close()
  }
} finally { await browser.close() }
console.log('IPv6 egress browser checks passed (desktop/mobile, light/dark, save/toggle/conflict).')
