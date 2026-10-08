package auth

import "strings"

// Grok 账号未声明 models 白名单、且尚未同步到账号目录时的保守默认模型集。
// 这是全仓库唯一权威来源：proxy 的注册/调度面(DefaultGrokModelIDsForAccount)
// 与 auth 的授权门(GrokChannelSupportsModel)都必须从这里取,避免两份清单发散后
// 出现"目录展示但调度拒绝"(或反之)的错配——PR #513 与 grok-4.6 默认集就曾因
// 双份硬编码在合并后互相打架。
//
// 两条通道的目录并不相同,必须分开取:
//   - OAuth 走 cli-chat-proxy,目录由 CLI 通道决定。实测 supergrok_heavy 与 free
//     两种套餐原先只返回 grok-4.5;grok-4.6 / grok-4.7 作为近两代旗舰一并列入兜底,不含 grok-3 / grok-2。
//   - API Key 走 xAI 公开 API,目录更宽。
//
// 默认集只是探测不到、且账号没有模型列表时的兜底:账号导入或连通性测试跑过
// FetchGrokModelIDs 后,应以探到的真实目录为准。grok-4.7-fast 是例外:它是对外
// 公开名,目录里通常没有。账号没写模型列表、且目录里还有文本模型时继续暴露;
// 模型列表没写这个名字时不补。真正发给上游的名字是 grok-4.7-build-fast。

const (
	// GrokFastPublicModelID 是下游模型列表和请求里使用的名字。
	GrokFastPublicModelID = "grok-4.7-fast"
	// GrokFastUpstreamModelID 是 cli-chat-proxy 上的原始模型名。
	GrokFastUpstreamModelID = "grok-4.7-build-fast"
)

// IsGrokFastPublicModel 判断名字是不是对外的 grok-4.7-fast。
func IsGrokFastPublicModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), GrokFastPublicModelID)
}

// GrokWireModelID 把对外模型名换成上游请求名。其他名字原样返回。
func GrokWireModelID(model string) string {
	model = strings.TrimSpace(model)
	if IsGrokFastPublicModel(model) {
		return GrokFastUpstreamModelID
	}
	return model
}

// grokOAuthExcludedModels 是 CLI / OAuth 通道不提供的旧文本模型。
// grok-4.5 及以上（含 grok-4.7-fast）保留；grok-4 以及 grok-3 / grok-2 一代不进入 OAuth 白名单。
var grokOAuthExcludedModels = map[string]struct{}{
	"grok-4":      {},
	"grok-4-fast": {},
	"grok-3":      {},
	"grok-3-fast": {},
	"grok-3-mini": {},
	"grok-2":      {},
}

// GrokModelAllowedForAuthKind 判断该凭据类型能否把模型写入白名单或拿去调度。
// API Key 走公开目录，保留旧模型；OAuth 只保留 grok-4.5 及以上。
func GrokModelAllowedForAuthKind(authKind, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(authKind), GrokAuthKindAPIKey) {
		return true
	}
	_, excluded := grokOAuthExcludedModels[model]
	return !excluded
}

// FilterGrokModelsForAuthKind 按凭据类型丢掉 OAuth 不该声明的旧模型。空列表保持为空。
func FilterGrokModelsForAuthKind(authKind string, models []string) []string {
	if len(models) == 0 || strings.EqualFold(strings.TrimSpace(authKind), GrokAuthKindAPIKey) {
		return models
	}
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if GrokModelAllowedForAuthKind(authKind, model) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// GrokOAuthDefaultModelIDs 返回 OAuth 凭据账号的默认可用文本模型集。
func GrokOAuthDefaultModelIDs() []string {
	return []string{"grok-4.7", GrokFastPublicModelID, "grok-4.6", "grok-4.5"}
}

// GrokAPIKeyDefaultModelIDs 返回 API Key 凭据账号的默认可用文本模型集。
func GrokAPIKeyDefaultModelIDs() []string {
	return []string{"grok-4.7", GrokFastPublicModelID, "grok-4.6", "grok-4.5", "grok-4", "grok-3-fast", "grok-3", "grok-2"}
}

// 媒体（生图/生视频）模型集与文本模型集刻意分开:文本调度准入
// (grokAccountSupportsVisibleModel)以文本集/账号目录为准,媒体端点走独立的
// 准入(grokMediaModelsForAccount),互不放大对方的可路由范围。
// 媒体模型不出现在上游 /models 目录里,因此没有"目录同步后以目录为准"的回退,
// 默认集就是权威候选;账号声明的 models 白名单中出现 grok-imagine 条目时,
// 以声明为准收窄。

// GrokImageDefaultModelIDs 返回 Grok 账号默认可用的生图模型集。
func GrokImageDefaultModelIDs() []string {
	return []string{"grok-imagine-image", "grok-imagine-image-quality"}
}

// GrokVideoDefaultModelIDs 返回 Grok 账号默认可用的生视频模型集。
// grok-imagine-video-1.5 在 xAI 公开 API 上以 -preview 后缀发布,两个名字
// 都接受,转发时按上游 profile 归一(mapGrokMediaModelForProfile)。
func GrokVideoDefaultModelIDs() []string {
	return []string{"grok-imagine-video", "grok-imagine-video-1.5", "grok-imagine-video-1.5-preview"}
}
