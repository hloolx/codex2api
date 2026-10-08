package admin

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

const automaticModelDiscoveryTimeout = 30 * time.Second

// 自动价格轮询先发现模型，价格同步即可包含本轮新增的型号。
// 发现失败不阻断已有模型的价格更新，具体失败同时写入同步状态。
func (h *Handler) runAutomaticModelPricingSync(ctx context.Context, cfg *database.OfficialPricingSyncConfig) (*proxy.OfficialPricingSyncResult, error) {
	warnings := h.discoverModelsForPricing(ctx, cfg)
	return h.runOfficialPricingSyncWithWarnings(ctx, proxy.OfficialPricingSyncOptions{
		IncludeOpenAI: cfg.IncludeOpenAI, IncludeGrok: cfg.IncludeGrok, IncludeClaude: cfg.IncludeClaude,
	}, warnings)
}

func (h *Handler) discoverModelsForPricing(ctx context.Context, cfg *database.OfficialPricingSyncConfig) []string {
	discoveryCtx, cancel := context.WithTimeout(ctx, automaticModelDiscoveryTimeout)
	defer cancel()
	selected := map[string]bool{
		database.UpstreamChannelCodex:  cfg.IncludeOpenAI,
		database.UpstreamChannelGrok:   cfg.IncludeGrok,
		database.UpstreamChannelClaude: cfg.IncludeClaude,
	}
	funcs := h.modelRefreshFuncs
	if funcs == nil {
		funcs = h.defaultModelRefreshFuncs()
	}
	results := make(chan []string, len(selected))
	pending := 0
	for channel, enabled := range selected {
		if !enabled {
			continue
		}
		pending++
		go func(channel string) {
			results <- discoverChannelModelsForPricing(discoveryCtx, channel, funcs[channel])
		}(channel)
	}
	var warnings []string
	for range pending {
		warnings = append(warnings, (<-results)...)
	}
	sort.Strings(warnings)
	return warnings
}

func discoverChannelModelsForPricing(ctx context.Context, channel string, refresh channelModelRefreshFunc) []string {
	if refresh == nil {
		return []string{fmt.Sprintf("%s 模型自动刷新不可用", channel)}
	}
	var warnings []string
	emit := func(event modelRefreshEvent) {
		if event.Error != "" {
			// 套餐探测并行回调；通过日志报告，汇总使用最终结果避免共享切片竞态。
			log.Printf("模型自动发现失败: channel=%s plan=%s error=%s", channel, event.Plan, event.Error)
		}
	}
	result := runChannelModelRefresh(ctx, channel, refresh, emit)
	if result.Error != "" {
		warnings = append(warnings, fmt.Sprintf("%s 模型自动刷新失败: %s", channel, result.Error))
	}
	if result.Failed > 0 {
		warnings = append(warnings, fmt.Sprintf("部分 %s 模型来源自动刷新失败，已保留未成功更新的数据；详见服务日志", channel))
	}
	log.Printf("模型自动发现完成: channel=%s added=%v refreshed=%d failed=%d", channel, result.Added, result.Refreshed, result.Failed)
	return warnings
}
