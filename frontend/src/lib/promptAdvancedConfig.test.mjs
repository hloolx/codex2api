import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  parseAdvancedConfigDocument,
  patchAdvancedConfigDocument,
  readAdvancedConfigPath,
} from '../types.ts'

test('advanced config parsing keeps invalid input distinct from defaults', () => {
  assert.deepEqual(parseAdvancedConfigDocument('{broken'), {
    ok: false,
    value: null,
    error: 'invalid_json',
  })
  assert.deepEqual(parseAdvancedConfigDocument('[]'), {
    ok: false,
    value: null,
    error: 'root_not_object',
  })

  const failedPatch = patchAdvancedConfigDocument('{broken', [
    { path: ['guard', 'mode'], value: 'enforce' },
  ])
  assert.equal(failedPatch.ok, false)
  assert.equal(failedPatch.serialized, '{broken')
})

test('field-level advanced config patches preserve unknown nested fields and enum values', () => {
  const raw = JSON.stringify({
    future_top_level: { enabled: true, revision: 7 },
    guard: {
      mode: 'future_mode',
      future_guard_option: { sample: 0.25 },
      provider_profiles: {
        openai: 'future_profile',
        future_provider: 'strict-plus',
      },
      layers: {
        current_user: { mode: 'enforce', future_layer_option: 'keep-me' },
        future_layer: { mode: 'observe-plus' },
      },
      performance: {
        shadow_workers: 2,
        future_queue_policy: 'adaptive',
      },
    },
    sidecar: { mode: 'future_sidecar_mode', future_sidecar_option: 9 },
  })

  const result = patchAdvancedConfigDocument(raw, [
    { path: ['guard', 'layers', 'current_user', 'mode'], value: 'warn' },
  ])
  assert.equal(result.ok, true)
  const saved = JSON.parse(result.serialized)
  assert.deepEqual(saved.future_top_level, { enabled: true, revision: 7 })
  assert.equal(saved.guard.mode, 'future_mode')
  assert.deepEqual(saved.guard.future_guard_option, { sample: 0.25 })
  assert.equal(saved.guard.provider_profiles.openai, 'future_profile')
  assert.equal(saved.guard.provider_profiles.future_provider, 'strict-plus')
  assert.equal(saved.guard.layers.current_user.future_layer_option, 'keep-me')
  assert.equal(saved.guard.layers.current_user.mode, 'warn')
  assert.equal(saved.guard.layers.future_layer.mode, 'observe-plus')
  assert.equal(saved.guard.performance.shadow_workers, 2)
  assert.equal(saved.guard.performance.future_queue_policy, 'adaptive')
  assert.equal(saved.sidecar.mode, 'future_sidecar_mode')
  assert.equal(saved.sidecar.future_sidecar_option, 9)
})

test('removing one known override does not rebuild its parent object', () => {
  const raw = JSON.stringify({
    guard: {
      provider_profiles: {
        openai: 'balanced',
        future_provider: 'future_profile',
      },
    },
  })
  const result = patchAdvancedConfigDocument(raw, [{
    path: ['guard', 'provider_profiles', 'openai'],
    remove: true,
  }])
  assert.equal(result.ok, true)
  assert.equal(readAdvancedConfigPath(result.value, ['guard', 'provider_profiles', 'openai']), undefined)
  assert.equal(readAdvancedConfigPath(result.value, ['guard', 'provider_profiles', 'future_provider']), 'future_profile')
})

test('review adapter patches preserve prompts, payload templates, and unknown fields', () => {
  const raw = JSON.stringify({
    review_adapter: {
      request_mode: 'moderations',
      future_response_parser: 'v2',
    },
    future_root: true,
  })
  const result = patchAdvancedConfigDocument(raw, [
    { path: ['review_adapter', 'request_mode'], value: 'chat_completions' },
    { path: ['review_adapter', 'scope'], value: 'local_candidates' },
    { path: ['review_adapter', 'system_prompt'], value: 'system' },
    { path: ['review_adapter', 'user_prompt_template'], value: '<user_input>{{text}}</user_input>' },
    { path: ['review_adapter', 'payload_template'], value: '{"input":"{{user_prompt}}"}' },
    { path: ['review_adapter', 'confidence_threshold'], value: 0.72 },
  ])
  assert.equal(result.ok, true)
  const saved = JSON.parse(result.serialized)
  assert.equal(saved.review_adapter.request_mode, 'chat_completions')
  assert.equal(saved.review_adapter.scope, 'local_candidates')
  assert.equal(saved.review_adapter.user_prompt_template, '<user_input>{{text}}</user_input>')
  assert.equal(saved.review_adapter.payload_template, '{"input":"{{user_prompt}}"}')
  assert.equal(saved.review_adapter.confidence_threshold, 0.72)
  assert.equal(saved.review_adapter.future_response_parser, 'v2')
  assert.equal(saved.future_root, true)
})

test('Chinese locale labels do not expose internal policy enum values', () => {
  const locale = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))
  assert.equal('sync' in locale.promptFilter.guard.performance.overflowModes, false)
  const labels = [
    ...['off', 'shadow', 'warn', 'enforce'].map((mode) => locale.promptFilter.guard.modes[mode].label),
    ...['balanced', 'strict'].map((profile) => locale.promptFilter.guard.profiles[profile].label),
    ...['shadow', 'warn', 'enforce'].map((mode) => locale.promptFilter.extensions.sidecar.modes[mode]),
  ]
  const internalValues = new Set(['off', 'shadow', 'warn', 'enforce', 'balanced', 'strict'])
  for (const label of labels) {
    assert.equal(internalValues.has(String(label).toLowerCase()), false, `internal enum leaked as label: ${label}`)
  }
})

test('hidden runtime settings survive visible editor patches', () => {
  const raw = JSON.stringify({
    guard: {
      performance: {
        max_segments: 64,
        max_current_user_bytes: 131072,
        max_auxiliary_bytes: 32768,
        scan_chunk_bytes: 8192,
        scan_overlap_bytes: 512,
        future_budget_strategy: 'adaptive',
      },
    },
    sidecar: {
      enabled: false,
      scan_clean_enabled: true,
      sample_percent: 9,
      cache_ttl_seconds: 4321,
    },
    attachment: {
      enabled: false,
      max_bytes: 524288,
      cache_ttl_seconds: 987,
    },
    output: {
      enabled: false,
      strict_only: true,
      buffer_bytes: 8192,
      overlap_bytes: 1024,
    },
    newapi: {
      enabled: false,
      secret: 'retired-global-secret',
      offense_window_seconds: 86400,
      ban_after: 2,
      max_clock_skew_seconds: 240,
    },
  })
  const result = patchAdvancedConfigDocument(raw, [
    { path: ['guard', 'default_profile'], value: 'strict' },
    { path: ['sidecar', 'enabled'], value: true },
    { path: ['attachment', 'enabled'], value: true },
    { path: ['output', 'strict_only'], value: false },
    { path: ['newapi', 'enabled'], remove: true },
    { path: ['newapi', 'secret'], remove: true },
    { path: ['newapi', 'offense_window_seconds'], remove: true },
    { path: ['newapi', 'ban_after'], remove: true },
  ])
  assert.equal(result.ok, true)
  const saved = JSON.parse(result.serialized)
  assert.equal(saved.guard.default_profile, 'strict')
  assert.equal(saved.guard.performance.max_segments, 64)
  assert.equal(saved.guard.performance.max_current_user_bytes, 131072)
  assert.equal(saved.guard.performance.future_budget_strategy, 'adaptive')
  assert.equal(saved.sidecar.enabled, true)
  assert.equal(saved.sidecar.scan_clean_enabled, true)
  assert.equal(saved.sidecar.sample_percent, 9)
  assert.equal(saved.sidecar.cache_ttl_seconds, 4321)
  assert.equal(saved.attachment.enabled, true)
  assert.equal(saved.attachment.max_bytes, 524288)
  assert.equal(saved.attachment.cache_ttl_seconds, 987)
  assert.equal(saved.output.strict_only, false)
  assert.equal(saved.output.buffer_bytes, 8192)
  assert.equal(saved.output.overlap_bytes, 1024)
  assert.equal(saved.newapi.enabled, undefined)
  assert.equal(saved.newapi.secret, undefined)
  assert.equal(saved.newapi.offense_window_seconds, undefined)
  assert.equal(saved.newapi.ban_after, undefined)
  assert.equal(saved.newapi.max_clock_skew_seconds, 240)
})

test('Prompt Filter editor does not render runtime tuning controls', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const editorStart = source.indexOf('function AdvancedProtectionEditor(')
  const editorEnd = source.indexOf('\nfunction AdvancedPanel(', editorStart)
  assert.notEqual(editorStart, -1)
  assert.notEqual(editorEnd, -1)
  const editorSource = source.slice(editorStart, editorEnd)
  const forbiddenFragments = [
    'promptFilter.guard.performance.',
    'scan_clean_enabled',
    'sample_percent',
    'async_shadow_auxiliary_enabled',
    'exact_segment_cache_enabled',
    'shadow_queue_size',
    'shadow_workers',
    'buffer_bytes',
    'overlap_bytes',
    'max_clock_skew_seconds',
  ]
  for (const fragment of forbiddenFragments) {
    assert.equal(editorSource.includes(fragment), false, `internal control leaked into editor source: ${fragment}`)
  }
})

test('Prompt Filter consolidates advanced controls and tests all configured review keys', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  assert.match(source, /advancedOpen/)
  assert.match(source, /applyRecommendedProtection/)
  assert.match(source, /test_all_keys: true/)
  assert.match(source, /reviewTestResult\.results/)
  assert.match(source, /openLearningReview/)
  for (const fragment of [
    'reviewTemplatesTitle',
    'normalizationSimplifiedDesc',
    'guard.simplifiedSummary',
    'extensions.collapsedDesc',
    'recommendedStrengthTitle',
  ]) {
    assert.equal(source.includes(fragment), true, `simplified protection control is missing: ${fragment}`)
  }
	  const reviewPanel = source.indexOf("t('promptFilter.reviewServiceSummary')")
	  const reviewTemplates = source.indexOf("t('promptFilter.reviewTemplatesTitle')")
	  const expertPanel = source.indexOf("t('promptFilter.expertSettingsSummary')")
	  assert.ok(reviewPanel >= 0, 'missing model review panel')
	  assert.ok(reviewTemplates >= 0, 'missing review request templates')
	  assert.ok(expertPanel >= 0, 'missing expert settings panel')
	  assert.ok(reviewTemplates > reviewPanel && reviewTemplates < expertPanel, 'review request templates must stay inside the model review section')
})

test('conversation CY locks expose a bounded automatic expiry control', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  assert.match(source, /conversation_lock_ttl_hours: 168/)
  assert.match(source, /user_cyber_cooldown_minutes: 30/)
  assert.match(source, /conversationLockTTL/)
  assert.match(source, /userCyberCooldownMinutes/)
  assert.match(source, /min=\{1\} max=\{720\}/)
  assert.match(source, /min=\{1\} max=\{1440\}/)
})

test('review prompt defaults are owned by the backend rather than duplicated in the UI', () => {
  const frontendSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const backendSource = readFileSync(new URL('../../../security/promptfilter/review.go', import.meta.url), 'utf8')
  assert.equal(frontendSource.includes('const defaultReviewSystemPrompt'), false)
  assert.match(frontendSource, /system_prompt: ''/)
  assert.match(frontendSource, /reviewSystemPromptPlaceholder/)
  assert.match(backendSource, /const DefaultReviewSystemPrompt = `/)
})

test('Prompt Filter rule tester renders the final GuardPipeline decision metadata', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const requiredFragments = [
    'result.decision?.action',
    'decision?.audit_score',
    'decision?.primary_origin',
    'decision?.reason_code',
    'decision?.strike_eligible',
    'result.protocol',
    'result.provider',
  ]
  for (const fragment of requiredFragments) {
    assert.equal(source.includes(fragment), true, `GuardPipeline test metadata is not rendered: ${fragment}`)
  }
})

test('Prompt Filter exposes explicit review scope and live connection testing', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const requiredFragments = [
    "request_mode: 'moderations'",
    "scope: 'all_requests'",
    "'local_candidates'",
    "value: 'chat_completions'",
    "['review_adapter', key]",
    'api.testPromptReview',
    'reviewScopeHint',
    '{{user_prompt}}',
  ]
  for (const fragment of requiredFragments) {
    assert.equal(source.includes(fragment), true, `full-request review control is missing: ${fragment}`)
  }
})

test('terminal enforcement exposes clean-review model exemptions', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))
  assert.match(source, /terminal_bypass_models: \['codex-auto-review'\]/)
  assert.match(source, /terminalBypassModelsText/)
  assert.match(source, /update\('enforcement', \{ terminal_bypass_models:/)
  assert.match(zh.promptFilter.terminalBypassModelsHint, /二审明确通过后可以清除本地终局命中/)
  assert.match(source, /approval_review_models: \['codex-auto-review'\]/)
  assert.match(source, /update\('enforcement', \{ approval_review_models:/)
  assert.match(zh.promptFilter.approvalReviewModelsHint, /gpt-5\.6-luna/)
  assert.match(zh.promptFilter.help.approvalReviewModels, /approval_review_model_untrusted/)
  assert.match(source, /conversation_lock_enabled: true/)
  assert.match(source, /update\('enforcement', \{ conversation_lock_enabled: next \}\)/)
  assert.match(source, /<SwitchField[^>]+conversationLockEnabled/)
  assert.doesNotMatch(source, /<SwitchRow/)
  assert.match(zh.promptFilter.help.conversationLockEnabled, /上游明确返回 cyber_policy/)
})

test('local block message editor round-trips through enforcement config', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const locales = ['en', 'zh', 'zh-TW'].map((name) => JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8')))

  assert.match(source, /local_block_message: string/)
  assert.match(source, /local_block_message: ''/)
  assert.match(source, /typeof enforcement\.local_block_message === 'string'/)
  assert.doesNotMatch(source, /maxLength=\{2000\}/)
  assert.match(source, /Array\.from\(event\.target\.value\)\.slice\(0, 2000\)\.join\(''\)/)
  assert.match(source, /promptFilter\.localBlockMessagePlaceholder/)
  for (const locale of locales) {
    assert.equal(typeof locale.promptFilter.localBlockMessage, 'string')
    assert.equal(typeof locale.promptFilter.localBlockMessagePlaceholder, 'string')
    assert.equal(typeof locale.promptFilter.help.localBlockMessage, 'string')
  }
})

test('Moderations review exposes sub2api-compatible category thresholds', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const requiredFragments = [
    'moderation_thresholds',
    "'harassment/threatening': 0.90",
    "'self-harm/intent': 0.85",
    "'sexual/minors': 0.65",
    'resetModerationThresholds',
    'reviewTestResult.highest_category',
  ]
  for (const fragment of requiredFragments) {
    assert.equal(source.includes(fragment), true, `Moderations threshold control is missing: ${fragment}`)
  }
})

test('adaptive model review is a single switch with an explicit low-latency safety policy', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))
  assert.match(source, /\['adaptive_review', 'enabled'\]/)
  assert.match(source, /adaptiveReview\.enabled/)
  assert.match(source, /\['adaptive_review', 'min_clean_reviews'\], value: 3/)
  assert.match(source, /\['adaptive_review', 'min_observation_hours'\], value: 1/)
  assert.match(zh.promptFilter.adaptiveReview.description, /新用户先完整复核/)
  assert.match(zh.promptFilter.adaptiveReview.defaults, /\{\{minClean\}\} 次.*\{\{hours\}\} 小时.*\{\{sample\}\}%.*\{\{forceHours\}\} 小时/)
})
