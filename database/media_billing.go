package database

import (
	"math"
	"strings"
)

// Grok Imagine 媒体模型不按 token 计费:上游按媒体单位收费(生图按张、生视频按秒)。
// 这些模型绝不能落进文本 token 兜底价($1/$2 每百万 token),否则成本被低估到近乎 0。
const (
	MediaUnitImage  = "image"
	MediaUnitSecond = "second"

	// 视频的用户计费模式:按成功交付的视频次数,或按交付视频的生成时长(秒)。
	UserBillingModePerVideo  = "per_video"
	UserBillingModePerSecond = "per_second"
)

// grokMediaUnitCosts 是 xAI 官方 Imagine 价目快照(docs.x.ai/developers/pricing,
// 2026-09-28):生图 USD/张,生视频 USD/秒。未收录的型号上游成本记为未定价(0),
// 由定价页提示管理员补填,而不是套用文本 token 价。
var grokMediaUnitCosts = map[string]float64{
	"grok-imagine-image":         0.02,
	"grok-imagine-image-2.0":     0.04,
	"grok-imagine-image-quality": 0.05,
	"grok-imagine-video":         0.05,
	"grok-imagine-video-1.5":     0.08,
}

// GrokMediaPricingKey 返回 Grok Imagine 媒体模型的规范定价键;非媒体模型返回空串。
// 裸别名 grok-imagine 与网关的生图归一一致,指向质量档;video-1.5 的 -preview
// 发布名与正式名共用一行价格。
func GrokMediaPricingKey(model string) string {
	normalized := normalizeBillingModelName(model)
	if !strings.HasPrefix(normalized, "grok-imagine") {
		return ""
	}
	switch normalized {
	case "grok-imagine":
		return "grok-imagine-image-quality"
	case "grok-imagine-video-1.5-preview":
		return "grok-imagine-video-1.5"
	}
	return normalized
}

// MediaBillingUnit 返回媒体模型的上游计费单位(image / second);非媒体模型返回空串。
func MediaBillingUnit(model string) string {
	key := GrokMediaPricingKey(model)
	switch {
	case strings.HasPrefix(key, "grok-imagine-video"):
		return MediaUnitSecond
	case strings.HasPrefix(key, "grok-imagine-image"):
		return MediaUnitImage
	}
	return ""
}

func grokMediaBasePricing(key string) *ModelPricing {
	return &ModelPricing{MediaUnitCost: grokMediaUnitCosts[key]}
}

// IsUnitUserBillingMode 判断用户计费模式是否按媒体单位(张/次/秒)结算,
// 这类模式的用户金额 = 单价 × 计费单位数,与上游成本相互独立。
func IsUnitUserBillingMode(mode string) bool {
	return mode == UserBillingModePerImage || mode == UserBillingModePerVideo || mode == UserBillingModePerSecond
}

// usageLogMediaUnits 返回一条用量事件按上游口径的媒体计费单位数:
// 生图 = 成功交付张数;生视频 = 上游生成时长(秒)。上游对审核拦截的视频照样收费,
// 因此视频秒数不看是否交付。
func usageLogMediaUnits(log *UsageLogInput, unit string) float64 {
	if log == nil || log.StatusCode < 200 || log.StatusCode >= 300 {
		return 0
	}
	switch unit {
	case MediaUnitImage:
		return float64(max(0, log.ImageCount))
	case MediaUnitSecond:
		return float64(max(0, log.VideoSeconds))
	}
	return 0
}

// mediaUsageCost 计算媒体请求的上游成本。优先级:管理员手填的单位成本 >
// 上游自报成本(xAI usage.cost_in_usd_ticks) > 同步/内置单位成本 × 单位数。
func mediaUsageCost(log *UsageLogInput, model, unit string) float64 {
	key := GrokMediaPricingKey(model)
	if ov, ok := lookupModelPricingOverride(key); ok && ov.Source == ModelPricingSourceCustom && ov.MediaUnitCost > 0 {
		return usageLogMediaUnits(log, unit) * ov.MediaUnitCost
	}
	if cost := log.UpstreamCostUSD; cost > 0 && !math.IsInf(cost, 0) {
		return cost
	}
	return usageLogMediaUnits(log, unit) * GetModelPricing(key).MediaUnitCost
}

// userBillingUnits 返回用户按单位计费模式下的计费数量,只计成功交付的产物。
func userBillingUnits(input *UsageLogInput, mode string) int {
	if input.StatusCode < 200 || input.StatusCode >= 300 || input.IsRetryAttempt || input.ErrorMessage != "" {
		return 0
	}
	switch mode {
	case UserBillingModePerImage:
		return max(0, input.ImageCount)
	case UserBillingModePerVideo:
		return max(0, input.VideoCount)
	case UserBillingModePerSecond:
		if input.VideoCount <= 0 {
			return 0
		}
		return max(0, input.VideoSeconds)
	}
	return 0
}
