import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { ANTIGRAVITY_DEFAULT_MODELS, orderAntigravityTestModels } from './antigravityModels.ts'

test('frontend fallbacks match public backend IDs including Flash 3.8', () => {
  const source = readFileSync(new URL('../../../proxy/antigravity_models.go', import.meta.url), 'utf8')
  const catalog = source.split('var antigravityPublicModelCatalog =')[1].split('var antigravityLogicalCompatibilityCatalog')[0]
  // Claude stays routable in the backend catalog, but the fallback omits it:
  // accounts migrate between Claude generations, so only a synced catalog knows.
  const ids = Array.from(catalog.matchAll(/id: "([^"]+)"/g), match => match[1]).filter(id => !id.startsWith('claude-'))
  assert.deepEqual([...ANTIGRAVITY_DEFAULT_MODELS].sort(), ids.sort())
  assert.equal(ANTIGRAVITY_DEFAULT_MODELS[0], 'gemini-3.8-flash-low')
})

test('test model ordering prefers configured model, then newest flash low tier', () => {
  const catalog = ['gemini-3.5-flash-low', 'gemini-3.5-flash-high', 'gemini-3.8-flash-low', 'gemini-3.7-flash-low', 'claude-sonnet-4-6', 'imagen-4']
  assert.deepEqual(orderAntigravityTestModels(catalog, '')[0], 'gemini-3.8-flash-low')
  assert.deepEqual(orderAntigravityTestModels(catalog, 'claude-sonnet-4-6')[0], 'claude-sonnet-4-6')
  assert.deepEqual(orderAntigravityTestModels(catalog, 'gemini-9-flash-low')[0], 'gemini-3.8-flash-low')
  assert.ok(!orderAntigravityTestModels(catalog, '').includes('imagen-4'))
  assert.deepEqual(orderAntigravityTestModels([], ''), [])
})

test('connection test choices match quota models instead of the broader catalog', () => {
  const available = ['claude-opus-4-6-thinking', 'gemini-3.8-flash-low', 'gemini-3.8-flash-high']
  const extra = ['gemini-2.5-pro', 'gemini-3-flash', 'gemini-3.1-flash-lite', 'gemini-3.5-flash-lite']
  const quota = { models: available.map(model_id => ({ model_id, remaining_fraction: 0 })) }
  assert.deepEqual(
    orderAntigravityTestModels([...available, ...extra], extra[0], quota),
    [available[1], available[0], available[2]],
  )
  assert.equal(orderAntigravityTestModels([], available[0], quota)[0], available[0])
  assert.deepEqual(orderAntigravityTestModels(extra, '', { models: [] }), [])
  assert.deepEqual(orderAntigravityTestModels(extra, '', { models: {} }), [])
})

test('legacy quota maps and model ID fields use quota IDs without display labels or static fallbacks', () => {
  assert.deepEqual(orderAntigravityTestModels(['extra'], '', {
    models: { 'claude-sonnet-4-6': { display_name: 'Sonnet' }, 'imagen-4': {} },
  }), ['claude-sonnet-4-6'])
  assert.deepEqual(orderAntigravityTestModels(['extra'], '', {
    models: [{ model: ' claude-a ' }, { model_id: 'claude-a' }, { modelId: 'claude-b' }, { name: 'claude-c' }, { id: 'claude-d' }, { display_name: 'Missing ID' }],
  }), ['claude-a', 'claude-b', 'claude-c', 'claude-d'])
  assert.deepEqual(orderAntigravityTestModels(['imagen-4']), [])
  assert.deepEqual(orderAntigravityTestModels(['custom-model']), ['custom-model'])
})
