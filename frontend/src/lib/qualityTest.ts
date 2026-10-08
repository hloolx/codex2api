import type { ClaudeTestEvent } from './claudeConnectionTest'
import type { CodexTestDiagnostics } from './codexConnectionTest'

export const PELICAN_PROMPT = '创建一个 HTML，内容是用 SVG 绘制一个鹈鹕骑自行车的 2D 动画。你不需要任何测试。'
export const QUALITY_TEST_OUTPUT_LIMIT = 1024 * 1024

// Built-in presets ship with the studio and are not stored; names live in the locale
// files under qualityTest.presets.builtins.<key>. The pelican prompt is the default.
export interface QualityTestBuiltinPreset { key: string; prompt: string }
export const QUALITY_TEST_BUILTIN_PRESETS: QualityTestBuiltinPreset[] = [
  { key: 'pelican', prompt: PELICAN_PROMPT },
  { key: 'polarbear', prompt: "创建一个 HTML，用 SVG 绘制一只北极熊骑自行车的 2D 动画：北极熊体型圆润、白色毛发、黑鼻子，坐在一辆两轮自行车上，双腿随踏板做圆周蹬踏，车轮持续转动并带辐条，背景是雪地与远处的冰山，雪花缓缓飘落。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'ultraman', prompt: "创建一个 HTML，用 SVG 绘制奥特曼骑摩托车的 2D 动画：奥特曼为银红配色、头顶有鳍状冠、胸口有闪烁的计时器，跨坐在一辆流线型摩托车上向右疾驰，车轮高速旋转、尾部拖出速度线，背景是横向滚动的城市楼群，天上偶尔有怪兽剪影掠过。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'octopus', prompt: "创建一个 HTML，用 SVG 绘制一只章鱼打架子鼓的 2D 动画：章鱼有 8 条可辨的触手，分别握着鼓棒，同时敲击底鼓、军鼓、镲片等至少 5 件鼓组部件，敲击节奏要稳定循环，被击中的部件有震动或闪光反馈，章鱼头部随节拍轻微点动。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'giraffe', prompt: "创建一个 HTML，用 SVG 绘制一只长颈鹿踩滑板的 2D 动画：长颈鹿脖子明显修长、带棕色斑块，四蹄站在一块滑板上从左向右滑行，滑板轮子转动，途中遇到一个小坡道时做起跳与落地动作，脖子随动作前后摆动，背景是热带草原与移动的云。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'mooncat', prompt: "创建一个 HTML，用 SVG 绘制一只穿宇航服的猫在月球上钓鱼的 2D 动画：猫戴着透明头盔、尾巴从宇航服后面伸出，坐在月面环形山边缘，鱼竿伸向一片漂浮的星星湖，钓线末端不时钓起一颗闪光的星星，背景是漆黑太空、缓慢自转的地球与散布的星点，猫会偶尔甩尾。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'clock', prompt: "创建一个 HTML，用 SVG 绘制一个模拟时钟：有表盘刻度、时针、分针、秒针，读取浏览器本地时间实时走针，秒针平滑扫动，时针随分钟联动偏移。表盘下方用数字显示当前时间。所有样式与脚本内联，不引用外部资源。你不需要任何测试。" },
  { key: 'solar', prompt: "创建一个 HTML，用 SVG 绘制太阳系动画：太阳居中，至少包含水星、金星、地球、火星、木星、土星六颗行星沿各自轨道公转，公转周期按真实比例缩放（地球 1 圈时水星约 4 圈、土星约 1/29 圈），地球带一颗绕转的月球，土星有光环，每颗行星标注中文名称。深色星空背景带闪烁星点。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'balls', prompt: "创建一个 HTML，用 Canvas 或 SVG 实现弹跳小球物理模拟：初始生成 8 个不同颜色和半径的小球，受重力作用下落，碰到容器四壁反弹并有能量损耗，小球之间发生碰撞时按动量守恒交换速度，不允许相互穿透或卡在墙内。点击画面任意位置新增一个小球。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'gears', prompt: "创建一个 HTML，用 SVG 绘制一组相互啮合的齿轮传动动画：至少 4 个齿轮，齿数分别为 12、24、36、18，齿形要真实可辨，相邻齿轮的齿必须正确啮合而不重叠或穿透，转速与齿数成反比，转向相邻相反。用不同颜色区分齿轮，并在每个齿轮中心标注齿数。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'city', prompt: "创建一个 HTML，用 SVG 绘制一幅横向无限滚动的城市夜景：至少三层视差（远景山脉与月亮、中景楼群、近景马路），层速由远到近递增；楼窗随机点亮熄灭，马路上有汽车往返行驶并带车灯，天空偶尔划过流星。画面循环时不能出现接缝或跳变。所有样式与脚本内联。你不需要任何测试。" },
  { key: 'strokes', prompt: "创建一个 HTML，用 SVG 路径动画演示汉字「永」的书写过程：按正确笔顺逐笔书写，共 5 笔（点、横折钩、横撇、撇、捺），每一笔用 stroke-dashoffset 动画呈现毛笔运笔效果，笔画粗细有提按变化，写完一笔再写下一笔，全部写完后暂停两秒重新开始。旁边列出笔顺编号。所有样式与脚本内联。你不需要任何测试。" },
]

export interface QualityTestJob {
  id: number
  account_id: number
  account_name: string
  plan_type: string
  channel: string
  model: string
  reasoning_effort: string
  prompt?: string
  output?: string
  status: 'running' | 'cancelling' | 'completed' | 'error' | 'stopped' | 'interrupted'
  preset_kind?: '' | 'builtin' | 'custom'
  preset_ref?: string
  preset_name?: string
  error?: string
  created_at: string
  updated_at: string
  completed_at?: string
  response_model?: string
  duration_ms: number
  first_content_ms?: number
  input_tokens?: number
  output_tokens?: number
  reasoning_tokens?: number
  timeout_ms?: number
  // interrupted: the upstream stream/transport broke before a terminal event (status is error).
  interrupted?: boolean
  retries?: number
}

export const QUALITY_TEST_TIMEOUT_MINUTES = [10, 20, 30]
export const QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES = 20

export interface QualityTestPrompt {
  id: number
  name: string
  prompt: string
  usage_count: number
  last_used_at?: string
  created_at: string
  updated_at: string
}

export interface QualityTestFacets {
  plans: string[]
  models: string[]
  efforts: string[]
  accounts: { id: number; name: string }[]
  presets: { kind: 'builtin' | 'custom'; ref: string; name: string }[]
}

export interface QualityTestJobsResponse {
  jobs: QualityTestJob[]
  active_jobs: QualityTestJob[]
  total: number
  concurrency_limit: number
  facets?: QualityTestFacets
}

// Filter values are the raw stored strings; effort "default" selects runs that used the model default.
// preset: 'none' | 'builtin:<key>' | 'custom:<id>'
export interface QualityTestJobsFilter { plan?: string; model?: string; effort?: string; account_id?: number; preset?: string }

export const EMPTY_QUALITY_TEST_FACETS: QualityTestFacets = { plans: [], models: [], efforts: [], accounts: [], presets: [] }

export function qualityTestFilterQuery(page: number, filter: QualityTestJobsFilter = {}): string {
  const params = new URLSearchParams({ page: String(page), page_size: '20' })
  if (filter.plan) params.set('plan', filter.plan)
  if (filter.model) params.set('model', filter.model)
  if (filter.effort) params.set('effort', filter.effort)
  if (filter.account_id) params.set('account_id', String(filter.account_id))
  if (filter.preset) params.set('preset', filter.preset)
  return params.toString()
}

export function isQualityTestActive(job?: Pick<QualityTestJob, 'status'> | null): boolean {
  return job?.status === 'running' || job?.status === 'cancelling'
}

// Status label key under qualityTest.status; a broken upstream stream is told apart from a rejection.
export function qualityTestStatusKey(job?: Pick<QualityTestJob, 'status' | 'interrupted'> | null): string {
  if (!job) return 'idle'
  return job.status === 'error' && job.interrupted ? 'streamInterrupted' : job.status
}

export function qualityTestPlanTone(plan: string): string {
  const value = plan.trim().toLowerCase()
  if (value.includes('pro')) return 'pro'
  if (value === 'plus') return 'plus'
  if (value === 'team' || value === 'business') return 'team'
  if (['enterprise', 'edu', 'education', 'k12'].includes(value)) return 'enterprise'
  return value === 'free' ? 'free' : 'other'
}

export interface QualityTestEvent extends ClaudeTestEvent {
  codex_diagnostics?: CodexTestDiagnostics
}

// Accept complete documents, Markdown-wrapped HTML, and standalone SVG/fragments.
// Never put the raw model reply into the admin document's DOM.
export function extractQualityTestHTML(output: string): string {
  const blocks = [...output.matchAll(/```(?:html|svg|xml)?[^\S\r\n]*\r?\n([\s\S]*?)```/gi)]
  const candidate = blocks.find((block) => /<!doctype\s+html|<html[\s>]/i.test(block[1]))?.[1]
    ?? blocks.find((block) => /<(?:svg|div|style|main|body)[\s>]/i.test(block[1]))?.[1]
    ?? output.replace(/^\s*```(?:html|svg|xml)?\s*\r?\n/i, '').replace(/\s*```\s*$/, '')
  const start = candidate.search(/<!doctype\s+html|<html[\s>]|<(?:svg|div|style|main|body|section|canvas)[\s>]/i)
  if (start < 0) return ''
  const html = candidate.slice(start).trim()
  const end = html.toLowerCase().lastIndexOf('</html>')
  return end >= 0 ? html.slice(0, end + 7) : html
}

// The policy precedes every byte of generated markup. The iframe must additionally
// use sandbox="allow-scripts" (never allow-same-origin). Inline animation is allowed;
// network requests, external resources, forms, workers and nested frames are blocked.
export function qualityTestPreviewDocument(html: string, exportScript = ''): string {
  const policy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'"
  return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${policy}"><meta name="referrer" content="no-referrer"><meta name="viewport" content="width=device-width, initial-scale=1"><style>html,body{margin:0;min-height:100%;}svg{max-width:100%;}</style></head><body>${html}${QUALITY_TEST_SIZE_SCRIPT}${exportScript}</body></html>`
}

// Reports the rendered content height so the host can size the frame to fit.
// Re-measures only on width changes: a height-triggered resize would loop with
// viewport-relative layouts. Values are untrusted; the host clamps them.
export const QUALITY_TEST_SIZE_SCRIPT = `<script>(function(){var w=innerWidth;function m(){var d=document.documentElement,b=document.body;parent.postMessage({type:'quality-test-preview-size',width:innerWidth,height:Math.max(d?d.scrollHeight:0,b?b.scrollHeight:0)},'*')}addEventListener('load',m);setTimeout(m,400);setTimeout(m,1200);if(document.fonts&&document.fonts.ready)document.fonts.ready.then(m);addEventListener('resize',function(){if(innerWidth!==w){w=innerWidth;m()}})})()</script>`

export const QUALITY_TEST_FRAME_MIN = 320
export const QUALITY_TEST_FRAME_MAX = 1400

export function clampQualityTestFrameHeight(value: unknown): number | undefined {
  const height = Number(value)
  if (!Number.isFinite(height) || height <= 0) return undefined
  return Math.round(Math.min(QUALITY_TEST_FRAME_MAX, Math.max(QUALITY_TEST_FRAME_MIN, height)))
}
