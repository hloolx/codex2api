package proxy

import (
	"testing"

	"github.com/codex2api/auth"
)

// TestDefaultGrokModelIDsForAccountByAuthKind 守护两条通道目录不同这一实测事实：
// OAuth 走 cli-chat-proxy，兜底当前旗舰 grok-4.7 / grok-4.6 / grok-4.5；API Key 走 xAI 公开 API，目录更宽。
func TestDefaultGrokModelIDsForAccountByAuthKind(t *testing.T) {
	oauth := &auth.Account{UpstreamType: auth.UpstreamGrok, RefreshToken: "rt"}
	gotOAuth := DefaultGrokModelIDsForAccount(oauth)
	if !modelIDInList("grok-4.7", gotOAuth) || !modelIDInList("grok-4.7-fast", gotOAuth) || !modelIDInList("grok-4.6", gotOAuth) || !modelIDInList("grok-4.5", gotOAuth) {
		t.Fatalf("OAuth 默认集 = %v, want grok-4.7 / grok-4.7-fast / grok-4.6 / grok-4.5", gotOAuth)
	}
	for _, model := range []string{"grok-3", "grok-2", "grok-3-fast"} {
		if modelIDInList(model, gotOAuth) {
			t.Errorf("OAuth 默认集不应含 %s, got %v", model, gotOAuth)
		}
	}

	apiKey := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-key"}
	if apiKey.GrokAuthKind() != auth.GrokAuthKindAPIKey {
		t.Fatalf("构造的账号应被识别为 API Key，实际 %q", apiKey.GrokAuthKind())
	}
	got := DefaultGrokModelIDsForAccount(apiKey)
	if len(got) <= len(gotOAuth) {
		t.Fatalf("API Key 默认集应比 OAuth 宽, oauth=%v apiKey=%v", gotOAuth, got)
	}
	if !modelIDInList("grok-4.7", got) || !modelIDInList("grok-4.7-fast", got) || !modelIDInList("grok-4.6", got) || !modelIDInList("grok-3", got) {
		t.Fatalf("API Key 默认集应含 grok-4.7 / grok-4.7-fast / grok-4.6 与 grok-3, got %v", got)
	}

	// 空账号按 OAuth 处理：CLI 通道是更保守的一侧，宁可少放行也不要advertise 不存在的模型。
	if nilGot := DefaultGrokModelIDsForAccount(nil); len(nilGot) != len(gotOAuth) {
		t.Fatalf("空账号应回落到最保守的 OAuth 默认集, got %v", nilGot)
	}
}

// TestRelayAccountSupportsModelHonoursAuthKind OAuth 账号不再被放行到 CLI 通道
// 不存在的模型（grok-3 等），避免调度到必然失败的账号上。
func TestRelayAccountSupportsModelHonoursAuthKind(t *testing.T) {
	oauth := &auth.Account{UpstreamType: auth.UpstreamGrok, RefreshToken: "rt"}
	if !relayAccountSupportsModel(oauth, "grok-4.6") {
		t.Fatalf("OAuth 账号应支持 grok-4.6")
	}
	if !relayAccountSupportsModel(oauth, "grok-4.5") {
		t.Fatalf("OAuth 账号应支持 grok-4.5")
	}
	if !relayAccountSupportsModel(oauth, "grok-4.6") {
		t.Fatalf("OAuth 账号应支持 grok-4.6")
	}
	for _, model := range []string{"grok-4", "grok-3", "grok-2", "grok-3-fast"} {
		if relayAccountSupportsModel(oauth, model) {
			t.Errorf("OAuth 账号不应被放行到 %s（CLI 通道无此模型）", model)
		}
	}
	declaredOld := &auth.Account{
		UpstreamType: auth.UpstreamGrok,
		RefreshToken: "rt",
		Models:       []string{"grok-4", "grok-4.7"},
	}
	declaredOld.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{
		{ModelID: "grok-4", APIBackend: auth.GrokProtocolResponses},
		{ModelID: "grok-4.7", APIBackend: auth.GrokProtocolResponses},
	}})
	if relayAccountSupportsModel(declaredOld, "grok-4") {
		t.Fatal("OAuth 白名单或目录里的 grok-4 也不能调度")
	}
	if !relayAccountSupportsModel(declaredOld, "grok-4.7") {
		t.Fatal("OAuth 仍应调度 grok-4.7")
	}
	narrowCatalog := &auth.Account{
		UpstreamType: auth.UpstreamGrok,
		RefreshToken: "rt",
		Models:       []string{"grok-4.5", "grok-4.6", "grok-4.7", "grok-4.7-fast"},
	}
	narrowCatalog.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.7", APIBackend: auth.GrokProtocolResponses}}})
	for _, model := range []string{"grok-4.5", "grok-4.6", "grok-4.7", "grok-4.7-fast"} {
		if !relayAccountSupportsModel(narrowCatalog, model) {
			t.Fatalf("白名单里的 OAuth 预设模型 %s 应可调度", model)
		}
	}
	if modelIDInList("grok-4", GrokVisibleModelIDsForAccount(declaredOld)) {
		t.Fatal("OAuth 可见模型不应包含 grok-4")
	}

	apiKey := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-key"}
	if !relayAccountSupportsModel(apiKey, "grok-3") {
		t.Fatalf("API Key 账号应仍支持 grok-3（公开 API 目录）")
	}
}

// TestRelayAccountSupportsModelRespectsDeclaredWhitelist 账号显式声明 models 只
// 能收窄真实目录，不能凭配置把 OAuth 默认目录之外的模型发明出来。
func TestRelayAccountSupportsModelRespectsDeclaredWhitelist(t *testing.T) {
	declared := &auth.Account{
		UpstreamType: auth.UpstreamGrok,
		RefreshToken: "rt",
		Models:       []string{"grok-3"},
	}
	if relayAccountSupportsModel(declared, "grok-3") {
		t.Fatalf("OAuth 无目录时不能仅凭声明放行 grok-3")
	}
	if relayAccountSupportsModel(declared, "grok-4.5") {
		t.Fatalf("声明白名单后不应再补默认集放行 grok-4.5")
	}
	declared.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-3", APIBackend: auth.GrokProtocolResponses}}})
	if relayAccountSupportsModel(declared, "grok-3") {
		t.Fatal("OAuth 不调度 grok-4.5 以下的模型，即使目录和白名单都写了 grok-3")
	}
	declared.Models = []string{"grok-catalog-only"}
	declared.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-catalog-only", APIBackend: auth.GrokProtocolResponses}}})
	if !relayAccountSupportsModel(declared, "grok-catalog-only") {
		t.Fatal("目录与声明同时命中的非旧模型应放行")
	}
}

func TestGrokChannelSupportsModelUsesCatalogBeforeDefaults(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, RefreshToken: "rt"}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-catalog-only", APIBackend: auth.GrokProtocolResponses}}})
	if !account.GrokChannelSupportsModel("grok-catalog-only") {
		t.Fatal("catalog model should be routable")
	}
	if account.GrokChannelSupportsModel("grok-4.5") {
		t.Fatal("non-empty catalog must replace conservative default set")
	}
}
