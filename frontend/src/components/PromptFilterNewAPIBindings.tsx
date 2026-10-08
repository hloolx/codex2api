import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  AlertTriangle,
  Check,
  Copy,
  KeyRound,
  Loader2,
  Pencil,
  Plus,
  RefreshCw,
  ShieldCheck,
  Trash2,
} from 'lucide-react'
import { api } from '../api'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type {
  APIKeyRow,
  PromptFilterNewAPIBinding,
  PromptFilterScope,
} from '../types'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'

type BindingForm = {
  apiKeyId: string
  platformCode: string
  platformName: string
  enabled: boolean
  requireSignedIdentity: boolean
  promptFilterScope: PromptFilterScope
}

const emptyBindingForm: BindingForm = {
  apiKeyId: '',
  platformCode: '',
  platformName: '',
  enabled: true,
  requireSignedIdentity: false,
  promptFilterScope: 'inherit',
}

function apiKeyLabel(apiKey: APIKeyRow): string {
  return `#${apiKey.id} ${apiKey.name || '未命名 Key'} · ${apiKey.key || '已脱敏'}`
}

function BindingSwitch({
  label,
  description,
  checked,
  onCheckedChange,
}: {
  label: string
  description: string
  checked: boolean
  onCheckedChange: (checked: boolean) => void
}) {
  return (
    <label className="flex items-start justify-between gap-4 rounded-lg border border-border/70 bg-muted/20 p-3">
      <span className="min-w-0">
        <span className="block text-sm font-medium text-foreground">{label}</span>
        <span className="mt-1 block text-xs leading-5 text-muted-foreground">{description}</span>
      </span>
      <Switch checked={checked} onCheckedChange={onCheckedChange} className="mt-0.5" />
    </label>
  )
}

function BindingFormFields({
  form,
  setForm,
  apiKeys,
  editing,
}: {
  form: BindingForm
  setForm: (next: BindingForm) => void
  apiKeys: APIKeyRow[]
  editing: boolean
}) {
  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="space-y-1.5 sm:col-span-2">
          <span className="text-sm font-medium">Codex2API Key</span>
          <Select
            value={form.apiKeyId}
            disabled={editing}
            placeholder="选择用于接入的 Codex2API Key"
            onValueChange={(apiKeyId) => setForm({ ...form, apiKeyId })}
            options={apiKeys.map((apiKey) => ({ value: String(apiKey.id), label: apiKeyLabel(apiKey) }))}
          />
          <span className="block text-xs leading-5 text-muted-foreground">
            一个 Key 只能绑定一个调用方；未绑定 Key 不接受 NewAPI 签名身份。
          </span>
        </label>
        <label className="space-y-1.5">
          <span className="text-sm font-medium">调用方代码</span>
          <Input
            value={form.platformCode}
            placeholder="输入唯一接入标识"
            maxLength={32}
            onChange={(event) => setForm({
              ...form,
              platformCode: event.target.value.toLowerCase().replace(/[^a-z0-9_-]/g, ''),
            })}
          />
          <span className="block text-xs leading-5 text-muted-foreground">仅支持小写字母、数字、下划线和短横线，最长 32 字符且全局唯一。</span>
        </label>
        <label className="space-y-1.5">
          <span className="text-sm font-medium">调用方名称</span>
          <Input
            value={form.platformName}
            placeholder="输入便于识别的名称"
            maxLength={255}
            onChange={(event) => setForm({ ...form, platformName: event.target.value })}
          />
        </label>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <BindingSwitch
          label="启用此绑定"
          description="关闭后，该 Key 不接受此调用方的签名身份，也不会使用其他 Key 的绑定。"
          checked={form.enabled}
          onCheckedChange={(enabled) => setForm({ ...form, enabled })}
        />
        <BindingSwitch
          label="强制签名身份"
          description="默认关闭。确认对应 NewAPI 已正确配置此绑定的密钥后再开启，否则请求会因缺少或签名错误而被拒绝。"
          checked={form.requireSignedIdentity}
          onCheckedChange={(requireSignedIdentity) => setForm({ ...form, requireSignedIdentity })}
        />
      </div>
      <label className="block space-y-1.5 rounded-lg border border-border/70 bg-muted/20 p-3">
        <span className="text-sm font-medium">该 Key 的 Prompt 检查范围</span>
        <Select
          value={form.promptFilterScope}
          onValueChange={(promptFilterScope) => setForm({ ...form, promptFilterScope: promptFilterScope as PromptFilterScope })}
          options={[
            { value: 'inherit', label: '继承全局：本地检测 + 远程模型复核' },
            { value: 'local_only', label: '仅本地检测：跳过远程模型复核' },
            { value: 'off', label: '完全跳过：不执行 Prompt 检查' },
          ]}
        />
        <span className="block text-xs leading-5 text-muted-foreground">
          “仅本地检测”仍会执行高危规则、会话关联、审计记录与风险画像；“完全跳过”只保留 API Key 和签名身份校验，适合明确受控的内部调用方。
        </span>
      </label>
    </div>
  )
}

export default function PromptFilterNewAPIBindings() {
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [bindings, setBindings] = useState<PromptFilterNewAPIBinding[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm, setCreateForm] = useState<BindingForm>(emptyBindingForm)
  const [editing, setEditing] = useState<PromptFilterNewAPIBinding | null>(null)
  const [editForm, setEditForm] = useState<BindingForm>(emptyBindingForm)
  const [rotating, setRotating] = useState<PromptFilterNewAPIBinding | null>(null)
  const [graceSeconds, setGraceSeconds] = useState('300')
  const [saving, setSaving] = useState(false)
  const [secretReveal, setSecretReveal] = useState<{ platformName: string; secret: string } | null>(null)
  const [secretCopied, setSecretCopied] = useState(false)
  const [secretCloseConfirmOpen, setSecretCloseConfirmOpen] = useState(false)
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [keyResult, bindingResult] = await Promise.all([
        api.getAPIKeys({ view: 'lite' }),
        api.getPromptFilterNewAPIBindings(),
      ])
      setAPIKeys(keyResult.keys ?? [])
      setBindings(bindingResult.bindings ?? [])
    } catch (loadError) {
      setError(getErrorMessage(loadError))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const boundKeyIDs = useMemo(() => new Set(bindings.map((binding) => binding.api_key_id)), [bindings])
  const availableAPIKeys = useMemo(
    () => apiKeys.filter((apiKey) => !boundKeyIDs.has(apiKey.id)),
    [apiKeys, boundKeyIDs],
  )
  const apiKeyByID = useMemo(() => new Map(apiKeys.map((apiKey) => [apiKey.id, apiKey])), [apiKeys])

  const openCreate = () => {
    const firstAvailable = availableAPIKeys[0]
    setCreateForm({ ...emptyBindingForm, apiKeyId: firstAvailable ? String(firstAvailable.id) : '' })
    setCreateOpen(true)
  }

  const openEdit = (binding: PromptFilterNewAPIBinding) => {
    setEditForm({
      apiKeyId: String(binding.api_key_id),
      platformCode: binding.platform_code,
      platformName: binding.platform_name,
      enabled: binding.enabled,
      requireSignedIdentity: binding.require_signed_identity,
      promptFilterScope: binding.prompt_filter_scope || 'inherit',
    })
    setEditing(binding)
  }

  const revealSecret = (platformName: string, secret?: string) => {
    if (!secret) {
      showToast('操作成功，但服务器没有返回一次性密钥，请重新轮换。', 'error')
      return
    }
    setSecretCopied(false)
    setSecretReveal({ platformName, secret })
  }

  const createBinding = async () => {
    const apiKeyId = Number(createForm.apiKeyId)
    if (!Number.isInteger(apiKeyId) || apiKeyId <= 0 || !createForm.platformCode.trim()) {
      showToast('请选择 Codex2API Key，并填写调用方代码。', 'error')
      return
    }
    if (createForm.promptFilterScope === 'off') {
      const approved = await confirm({
        title: '确认让该 Key 跳过全部 Prompt 检查？',
        description: '该 Key 的请求将不执行本地高危规则和远程模型复核。API Key 鉴权及可选的 NewAPI 签名身份校验仍会保留。',
        confirmText: '确认完全跳过',
        tone: 'warning',
      })
      if (!approved) return
    }
    setSaving(true)
    try {
      const result = await api.createPromptFilterNewAPIBinding({
        api_key_id: apiKeyId,
        platform_code: createForm.platformCode.trim(),
        platform_name: createForm.platformName.trim(),
        enabled: createForm.enabled,
        require_signed_identity: createForm.requireSignedIdentity,
        prompt_filter_scope: createForm.promptFilterScope,
      })
      setCreateOpen(false)
      await load()
      revealSecret(result.platform_name || result.platform_code, result.secret)
    } catch (createError) {
      showToast(getErrorMessage(createError), 'error')
    } finally {
      setSaving(false)
    }
  }

  const updateBinding = async () => {
    if (!editing || !editForm.platformCode.trim()) return
    if (!editing.require_signed_identity && editForm.requireSignedIdentity) {
      const approved = await confirm({
        title: '确认开启强制签名身份？',
        description: (
          <span>
            必须先在“{editForm.platformName.trim() || editForm.platformCode.trim()}”对应的 NewAPI 配置此绑定的密钥；否则开启后，该 Codex2API Key 的请求会立即返回 401。签名失败属于身份校验错误，不会记为 Prompt 违规，也不会触发违规处罚。
          </span>
        ),
        confirmText: '已配置，确认开启',
        tone: 'warning',
      })
      if (!approved) return
    }
    if (editing.prompt_filter_scope !== 'off' && editForm.promptFilterScope === 'off') {
      const approved = await confirm({
        title: '确认让该 Key 跳过全部 Prompt 检查？',
        description: '保存后，该 Key 将不再执行本地高危规则和远程模型复核。API Key 鉴权及可选的 NewAPI 签名身份校验仍会保留。',
        confirmText: '确认完全跳过',
        tone: 'warning',
      })
      if (!approved) return
    }
    setSaving(true)
    try {
      await api.updatePromptFilterNewAPIBinding(editing.api_key_id, {
        platform_code: editForm.platformCode.trim(),
        platform_name: editForm.platformName.trim(),
        enabled: editForm.enabled,
        require_signed_identity: editForm.requireSignedIdentity,
        prompt_filter_scope: editForm.promptFilterScope,
      })
      setEditing(null)
      await load()
      showToast('审计身份绑定已更新。')
    } catch (updateError) {
      showToast(getErrorMessage(updateError), 'error')
    } finally {
      setSaving(false)
    }
  }

  const rotateSecret = async () => {
    if (!rotating) return
    const grace = Number(graceSeconds)
    if (!Number.isInteger(grace) || grace < 60 || grace > 86400) {
      showToast('自动生成新密钥时，宽限秒数必须是 60 到 86400 之间的整数。', 'error')
      return
    }
    setSaving(true)
    try {
      const result = await api.generatePromptFilterNewAPIBindingSecret(rotating.api_key_id, grace)
      setRotating(null)
      await load()
      revealSecret(result.platform_name || result.platform_code, result.secret)
    } catch (rotateError) {
      showToast(getErrorMessage(rotateError), 'error')
    } finally {
      setSaving(false)
    }
  }

  const deleteBinding = async (binding: PromptFilterNewAPIBinding) => {
    const approved = await confirm({
      title: `删除“${binding.platform_name || binding.platform_code}”绑定？`,
      description: (
        <span>
          删除后，Key #{binding.api_key_id} 将不再接受 NewAPI 签名身份；请先确认该调用方已停止使用此 Key。
        </span>
      ),
      confirmText: '确认删除',
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!approved) return
    try {
      await api.deletePromptFilterNewAPIBinding(binding.api_key_id)
      await load()
      showToast('审计身份绑定已删除。')
    } catch (deleteError) {
      showToast(getErrorMessage(deleteError), 'error')
    }
  }

  const copySecret = async () => {
    if (!secretReveal) return
    try {
      await navigator.clipboard.writeText(secretReveal.secret)
      setSecretCopied(true)
      showToast('绑定密钥已复制。')
    } catch {
      showToast('复制失败，请手动选择并复制密钥。', 'error')
    }
  }

  const requestCloseSecretReveal = () => {
    if (!secretReveal) return
    setSecretCloseConfirmOpen(true)
  }

  const confirmCloseSecretReveal = () => {
    setSecretCloseConfirmOpen(false)
    setSecretReveal(null)
    setSecretCopied(false)
  }

  return (
    <>
      <section aria-labelledby="newapi-key-bindings-title" className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <div className="flex items-center gap-2">
              <KeyRound className="size-5 text-primary" />
              <h4 id="newapi-key-bindings-title" className="text-sm font-semibold">按 Codex2API Key 绑定审计身份</h4>
            </div>
            <p className="mt-1 text-sm leading-6 text-muted-foreground">
              将签名身份与实际请求使用的 Codex2API Key 对应起来，并可为指定 Key 缩小 Prompt 检查范围。
              全局阈值和审核档位仍由 GuardPipeline 管理，调用方不能通过请求头或元数据修改此策略。
            </p>
          </div>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={`size-4 ${loading ? 'animate-spin' : ''}`} />
              刷新
            </Button>
            <Button size="sm" onClick={openCreate} disabled={loading || availableAPIKeys.length === 0}>
              <Plus className="size-4" />
              新增绑定
            </Button>
          </div>
        </div>

        <div className="flex items-start gap-3 rounded-lg border border-sky-500/25 bg-sky-500/[0.06] p-3">
          <ShieldCheck className="mt-0.5 size-5 shrink-0 text-sky-600 dark:text-sky-300" />
          <div className="text-sm leading-6">
            <div className="font-medium">绑定后使用该 Key 的审计密钥</div>
            <div className="text-muted-foreground">
              签名缺失、调用方代码错误或密钥不匹配时，不会借用其他 Key 的身份，也不会把审计记录关联到错误绑定。
            </div>
          </div>
        </div>

        {error ? (
          <div className="flex items-center justify-between gap-3 rounded-lg border border-destructive/25 bg-destructive/[0.05] p-3 text-sm text-destructive">
            <span>{error}</span>
            <Button variant="outline" size="sm" onClick={() => void load()}>重试</Button>
          </div>
        ) : null}

        {loading ? (
          <div className="flex min-h-28 items-center justify-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />正在读取审计身份绑定…
          </div>
        ) : bindings.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border p-7 text-center">
            <div className="text-sm font-medium">还没有按 Key 配置的审计身份</div>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">
              新建后会自动生成 256 位绑定密钥，并且只展示一次。
            </p>
            <Button className="mt-4" size="sm" onClick={openCreate} disabled={availableAPIKeys.length === 0}>
              <Plus className="size-4" />新增第一个绑定
            </Button>
            {availableAPIKeys.length === 0 ? <p className="mt-2 text-xs text-amber-600">没有可绑定的 API Key，请先新建独立 Key。</p> : null}
          </div>
        ) : (
            <div className="overflow-x-auto rounded-lg border border-border">
              <table className="w-full min-w-[860px] text-sm">
                <thead className="bg-muted/50 text-left text-xs text-muted-foreground">
                  <tr>
                    <th className="px-3 py-2.5 font-medium">调用方</th>
                    <th className="px-3 py-2.5 font-medium">Codex2API Key</th>
                    <th className="px-3 py-2.5 font-medium">状态</th>
                    <th className="px-3 py-2.5 font-medium">绑定密钥</th>
                    <th className="px-3 py-2.5 font-medium">更新时间</th>
                    <th className="px-3 py-2.5 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {bindings.map((binding) => {
                    const apiKey = apiKeyByID.get(binding.api_key_id)
                    return (
                      <tr key={binding.api_key_id} className="align-top">
                        <td className="px-3 py-3">
                          <div className="font-medium">{binding.platform_name || binding.platform_code}</div>
                          <div className="mt-0.5 font-mono text-xs text-muted-foreground">{binding.platform_code}</div>
                        </td>
                        <td className="px-3 py-3">
                          <div className="font-medium">#{binding.api_key_id} {apiKey?.name || 'Key 已删除或不可见'}</div>
                          <div className="mt-0.5 font-mono text-xs text-muted-foreground">{apiKey?.key || '—'}</div>
                        </td>
                        <td className="px-3 py-3">
                          <div className="flex flex-wrap gap-1.5">
                            <Badge variant={binding.enabled ? 'default' : 'outline'}>{binding.enabled ? '已启用' : '已关闭'}</Badge>
                            <Badge variant={binding.require_signed_identity ? 'default' : 'outline'}>
                              {binding.require_signed_identity ? '强制签名' : '未强制签名'}
                            </Badge>
                            <Badge variant={binding.prompt_filter_scope === 'off' ? 'destructive' : 'outline'}>
                              {binding.prompt_filter_scope === 'local_only'
                                ? '仅本地检测'
                                : binding.prompt_filter_scope === 'off'
                                  ? '跳过 Prompt 检查'
                                  : '继承全局审核'}
                            </Badge>
                          </div>
                        </td>
                        <td className="px-3 py-3">
                          <div className="font-mono text-xs">{binding.secret_configured ? binding.secret_masked : '未配置'}</div>
                          {binding.previous_secret_active ? (
                            <div className="mt-1 text-xs text-amber-600">
                              旧密钥宽限至 {formatBeijingTime(binding.previous_secret_expires_at)}
                            </div>
                          ) : null}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-xs text-muted-foreground">{formatBeijingTime(binding.updated_at)}</td>
                        <td className="px-3 py-3">
                          <div className="flex justify-end gap-1.5">
                            <Button variant="outline" size="sm" onClick={() => openEdit(binding)}>
                              <Pencil className="size-3.5" />编辑
                            </Button>
                            <Button variant="outline" size="sm" onClick={() => { setGraceSeconds('300'); setRotating(binding) }}>
                              <RefreshCw className="size-3.5" />轮换
                            </Button>
                            <Button variant="ghost" size="icon" className="text-destructive" aria-label="删除绑定" onClick={() => void deleteBinding(binding)}>
                              <Trash2 className="size-4" />
                            </Button>
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
        )}
      </section>

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>新增 Key 审计身份绑定</DialogTitle>
            <DialogDescription>选择请求实际使用的 Codex2API Key。保存后系统自动生成绑定密钥，并只展示一次。</DialogDescription>
          </DialogHeader>
          <BindingFormFields form={createForm} setForm={setCreateForm} apiKeys={availableAPIKeys} editing={false} />
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>取消</Button>
            <Button onClick={() => void createBinding()} disabled={saving || !createForm.apiKeyId || !createForm.platformCode.trim()}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
              创建并生成密钥
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(editing)} onOpenChange={(open) => { if (!open && !saving) setEditing(null) }}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>编辑审计身份绑定</DialogTitle>
            <DialogDescription>管理调用方身份及该 Key 的检查范围；全局拦截阈值和审核档位仍由统一 GuardPipeline 管理。</DialogDescription>
          </DialogHeader>
          <BindingFormFields form={editForm} setForm={setEditForm} apiKeys={apiKeys.filter((key) => key.id === editing?.api_key_id)} editing />
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditing(null)} disabled={saving}>取消</Button>
            <Button onClick={() => void updateBinding()} disabled={saving || !editForm.platformCode.trim()}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
              保存修改
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(rotating)} onOpenChange={(open) => { if (!open && !saving) setRotating(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>轮换“{rotating?.platform_name || rotating?.platform_code}”绑定密钥</DialogTitle>
            <DialogDescription>系统会生成新密钥。宽限期内新旧密钥都可验证，便于无中断更新对应的 NewAPI 配置。</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <label className="space-y-1.5">
              <span className="text-sm font-medium">旧密钥宽限秒数</span>
              <Input type="number" min={60} max={86400} step={1} value={graceSeconds} onChange={(event) => setGraceSeconds(event.target.value)} />
              <span className="block text-xs leading-5 text-muted-foreground">建议 300 秒，最少 60 秒；即使浏览器在响应返回前断开，旧密钥仍可用于恢复并重新轮换。</span>
            </label>
            <div className="flex items-start gap-2 rounded-lg border border-amber-500/25 bg-amber-500/[0.06] p-3 text-sm leading-6">
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
              轮换成功后，明文新密钥只展示一次。请在宽限期结束前更新对应调用方，其他绑定无需改动。
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRotating(null)} disabled={saving}>取消</Button>
            <Button onClick={() => void rotateSecret()} disabled={saving}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
              确认轮换
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(secretReveal)} onOpenChange={(open) => { if (!open) requestCloseSecretReveal() }}>
        <DialogContent
          className="sm:max-w-2xl"
          onEscapeKeyDown={(event) => { event.preventDefault(); requestCloseSecretReveal() }}
          onPointerDownOutside={(event) => { event.preventDefault(); requestCloseSecretReveal() }}
        >
          <DialogHeader>
            <DialogTitle>{secretReveal?.platformName} 绑定密钥</DialogTitle>
            <DialogDescription>密钥已经保存到 Codex2API 数据库，但明文只在本次响应中展示。列表不会再次回显。</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="flex gap-2">
              <Input readOnly value={secretReveal?.secret ?? ''} className="font-mono text-xs" />
              <Button variant="outline" onClick={() => void copySecret()}>
                {secretCopied ? <Check className="size-4" /> : <Copy className="size-4" />}
                {secretCopied ? '已复制' : '复制密钥'}
              </Button>
            </div>
            <div className="rounded-lg border border-amber-500/25 bg-amber-500/[0.06] p-3 text-sm leading-6 text-amber-800 dark:text-amber-200">
              请把此密钥只配置到“{secretReveal?.platformName}”对应的 NewAPI。关闭后无法重新获取明文，只能再次轮换。
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={requestCloseSecretReveal}>关闭</Button>
            <Button onClick={() => void copySecret()}>
              <Copy className="size-4" />{secretCopied ? '已复制' : '复制后配置'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={secretCloseConfirmOpen} onOpenChange={setSecretCloseConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>确认关闭密钥窗口？</DialogTitle>
            <DialogDescription>关闭后将清除本次明文展示，列表不会再次显示。以后只能通过轮换获得新密钥。</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setSecretCloseConfirmOpen(false)}>返回复制</Button>
            <Button variant="destructive" onClick={confirmCloseSecretReveal}>确认关闭</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {confirmDialog}
    </>
  )
}
