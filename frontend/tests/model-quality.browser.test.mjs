import assert from 'node:assert/strict'
import { mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright'
import { createStateFixture } from './state-management-fixture.mjs'

const base = process.env.MODEL_QUALITY_UI_URL || 'http://127.0.0.1:5177'
const output = new URL('../../.impeccable/review/', import.meta.url)
await mkdir(output, { recursive: true })
const browser = await chromium.launch({ headless: true })
try {
  for (const device of ['desktop', 'mobile']) for (const theme of ['light', 'dark']) {
    const context = await browser.newContext({ viewport: device === 'desktop' ? { width: 1440, height: 1050 } : { width: 390, height: 844 }, colorScheme: theme, reducedMotion: 'reduce' })
    const fixture = createStateFixture()
    let config = { enabled: true, models: ['gpt-6-sol', 'gpt-6-astra'], revision: 2 }
    let retest
    let failedSave = false
    const errors = []
    await context.addInitScript(({ theme }) => { localStorage.setItem('lang', 'zh'); localStorage.setItem('theme', theme) }, { theme })
    await context.route('**/*', async route => {
      const url = new URL(route.request().url())
      if (url.origin !== base) return route.abort()
      if (!url.pathname.startsWith('/api/')) return route.continue()
      const body = route.request().postDataJSON()
      const path = url.pathname
      if (path === '/api/admin/model-quality') {
        if (route.request().method() === 'PUT') {
          if (failedSave) return route.fulfill({ status: 409, json: { error: '配置已被修改，请刷新后重试' } })
          config = { ...body, revision: config.revision + 1 }
          return route.fulfill({ json: { config } })
        }
        const now = Math.floor(Date.now() / 1000)
        const accounts = [
          { id: 101, name: 'example-a@example.test', available: true, states: config.models.map((model, i) => ({ account_id: 101, model, status: i ? 'pass' : 'fail', last_outcome: i ? 'pass' : 'error', reason: i ? '糖果题通过：21' : '上游返回 HTTP 503，保留上次未通过结果', checked_at: now - 60, next_check_at: now + 540, running: false, execution: 'scheduled', can_retest: true })) },
          { id: 102, name: 'example-b@example.test', available: true, states: config.models.map((model, i) => ({ account_id: 102, model, status: 'pending', last_outcome: '', reason: '', checked_at: 0, next_check_at: 0, running: false, execution: i ? 'waiting_capacity' : 'model_cooldown', cooldown_reason: i ? '' : 'model_not_supported', resume_at: i ? 0 : now + 600, occupied: 25, capacity: 25, can_retest: true })) },
        ].filter(account => account.name.includes(url.searchParams.get('search') || ''))
        return route.fulfill({ json: { config, models: ['gpt-6-sol', 'gpt-6-astra', 'gpt-6-luna'], accounts, total: accounts.length, page: 1, page_size: 30, interval_seconds: 600 } })
      }
      if (path === '/api/admin/model-quality/retest') { retest = body; return route.fulfill({ status: 202, json: { message: 'queued' } }) }
      if (path === '/api/admin/quality-tests') return route.fulfill({ json: { jobs: [], active_jobs: [], total: 0, concurrency_limit: 3 } })
      if (path === '/api/admin/quality-test-prompts') return route.fulfill({ json: { prompts: [] } })
      return route.fulfill({ json: await fixture(url, route.request().method(), body || {}) })
    })
    const page = await context.newPage()
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(`${base}/admin/quality-test?view=monitor`)
    await page.getByRole('heading', { name: '自动检测与模型暂停' }).waitFor()
    await page.getByText('example-a@example.test', { exact: true }).waitFor()
    assert.equal(await page.locator('.model-quality-verdict .model-quality-fail').count(), 1)
    assert.equal(await page.locator('.model-quality-verdict .model-quality-pass').count(), 1)
    assert.equal(await page.locator('.model-quality-verdict .model-quality-pending').count(), 2)
    await page.getByText('上游曾拒绝此模型，正在冷却。可在账号管理测试此模型，成功后解除这条旧冷却。', { exact: true }).waitFor()
    await page.getByText('等待空闲并发：已占用 25 / 25，释放后自动检测', { exact: true }).waitFor()
    const coolingAccount = page.locator('.model-quality-account').filter({ hasText: 'example-b@example.test' })
    assert.equal(await coolingAccount.getByRole('button', { name: '申请复测', exact: true }).first().isDisabled(), false)
    await coolingAccount.getByRole('button', { name: '申请复测', exact: true }).first().click()
    await page.getByText('已加入复测队列，账号、模型和并发可用后开始', { exact: true }).waitFor()
    assert.deepEqual(retest, { account_id: 102, model: 'gpt-6-sol' })
    const contrast = await page.locator('.model-quality-verdict .model-quality-fail').evaluate(element => {
      const canvas = document.createElement('canvas'), ctx = canvas.getContext('2d')
      const luminance = color => { ctx.clearRect(0, 0, 1, 1); ctx.fillStyle = color; ctx.fillRect(0, 0, 1, 1); const values = [...ctx.getImageData(0, 0, 1, 1).data].slice(0, 3).map(v => { v /= 255; return v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4 }); return values[0] * .2126 + values[1] * .7152 + values[2] * .0722 }
      const a = luminance(getComputedStyle(element).color), b = luminance(getComputedStyle(element.closest('.model-quality-verdict')).backgroundColor)
      return (Math.max(a, b) + .05) / (Math.min(a, b) + .05)
    })
    assert.ok(contrast >= 4.5, `${device}/${theme} failed verdict contrast ${contrast}`)
    await page.evaluate(async () => { await document.fonts.ready; await Promise.allSettled(document.getAnimations().filter(a => a.effect?.getComputedTiming().iterations !== Infinity).map(a => a.finished)) })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `${device}/${theme} overflow`)
    await page.screenshot({ path: fileURLToPath(new URL(`${device}${theme === 'dark' ? '-dark' : ''}.png`, output)), fullPage: true, animations: 'disabled' })
    await page.getByRole('button', { name: '申请复测', exact: true }).first().click()
    await page.getByText('已加入复测队列，账号、模型和并发可用后开始', { exact: true }).waitFor()
    assert.deepEqual(retest, { account_id: 101, model: 'gpt-6-sol' })
    const toggle = page.getByRole('switch', { name: '自动检测与模型暂停' })
    failedSave = true
    await toggle.click()
    await page.getByRole('alert').filter({ hasText: '配置已被修改' }).waitFor()
    assert.equal(await toggle.getAttribute('aria-checked'), 'true', 'failed save must preserve enabled state')
    failedSave = false
    await toggle.click()
    await page.getByText('检测已关闭，不会发起检测或限制模型调用，之前的检测结果已清除。账号原有停用和限流设置仍然生效。', { exact: true }).waitFor()
    assert.equal(config.enabled, false)
    assert.equal(await page.locator('.model-quality-verdict .model-quality-fail').count(), 0)
    assert.equal(await page.getByRole('button', { name: '申请复测', exact: true }).first().isDisabled(), true)
    await page.getByRole('button', { name: '移除 gpt-6-sol', exact: true }).click()
    await page.getByRole('button', { name: '移除 gpt-6-sol', exact: true }).waitFor({ state: 'hidden' })
    assert.deepEqual(config.models, ['gpt-6-astra'])
    await page.getByRole('button', { name: '移除 gpt-6-astra', exact: true }).click()
    await page.getByRole('button', { name: '移除 gpt-6-astra', exact: true }).waitFor({ state: 'hidden' })
    assert.equal(await toggle.isDisabled(), true)
    await page.locator('#quality-add-model').click()
    await page.getByRole('option', { name: 'gpt-6-luna', exact: true }).click()
    await page.getByRole('button', { name: '移除 gpt-6-luna', exact: true }).waitFor()
    await toggle.click()
    await page.locator('.model-quality-switch').getByText('已开启', { exact: true }).waitFor()
    assert.deepEqual(config.models, ['gpt-6-luna'])
    assert.equal(config.enabled, true)
    assert.deepEqual(errors, [])
    await context.close()
  }
  console.log('Model quality UI passed: desktop/mobile, light/dark, red/green/pending, manual retest, failed save, disable and deselection.')
} finally { await browser.close() }
