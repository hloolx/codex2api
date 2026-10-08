package proxy

import (
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// grokVideoSettleNamespace 记录已结算的视频任务:SETNX 语义保证重复轮询、
	// 下载与多实例并发只结算一次。TTL 与任务绑定一致,绑定过期后任务也查不到了。
	grokVideoSettleNamespace = "grok_video_settle"
	grokVideoSettleOwner     = "settled"

	// xAI 以 USD ticks 自报成本:1 USD = 1e10 ticks。
	grokUSDTicksPerDollar = 1e10

	// 上游缺省时长:生成 8 秒、续写 6 秒(docs.x.ai videos API)。编辑沿用源视频
	// 长度,网关无从得知,只能依赖状态体里的 video.duration。
	grokVideoDefaultGenerationSeconds = 8
	grokVideoDefaultExtensionSeconds  = 6
)

// grokUpstreamCostUSD 解析 xAI 响应里的 usage.cost_in_usd_ticks;未自报返回 0。
func grokUpstreamCostUSD(body []byte) float64 {
	ticks := gjson.GetBytes(body, "usage.cost_in_usd_ticks")
	if !ticks.Exists() || ticks.Float() <= 0 {
		return 0
	}
	return ticks.Float() / grokUSDTicksPerDollar
}

// grokVideoExplicitSeconds 读取请求显式给出的时长;上游同时接受 duration 与
// OpenAI 兼容的 seconds,数字或数字字符串均可。
func grokVideoExplicitSeconds(rawBody []byte) int64 {
	for _, field := range []string{"duration", "seconds"} {
		if v := gjson.GetBytes(rawBody, field); v.Exists() && v.Int() > 0 {
			return v.Int()
		}
	}
	return 0
}

// grokVideoRequestedSeconds 返回提交时可推知的视频时长,仅作状态体缺失
// video.duration 时的兜底计费依据。
func grokVideoRequestedSeconds(rawBody []byte, operation string) int {
	if seconds := grokVideoExplicitSeconds(rawBody); seconds > 0 {
		return int(seconds)
	}
	switch operation {
	case "generations":
		return grokVideoDefaultGenerationSeconds
	case "extensions":
		return grokVideoDefaultExtensionSeconds
	}
	return 0
}

// grokVideoFailureStatus 把状态体里的 VideoError.code 映射为用量日志状态码。
func grokVideoFailureStatus(code string) int {
	switch code {
	case "invalid_argument", "failed_precondition":
		return http.StatusBadRequest
	case "permission_denied":
		return http.StatusForbidden
	case "service_unavailable":
		return http.StatusServiceUnavailable
	case "internal_error":
		return http.StatusInternalServerError
	}
	return http.StatusBadGateway
}

// settleGrokVideo 在状态查询首次观察到终态时为视频任务记一条结算用量:
//   - done:上游成本按生成秒数(或上游自报成本)计;用户按次/按秒计费只计已交付
//     (有产物 URL)的视频,审核拦截(respect_moderation=false)不向用户收费;
//   - failed / expired:不计费,只留一条失败记录说明原因。
//
// 提交行只代表"提交成功",金额为 0;客户端从不轮询的任务不会结算。
func (h *Handler) settleGrokVideo(c *gin.Context, requestID string, binding grokVideoBinding, body []byte, durationMs int) {
	status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String()))
	if status != "done" && status != "failed" && status != "expired" {
		return
	}
	if h == nil || h.cache == nil {
		return
	}
	claimed, err := h.cache.AcquireLease(c.Request.Context(), grokVideoSettleNamespace, requestID, grokVideoSettleOwner, grokVideoBindingTTL)
	if err != nil {
		log.Printf("Grok 视频任务结算占位失败,本次跳过 (request_id=%s): %v", requestID, err)
		return
	}
	if !claimed {
		return
	}

	model := binding.RequestModel
	if model == "" {
		model = binding.Model
	}
	const endpoint = "/v1/videos/:request_id"
	input := &database.UsageLogInput{
		AccountID: binding.AccountID, Endpoint: endpoint, Model: model, EffectiveModel: binding.EffectiveModel,
		StatusCode: http.StatusOK, DurationMs: durationMs,
		InboundEndpoint: endpoint, UpstreamEndpoint: endpoint, Stream: false,
		UpstreamRequestID: requestID,
		UpstreamCostUSD:   grokUpstreamCostUSD(body),
	}
	switch status {
	case "done":
		input.VideoSeconds = int(gjson.GetBytes(body, "video.duration").Int())
		if input.VideoSeconds <= 0 {
			input.VideoSeconds = binding.RequestedSeconds
		}
		if strings.TrimSpace(gjson.GetBytes(body, "video.url").String()) != "" {
			input.VideoCount = 1
		} else {
			input.UpstreamErrorKind = "moderation_blocked"
			input.ErrorMessage = "video generated but withheld by upstream moderation"
		}
	case "failed":
		input.StatusCode = grokVideoFailureStatus(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
		input.UpstreamErrorKind = "video_failed"
		input.ErrorMessage = strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
		if input.ErrorMessage == "" {
			input.ErrorMessage = "video generation failed"
		}
	case "expired":
		input.StatusCode = http.StatusGone
		input.UpstreamErrorKind = "video_expired"
		input.ErrorMessage = "video generation request expired"
	}
	h.logUsageForRequest(c, input)
}
