package proxy

import (
	"log"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/codex2api/database"
)

// 上游响应模型观测（移植自 sub2api 的 upstream response model audit）：
// 记录一次转发尝试内上游响应自报的模型名，与实际发往上游的模型对比，写入用量日志，
// 供管理端「模型不一致」审计与筛选使用。观测绝不影响转发与计费路径。
//
//   - first：首个声明（如 response.created 帧携带的 response.model）
//   - terminal：终态事件声明（response.completed / response.incomplete / response.failed），
//     存在时优先生效
//   - conflict：同一次尝试内出现互相矛盾的自报——上游混流或中途换模型的信号
type upstreamResponseModelObserver struct {
	first    string
	terminal string
	conflict bool
}

// upstreamResponseModelMaxLength 与 usage_logs.upstream_response_model 列宽一致。
const upstreamResponseModelMaxLength = 200

func (o *upstreamResponseModelObserver) Observe(model string, terminal bool) {
	model = normalizeObservedUpstreamResponseModel(model)
	if model == "" {
		return
	}
	if current := o.Model(); current != "" && !strings.EqualFold(current, model) {
		o.conflict = true
	}
	if terminal {
		o.terminal = model
		return
	}
	if o.first == "" {
		o.first = model
	}
}

// Model 返回本次尝试观测到的上游自报模型：终态声明优先，否则取首个声明；
// 上游从未自报时为空串。
func (o *upstreamResponseModelObserver) Model() string {
	if o == nil {
		return ""
	}
	if o.terminal != "" {
		return o.terminal
	}
	return o.first
}

func (o *upstreamResponseModelObserver) Conflict() bool {
	return o != nil && o.conflict
}

func normalizeObservedUpstreamResponseModel(model string) string {
	model = strings.TrimSpace(model)
	runes := []rune(model)
	if len(runes) > upstreamResponseModelMaxLength {
		model = string(runes[:upstreamResponseModelMaxLength])
	}
	return model
}

// observeUpstreamResponseModelFrame 供 SSE 帧循环逐帧调用：帧体 response.model
// 非空即记录；成功终态与 response.failed 按终态声明处理（终态声明优先生效）。
func observeUpstreamResponseModelFrame(o *upstreamResponseModelObserver, parsed gjson.Result, eventType string) {
	if o == nil {
		return
	}
	if model := parsed.Get("response.model").String(); model != "" {
		o.Observe(model, isResponsesSuccessTerminalEvent(eventType) || eventType == "response.failed")
	}
}

// observeUpstreamResponseModelBody 供一次性响应体（非流式 JSON / compact 聚合结果）
// 调用：整个 body 就是终态。
func observeUpstreamResponseModelBody(o *upstreamResponseModelObserver, body []byte) {
	if o == nil || len(body) == 0 {
		return
	}
	o.Observe(gjson.GetBytes(body, "model").String(), true)
}

// upstreamModelMismatch 三态判定：上游未自报 → nil（不参与筛选与展示，历史行同为
// NULL）；自报了 → 与实发模型大小写不敏感地严格比对。变体级别的宽松归一化（-latest /
// 日期后缀）不在此处做——那是前端徽章分级的展示语义，落库值保持原始对比结果。
func upstreamModelMismatch(sentModel, responseModel string) *bool {
	responseModel = strings.TrimSpace(responseModel)
	if responseModel == "" {
		return nil
	}
	sentModel = strings.TrimSpace(sentModel)
	mismatch := sentModel == "" || !strings.EqualFold(sentModel, responseModel)
	return &mismatch
}

// upstreamSentModelForAudit 返回审计对比用的实发模型：优先 attempt 实发
// （账号级映射可能改写），兜底客户端请求模型。
func upstreamSentModelForAudit(attemptModel, logModel string) string {
	if m := strings.TrimSpace(attemptModel); m != "" {
		return m
	}
	return strings.TrimSpace(logModel)
}

// applyUpstreamResponseModelObservation 把观测结果写入用量日志输入。上游未自报时
// 两个字段保持零值/nil。conflict 打一条告警（对齐 sub2api 的
// upstream_response_model_conflict），落库值取终态优先的 Model()。
func applyUpstreamResponseModelObservation(input *database.UsageLogInput, o *upstreamResponseModelObserver, sentModel string, accountID int64) {
	if input == nil || o == nil {
		return
	}
	model := o.Model()
	if model == "" {
		return
	}
	if o.Conflict() {
		log.Printf("upstream_response_model_conflict (account %d, endpoint %s): sent_model=%s, selected_response_model=%s", accountID, input.Endpoint, sentModel, model)
	}
	input.UpstreamResponseModel = model
	input.UpstreamModelMismatch = upstreamModelMismatch(sentModel, model)
}
