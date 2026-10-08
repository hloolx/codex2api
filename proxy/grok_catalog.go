package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type GrokProtocol = auth.GrokProtocol

const (
	GrokProtocolResponses       = auth.GrokProtocolResponses
	GrokProtocolChatCompletions = auth.GrokProtocolChatCompletions
	GrokProtocolMessages        = auth.GrokProtocolMessages
)

const (
	defaultGrokCatalogContextWindow       int64 = 256000
	defaultGrokCatalogMaxCompletionTokens int64 = 32768
	grokMaxCatalogBody                          = 4 << 20
)

// GrokReasoningEffortOption preserves both bare string and rich object menu entries.
type GrokReasoningEffortOption struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// GrokModelCatalogItem is the lossless-enough control-plane projection used by
// persistence, routing and the admin diagnostics API. Pointer booleans plus
// FieldPresence preserve missing/null/false as distinct observations.
type GrokModelCatalogItem struct {
	ID                      string                      `json:"id"`
	DisplayName             string                      `json:"display_name,omitempty"`
	Description             string                      `json:"description,omitempty"`
	BaseURL                 string                      `json:"base_url,omitempty"`
	APIBaseURL              string                      `json:"api_base_url,omitempty"`
	ContextWindow           int64                       `json:"context_window"`
	MaxCompletionTokens     int64                       `json:"max_completion_tokens"`
	APIBackend              GrokProtocol                `json:"api_backend"`
	ReasoningEffort         string                      `json:"reasoning_effort,omitempty"`
	ReasoningEfforts        []GrokReasoningEffortOption `json:"reasoning_efforts,omitempty"`
	SupportsReasoningEffort *bool                       `json:"supports_reasoning_effort,omitempty"`
	SupportsBackendSearch   *bool                       `json:"supports_backend_search,omitempty"`
	StreamToolCalls         *bool                       `json:"stream_tool_calls,omitempty"`
	SupportedInAPI          *bool                       `json:"supported_in_api,omitempty"`
	Hidden                  *bool                       `json:"hidden,omitempty"`
	ExtraHeaders            http.Header                 `json:"extra_headers,omitempty"`
	// FieldPresence values are "missing", "null" or "value". It is explicit
	// rather than relying on omitempty so persisted facts do not collapse false/0.
	FieldPresence map[string]string `json:"field_presence,omitempty"`
	FirstSeenAt   time.Time         `json:"first_seen_at,omitempty"`
}

type GrokModelCatalog struct {
	Models         []GrokModelCatalogItem `json:"models"`
	HTTPETag       string                 `json:"http_etag,omitempty"`
	ModelsETagHint string                 `json:"models_etag_hint,omitempty"`
	NotModified    bool                   `json:"not_modified,omitempty"`
	ObservedAt     time.Time              `json:"observed_at"`
	StatusCode     int                    `json:"status_code"`
}

// GrokHTTPError intentionally retains only status and a bounded structured
// message. The complete upstream body may include identities or provider detail
// and must not end up in logs or admin responses.
type GrokHTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *GrokHTTPError) Error() string {
	if e == nil {
		return "Grok upstream error"
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	if message == "" {
		message = "upstream request failed"
	}
	return fmt.Sprintf("Grok 上游返回 %d: %s", e.StatusCode, message)
}

func AsGrokHTTPError(err error, target **GrokHTTPError) bool {
	return errors.As(err, target)
}

func catalogField(item gjson.Result, names ...string) (gjson.Result, string) {
	for _, name := range names {
		value := item.Get(name)
		if value.Exists() {
			if value.Type == gjson.Null {
				return value, "null"
			}
			return value, "value"
		}
	}
	return gjson.Result{}, "missing"
}

func catalogString(item gjson.Result, names ...string) (string, string) {
	value, presence := catalogField(item, names...)
	if presence != "value" {
		return "", presence
	}
	return strings.TrimSpace(value.String()), presence
}

func catalogInt64(item gjson.Result, fallback int64, names ...string) (int64, string) {
	value, presence := catalogField(item, names...)
	if presence != "value" {
		return fallback, presence
	}
	if value.Type == gjson.Number {
		return value.Int(), presence
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value.String()), 10, 64)
	if err != nil {
		return fallback, presence
	}
	return parsed, presence
}

func catalogBool(item gjson.Result, names ...string) (*bool, string) {
	value, presence := catalogField(item, names...)
	if presence != "value" {
		return nil, presence
	}
	var parsed bool
	switch value.Type {
	case gjson.True:
		parsed = true
	case gjson.False:
		parsed = false
	case gjson.String:
		v, err := strconv.ParseBool(strings.TrimSpace(value.String()))
		if err != nil {
			return nil, presence
		}
		parsed = v
	default:
		return nil, presence
	}
	return &parsed, presence
}

func normalizeGrokCatalogBackend(raw string) GrokProtocol {
	if protocol := auth.NormalizeGrokProtocol(raw); protocol != "" {
		return protocol
	}
	// Grok Build's client fallback for absent/unknown extended metadata.
	return GrokProtocolChatCompletions
}

func parseGrokReasoningEfforts(value gjson.Result) []GrokReasoningEffortOption {
	if !value.IsArray() {
		return nil
	}
	var result []GrokReasoningEffortOption
	value.ForEach(func(_, entry gjson.Result) bool {
		option := GrokReasoningEffortOption{}
		if entry.Type == gjson.String {
			option.ID = strings.TrimSpace(entry.String())
			option.Label = option.ID
		} else if entry.IsObject() {
			// grok-build 的线协议档位在 value 上；id 只是展示键，两者不一致时以 value 为准。
			option.ID = strings.TrimSpace(entry.Get("value").String())
			if option.ID == "" {
				option.ID = strings.TrimSpace(entry.Get("id").String())
			}
			option.Label = strings.TrimSpace(entry.Get("label").String())
			option.Description = strings.TrimSpace(entry.Get("description").String())
		}
		if option.ID != "" {
			result = append(result, option)
		}
		return true
	})
	return result
}

var blockedGrokModelHeaders = map[string]struct{}{
	"authorization": {}, "proxy-authorization": {}, "cookie": {}, "set-cookie": {},
	"x-api-key": {}, "x-xai-token-auth": {}, "x-authenticateresponse": {},
	"x-userid": {}, "x-grok-user-id": {}, "x-grok-agent-id": {},
	"x-grok-session-id": {}, "x-grok-conv-id": {}, "x-grok-conv-group-id": {}, "x-grok-req-id": {},
	"host": {}, "content-length": {}, "transfer-encoding": {}, "connection": {},
	"upgrade": {}, "accept-encoding": {},
}

func sanitizeGrokModelExtraHeaders(value gjson.Result) http.Header {
	if !value.IsObject() {
		return nil
	}
	headers := make(http.Header)
	value.ForEach(func(key, raw gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || strings.ContainsAny(name, "\r\n") {
			return true
		}
		if _, blocked := blockedGrokModelHeaders[strings.ToLower(name)]; blocked {
			return true
		}
		if raw.Type == gjson.String {
			value := strings.TrimSpace(raw.String())
			if value != "" && !strings.ContainsAny(value, "\r\n") {
				headers.Set(canonical, value)
			}
		}
		return true
	})
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func parseGrokCatalogItem(item gjson.Result) (GrokModelCatalogItem, bool) {
	if item.Type == gjson.String {
		id := strings.TrimSpace(item.String())
		if id == "" {
			return GrokModelCatalogItem{}, false
		}
		return GrokModelCatalogItem{
			ID: id, ContextWindow: defaultGrokCatalogContextWindow,
			MaxCompletionTokens: defaultGrokCatalogMaxCompletionTokens,
			APIBackend:          GrokProtocolChatCompletions,
			FieldPresence:       map[string]string{"id": "value", "context_window": "missing", "max_completion_tokens": "missing", "api_backend": "missing"},
		}, true
	}
	if !item.IsObject() {
		return GrokModelCatalogItem{}, false
	}
	presence := make(map[string]string)
	id, idPresence := catalogString(item, "model", "modelId", "model_id", "id", "_meta.model", "_meta.modelId", "_meta.model_id")
	presence["id"] = idPresence
	if id == "" {
		return GrokModelCatalogItem{}, false
	}
	displayName, namePresence := catalogString(item, "name", "displayName", "display_name", "_meta.name")
	presence["display_name"] = namePresence
	description, descriptionPresence := catalogString(item, "description", "_meta.description")
	presence["description"] = descriptionPresence
	baseURL, basePresence := catalogString(item, "baseUrl", "base_url", "_meta.baseUrl", "_meta.base_url")
	presence["base_url"] = basePresence
	apiBaseURL, apiBasePresence := catalogString(item, "apiBaseUrl", "api_base_url", "_meta.apiBaseUrl", "_meta.api_base_url")
	presence["api_base_url"] = apiBasePresence
	contextWindow, contextPresence := catalogInt64(item, defaultGrokCatalogContextWindow, "contextWindow", "context_window", "_meta.totalContextTokens", "_meta.total_context_tokens")
	presence["context_window"] = contextPresence
	maxTokens, maxPresence := catalogInt64(item, defaultGrokCatalogMaxCompletionTokens, "maxCompletionTokens", "max_completion_tokens", "_meta.maxCompletionTokens", "_meta.max_completion_tokens")
	presence["max_completion_tokens"] = maxPresence
	backend, backendPresence := catalogString(item, "apiBackend", "api_backend", "_meta.apiBackend", "_meta.api_backend")
	presence["api_backend"] = backendPresence
	reasoningEffort, reasoningPresence := catalogString(item, "reasoningEffort", "reasoning_effort", "_meta.reasoningEffort", "_meta.reasoning_effort")
	presence["reasoning_effort"] = reasoningPresence
	reasoningEffortsValue, reasoningEffortsPresence := catalogField(item, "reasoningEfforts", "reasoning_efforts", "_meta.reasoningEfforts", "_meta.reasoning_efforts")
	presence["reasoning_efforts"] = reasoningEffortsPresence
	supportsReasoning, supportsReasoningPresence := catalogBool(item, "supportsReasoningEffort", "supports_reasoning_effort", "_meta.supportsReasoningEffort", "_meta.supports_reasoning_effort")
	presence["supports_reasoning_effort"] = supportsReasoningPresence
	supportsSearch, supportsSearchPresence := catalogBool(item, "supportsBackendSearch", "supports_backend_search", "_meta.supportsBackendSearch", "_meta.supports_backend_search")
	presence["supports_backend_search"] = supportsSearchPresence
	streamTools, streamToolsPresence := catalogBool(item, "streamToolCalls", "stream_tool_calls", "_meta.streamToolCalls", "_meta.stream_tool_calls")
	presence["stream_tool_calls"] = streamToolsPresence
	supportedAPI, supportedPresence := catalogBool(item, "supportedInApi", "supported_in_api", "_meta.supportedInApi", "_meta.supported_in_api")
	presence["supported_in_api"] = supportedPresence
	hidden, hiddenPresence := catalogBool(item, "hidden", "_meta.hidden")
	presence["hidden"] = hiddenPresence
	extraHeadersValue, extraHeadersPresence := catalogField(item, "extraHeaders", "extra_headers", "_meta.extraHeaders", "_meta.extra_headers")
	presence["extra_headers"] = extraHeadersPresence
	return GrokModelCatalogItem{
		ID: id, DisplayName: displayName, Description: description,
		BaseURL: baseURL, APIBaseURL: apiBaseURL, ContextWindow: contextWindow,
		MaxCompletionTokens: maxTokens, APIBackend: normalizeGrokCatalogBackend(backend),
		ReasoningEffort: reasoningEffort, ReasoningEfforts: parseGrokReasoningEfforts(reasoningEffortsValue),
		SupportsReasoningEffort: supportsReasoning, SupportsBackendSearch: supportsSearch,
		StreamToolCalls: streamTools, SupportedInAPI: supportedAPI, Hidden: hidden,
		ExtraHeaders: sanitizeGrokModelExtraHeaders(extraHeadersValue), FieldPresence: presence,
	}, true
}

// ParseGrokModelCatalog accepts data/models arrays and a bare top-level array.
// It retains hidden/unsupported entries; visibility is an account-auth decision
// performed separately by VisibleGrokModelIDs.
func ParseGrokModelCatalog(body []byte, _ bool) ([]GrokModelCatalogItem, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("Grok 模型目录不是有效 JSON")
	}
	root := gjson.ParseBytes(body)
	containers := []gjson.Result{root.Get("data"), root.Get("models")}
	if root.IsArray() {
		containers = []gjson.Result{root}
	}
	foundArray := false
	seen := make(map[string]struct{})
	models := make([]GrokModelCatalogItem, 0)
	for _, container := range containers {
		if !container.IsArray() {
			continue
		}
		foundArray = true
		container.ForEach(func(_, raw gjson.Result) bool {
			item, ok := parseGrokCatalogItem(raw)
			if !ok {
				return true
			}
			key := strings.ToLower(item.ID)
			if _, exists := seen[key]; exists {
				return true
			}
			seen[key] = struct{}{}
			models = append(models, item)
			return true
		})
	}
	if !foundArray {
		return nil, fmt.Errorf("Grok 模型目录缺少 data/models 数组")
	}
	return models, nil
}

func VisibleGrokModelIDs(models []GrokModelCatalogItem, authKind string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(models))
	for _, model := range models {
		if model.Hidden != nil && *model.Hidden {
			continue
		}
		if authKind == auth.GrokAuthKindAPIKey && model.SupportedInAPI != nil && !*model.SupportedInAPI {
			continue
		}
		id := strings.TrimSpace(model.ID)
		key := strings.ToLower(id)
		if id == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, id)
	}
	sort.SliceStable(result, func(i, j int) bool { return strings.ToLower(result[i]) < strings.ToLower(result[j]) })
	return result
}

func safeGrokHTTPError(status int, body []byte) *GrokHTTPError {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(body, "code").String())
	}
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(body, "message").String())
	}
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
	if len(message) > 240 {
		message = message[:240] + "…"
	}
	return &GrokHTTPError{StatusCode: status, Code: code, Message: message}
}

// FetchGrokModelCatalog performs a conditional GET. HTTP ETag and the opaque
// x-models-etag invalidation hint are kept in distinct fields.
func FetchGrokModelCatalog(ctx context.Context, account *auth.Account, proxyURL, ifNoneMatch string) (GrokModelCatalog, error) {
	result := GrokModelCatalog{ObservedAt: time.Now()}
	if account == nil {
		return result, fmt.Errorf("Grok 账号为空")
	}
	baseURL, bearer := account.GrokCredentials()
	if baseURL == "" || bearer == "" {
		return result, fmt.Errorf("Grok 账号缺少可用凭据")
	}
	ctx, finish := grokReadContext(ctx, account.ID(), "models")
	defer func() { finish(result.StatusCode) }()
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, err
	}
	// Model discovery has its own session-auth shape. In particular it carries
	// x-userid/x-email for OAuth, but not inference tracking or compaction heads.
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokUserAgent())
	req.Header.Set("x-grok-client-version", grokClientVersion)
	req.Header.Set("x-grok-client-mode", grokClientMode)
	if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
		req.Header.Set("X-XAI-Token-Auth", grokTokenAuth)
		if userID := account.GrokUserID(); userID != "" {
			req.Header.Set("x-userid", userID)
		}
		account.Mu().RLock()
		email := strings.TrimSpace(account.Email)
		account.Mu().RUnlock()
		if email != "" {
			req.Header.Set("x-email", email)
		}
	}
	applyAccountCustomHeaders(req, account)
	if ifNoneMatch = strings.TrimSpace(ifNoneMatch); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		return result, fmt.Errorf("请求 Grok 模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	decodeGrokResponseEncoding(resp)
	result.StatusCode = resp.StatusCode
	result.HTTPETag = strings.TrimSpace(resp.Header.Get("ETag"))
	result.ModelsETagHint = strings.TrimSpace(resp.Header.Get("x-models-etag"))
	if resp.StatusCode == http.StatusNotModified {
		result.NotModified = true
		return result, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, grokMaxCatalogBody+1))
	if err != nil {
		return result, fmt.Errorf("读取 Grok 模型目录失败: %w", err)
	}
	if len(body) > grokMaxCatalogBody {
		return result, fmt.Errorf("Grok 模型目录超过 %d bytes", grokMaxCatalogBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, safeGrokHTTPError(resp.StatusCode, body)
	}
	models, err := ParseGrokModelCatalog(body, account.GrokAuthKind() == auth.GrokAuthKindAPIKey)
	if err != nil {
		return result, err
	}
	result.Models = models
	return result, nil
}

func grokCatalogToRoutingModels(models []GrokModelCatalogItem) []auth.GrokModelRoute {
	result := make([]auth.GrokModelRoute, 0, len(models))
	for _, model := range models {
		extra := make(map[string]string, len(model.ExtraHeaders))
		for key, values := range model.ExtraHeaders {
			if len(values) > 0 {
				extra[key] = values[len(values)-1]
			}
		}
		hidden := model.Hidden != nil && *model.Hidden
		efforts := make([]string, 0, len(model.ReasoningEfforts))
		for _, option := range model.ReasoningEfforts {
			if option.ID != "" {
				efforts = append(efforts, option.ID)
			}
		}
		result = append(result, auth.GrokModelRoute{
			ModelID: model.ID, BaseURL: model.BaseURL, APIBaseURL: model.APIBaseURL,
			APIBackend: model.APIBackend, ExtraHeaders: extra, SupportedInAPI: model.SupportedInAPI,
			Hidden: hidden, ContextWindow: model.ContextWindow, MaxCompletionTokens: model.MaxCompletionTokens,
			SupportsReasoningEffort: model.SupportsReasoningEffort != nil && *model.SupportsReasoningEffort,
			ReasoningEffort:         model.ReasoningEffort,
			ReasoningEfforts:        auth.NormalizeGrokReasoningMenu(efforts),
			SupportsBackendSearch:   model.SupportsBackendSearch != nil && *model.SupportsBackendSearch,
			StreamToolCalls:         model.StreamToolCalls != nil && *model.StreamToolCalls,
			FirstSeenAt:             model.FirstSeenAt,
		})
	}
	return result
}

// MarshalGrokCatalogItem is a stable helper for database/admin adapters that
// need the exact field-presence metadata without depending on gjson internals.
func MarshalGrokCatalogItem(item GrokModelCatalogItem) ([]byte, error) {
	return json.Marshal(item)
}
