package auth

import (
	"strings"
	"time"
)

// GrokProtocol 是 Grok Build 支持的三种推理线协议。
type GrokProtocol string

const (
	GrokProtocolResponses       GrokProtocol = "responses"
	GrokProtocolChatCompletions GrokProtocol = "chat_completions"
	GrokProtocolMessages        GrokProtocol = "messages"
)

func NormalizeGrokProtocol(raw string) GrokProtocol {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "responses", "/responses", "/v1/responses":
		return GrokProtocolResponses
	case "chat", "chat_completions", "chat-completions", "/chat/completions", "/v1/chat/completions":
		return GrokProtocolChatCompletions
	case "messages", "/messages", "/v1/messages":
		return GrokProtocolMessages
	default:
		return ""
	}
}

const (
	GrokCapabilityOK          = "ok"
	GrokCapabilityDenied      = "denied"
	GrokCapabilityUnsupported = "unsupported"
	GrokCapabilityError       = "error"
	GrokCapabilityUnknown     = "unknown"
	grokCatalogStaleIfError   = time.Hour
)

// GrokModelRoute 是持久化富目录在运行时所需的最小、只读投影。
type GrokModelRoute struct {
	ModelID                 string
	BaseURL                 string
	APIBaseURL              string
	APIBackend              GrokProtocol
	ExtraHeaders            map[string]string
	SupportedInAPI          *bool
	Hidden                  bool
	ContextWindow           int64
	MaxCompletionTokens     int64
	SupportsReasoningEffort bool
	// ReasoningEffort 是目录声明的默认档（reasoning_effort）。请求没带 effort 时不注入。
	ReasoningEffort string
	// ReasoningEfforts 是上游 /v1/models 公布的线协议档位。空表示目录没有菜单，
	// 请求侧继续用版本启发式（grok-4.6 起放行 xhigh）。
	ReasoningEfforts      []string
	SupportsBackendSearch bool
	StreamToolCalls       bool
	FirstSeenAt           time.Time
}

// grokKnownReasoningEfforts 是 grok-build 线协议能点名的档位。
// 目录里的未知值会被丢掉，避免把展示文案当成可转发的 effort。
var grokKnownReasoningEfforts = map[string]struct{}{
	"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// NormalizeGrokReasoningMenu 把目录菜单收成小写、去重、只保留已知档位，并保持原顺序。
// 没有任何已知档位时返回 nil，调用方据此退回静态折叠。
func NormalizeGrokReasoningMenu(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := grokKnownReasoningEfforts[value]; !ok {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GrokProtocolCapability 是一次账号/模型/协议探针结论。
type GrokProtocolCapability struct {
	ModelID    string
	Origin     string
	Protocol   GrokProtocol
	Status     string
	ObservedAt time.Time
	ExpiresAt  time.Time
}

// GrokRoutingState 与一个账号当前 credential generation 绑定。
type GrokRoutingState struct {
	CredentialGeneration int64
	// CatalogKnown distinguishes an authoritative empty catalog from "catalog
	// has never been fetched". An empty successful response must not reopen the
	// conservative built-in fallback models.
	CatalogKnown bool
	Models       []GrokModelRoute
	Capabilities []GrokProtocolCapability
	ObservedAt   time.Time
	ExpiresAt    time.Time
}

// GrokResolvedRoute 是执行器最终选择的原生上游协议与采样 Base URL。
type GrokResolvedRoute struct {
	ModelID      string
	BaseURL      string
	Protocol     GrokProtocol
	ExtraHeaders map[string]string
	// Native 表示此次选择来自新鲜的同协议成功探针；false 表示按目录 backend
	// 转换。目录 backend 本身依然是上游原生协议，只是不同于下游入站协议。
	Native bool
}

func cloneGrokHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneGrokModelRoute(in GrokModelRoute) GrokModelRoute {
	in.ExtraHeaders = cloneGrokHeaders(in.ExtraHeaders)
	if len(in.ReasoningEfforts) > 0 {
		in.ReasoningEfforts = append([]string(nil), in.ReasoningEfforts...)
	}
	if in.SupportedInAPI != nil {
		value := *in.SupportedInAPI
		in.SupportedInAPI = &value
	}
	return in
}

func cloneGrokRoutingState(in GrokRoutingState) *GrokRoutingState {
	out := in
	out.Models = make([]GrokModelRoute, len(in.Models))
	for i := range in.Models {
		out.Models[i] = cloneGrokModelRoute(in.Models[i])
	}
	out.Capabilities = append([]GrokProtocolCapability(nil), in.Capabilities...)
	return &out
}

// SetGrokRoutingState 原子替换运行时目录；调用方后续修改 slice/map 不影响账号。
func (a *Account) SetGrokRoutingState(state GrokRoutingState) {
	if a == nil {
		return
	}
	if len(state.Models) > 0 {
		state.CatalogKnown = true
	}
	copyState := cloneGrokRoutingState(state)
	a.mu.Lock()
	a.grokRouting = copyState
	a.mu.Unlock()
}

// ClearGrokRoutingState 在凭据 generation 改变或账号身份切换时立即失效旧路由。
func (a *Account) ClearGrokRoutingState() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.grokRouting = nil
	a.mu.Unlock()
}

// GetGrokRoutingState 返回与内部存储完全分离的副本。
func (a *Account) GetGrokRoutingState() (GrokRoutingState, bool) {
	if a == nil {
		return GrokRoutingState{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.grokRouting == nil {
		return GrokRoutingState{}, false
	}
	return *cloneGrokRoutingState(*a.grokRouting), true
}

func grokCapabilityFresh(cap GrokProtocolCapability, now time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(cap.Status), GrokCapabilityOK) {
		return false
	}
	if cap.ExpiresAt.IsZero() {
		return false
	}
	return now.Before(cap.ExpiresAt)
}

func grokCatalogRoutable(state *GrokRoutingState, now time.Time) bool {
	if state == nil || state.ObservedAt.IsZero() {
		// Preserve compatibility for caller-constructed routing states which do
		// not carry persisted catalog timestamps. Persistent catalog snapshots
		// always set ObservedAt and therefore use the bounded deadline below.
		return state != nil && state.ExpiresAt.IsZero()
	}
	return now.Before(state.ObservedAt.Add(grokCatalogStaleIfError))
}

// GetGrokModelRoute 以目录 apiBackend 为上游协议。新鲜同协议探针只决定
// Native 直通，不能把协议改成和目录不一致的另一条口。
// 官方 grok-4.6 目录是 responses；Messages 探针只用 user:hi 测通，
// 若据此把 Claude Code 的 Anthropic 体打到 /v1/messages，上游会 400
// invalid-argument "Invalid message role"。
// 找不到具体模型时返回 false，调用者可按凭据类型使用保守默认目录。
func (a *Account) GetGrokModelRoute(model string, inbound GrokProtocol, now time.Time) (GrokResolvedRoute, bool) {
	if a == nil {
		return GrokResolvedRoute{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return GrokResolvedRoute{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.grokRouting == nil {
		return GrokResolvedRoute{}, false
	}
	if a.CredentialGeneration > 0 && a.grokRouting.CredentialGeneration > 0 &&
		a.grokRouting.CredentialGeneration != a.CredentialGeneration {
		return GrokResolvedRoute{}, false
	}
	// ExpiresAt is the normal refresh deadline; stale routing has one and only
	// one deadline, measured from the last successful catalog observation.
	if !grokCatalogRoutable(a.grokRouting, now) {
		return GrokResolvedRoute{}, false
	}
	var selected *GrokModelRoute
	for i := range a.grokRouting.Models {
		if strings.EqualFold(strings.TrimSpace(a.grokRouting.Models[i].ModelID), model) {
			copyModel := cloneGrokModelRoute(a.grokRouting.Models[i])
			selected = &copyModel
			break
		}
	}
	if selected == nil {
		return GrokResolvedRoute{}, false
	}
	// Catalog visibility is an authorization boundary, not merely a presentation
	// concern. A caller can submit a model ID without first consulting /v1/models,
	// so enforce the same rules again on the dispatch path.
	authKind := a.GrokAuthKindLocked()
	if selected.Hidden || (authKind == GrokAuthKindAPIKey && selected.SupportedInAPI != nil && !*selected.SupportedInAPI) {
		return GrokResolvedRoute{}, false
	}
	protocol := NormalizeGrokProtocol(string(selected.APIBackend))
	if protocol == "" {
		// Unknown/missing extended metadata follows the Grok Build client fallback.
		protocol = GrokProtocolChatCompletions
	}
	baseURL := strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	if authKind == GrokAuthKindAPIKey && strings.TrimSpace(selected.APIBaseURL) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(selected.APIBaseURL), "/")
	}
	if baseURL == "" {
		baseURL = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
		if baseURL == "" {
			if authKind == GrokAuthKindAPIKey {
				baseURL = GrokDefaultAPIBaseURL
			} else {
				baseURL = GrokDefaultChatProxyBaseURL
			}
		}
	}
	native := false
	if inbound = NormalizeGrokProtocol(string(inbound)); inbound != "" && inbound == protocol {
		for _, capability := range a.grokRouting.Capabilities {
			if strings.EqualFold(strings.TrimSpace(capability.ModelID), model) &&
				capability.Protocol == inbound && grokCapabilityFresh(capability, now) &&
				strings.EqualFold(strings.TrimRight(strings.TrimSpace(capability.Origin), "/"), baseURL) {
				native = true
				break
			}
		}
	}
	return GrokResolvedRoute{
		ModelID: model, BaseURL: baseURL, Protocol: protocol,
		ExtraHeaders: cloneGrokHeaders(selected.ExtraHeaders), Native: native,
	}, true
}

// GrokAuthKindLocked 与 GrokAuthKind 等价，但要求调用方已持有 a.mu。
func (a *Account) GrokAuthKindLocked() string {
	if a == nil || !a.isGrokAPILocked() {
		return ""
	}
	if strings.TrimSpace(a.APIKey) != "" {
		return GrokAuthKindAPIKey
	}
	return GrokAuthKindOAuth
}

// GrokReasoningMenu 返回该账号目录为 model 公布的线协议档位。
// 没有菜单、目录过期或模型不在目录里时返回 nil，请求侧退回版本启发式。
func (a *Account) GrokReasoningMenu(model string) []string {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return nil
	}
	for _, item := range a.GrokCatalogModels() {
		if strings.EqualFold(strings.TrimSpace(item.ModelID), model) {
			return NormalizeGrokReasoningMenu(item.ReasoningEfforts)
		}
	}
	return nil
}

// GrokCatalogModels 返回富目录内全部模型（含 hidden，供管理端诊断）。
func (a *Account) GrokCatalogModels() []GrokModelRoute {
	state, ok := a.GetGrokRoutingState()
	if !ok || (a.GetCredentialGeneration() > 0 && state.CredentialGeneration > 0 &&
		state.CredentialGeneration != a.GetCredentialGeneration()) ||
		!grokCatalogRoutable(&state, time.Now()) {
		return nil
	}
	return state.Models
}

// HasGrokModelCatalog reports whether the account has an authoritative
// catalog snapshot, including a successful snapshot containing zero models.
func (a *Account) HasGrokModelCatalog() bool {
	state, ok := a.GetGrokRoutingState()
	if !ok || !state.CatalogKnown {
		return false
	}
	generation := a.GetCredentialGeneration()
	return (generation <= 0 || state.CredentialGeneration <= 0 || state.CredentialGeneration == generation) &&
		grokCatalogRoutable(&state, time.Now())
}
