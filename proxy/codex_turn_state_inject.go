package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Credential-level injection runs after model and transport selection. HTTP
// headers and WebSocket frames share the same decision through the context.
// Managed state and probe requests take precedence over manual injection.

// codexTurnStateMetadataKey carries per-turn state on an existing WebSocket.
const codexTurnStateMetadataKey = "x-codex-turn-state"

type codexTurnStateInjectionKey struct{}
type codexClientModelKey struct{}

// WithCodexClientModel retains the original model for matching before aliases.
func WithCodexClientModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, codexClientModelKey{}, model)
}

func codexClientModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexClientModelKey{}).(string)
	return model
}

func withCodexTurnStateInjection(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTurnStateInjectionKey{}, value)
}

// CodexTurnStateInjectionFromContext returns the selected outbound state.
func CodexTurnStateInjectionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(codexTurnStateInjectionKey{}).(string)
	return value
}

// prepareCodexTurnStateInjection copies headers and WebSocket metadata only when
// the account's configured model scope matches and no managed state owns it.
func prepareCodexTurnStateInjection(ctx context.Context, account *auth.Account, requestBody []byte, headers http.Header, websocket bool) (context.Context, []byte, http.Header) {
	if account == nil {
		return ctx, requestBody, headers
	}
	upstreamModel := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	if ctx != nil && ctx.Value(statePoolBypassKey{}) == true {
		return ctx, requestBody, headers
	}
	if provider := ipv6StateProvider.Load(); provider != nil && provider.Applies != nil && provider.Applies(account, upstreamModel) {
		return withCodexTurnStateInjection(ctx, headers.Get(codexTurnStateHeader)), requestBody, headers
	}
	// Resolve already checked the actual business proxy before injection.
	if provider := verifiedStateProvider.Load(); provider != nil && provider.claims != nil {
		state := headers.Get(codexTurnStateHeader)
		if provider.claims(account, upstreamModel, gjson.GetBytes(requestBody, "reasoning.effort").String(), state) {
			return withCodexTurnStateInjection(ctx, state), requestBody, headers
		}
	}
	injected := account.CodexTurnStateInjection(codexClientModelFromContext(ctx), upstreamModel)
	if injected == "" {
		return ctx, requestBody, headers
	}
	ctx = withCodexTurnStateInjection(ctx, injected)
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set(codexTurnStateHeader, injected)
	if websocket {
		// Existing sockets require state in each response.create frame.
		if updated, err := sjson.SetBytes(requestBody, "client_metadata."+codexTurnStateMetadataKey, injected); err == nil {
			requestBody = updated
		}
	}
	return ctx, requestBody, headers
}

// applyCodexTurnStateInjectionHeader applies the decision after custom headers.
func applyCodexTurnStateInjectionHeader(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}
	if value := CodexTurnStateInjectionFromContext(ctx); value != "" {
		headers.Set(codexTurnStateHeader, value)
	}
}

// ApplyCodexTurnStateInjectionHeader exposes final header injection to wsrelay.
func ApplyCodexTurnStateInjectionHeader(ctx context.Context, headers http.Header) {
	applyCodexTurnStateInjectionHeader(ctx, headers)
}

// Oversized observed values are rejected rather than truncated.
const maxObservedCodexTurnStateBytes = 4096

// observedCodexTurnState accepts bounded single-line visible strings.
func observedCodexTurnState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxObservedCodexTurnStateBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

var codexTurnStateFrameNeedles = [][]byte{[]byte("turn-state"), []byte("Turn-State")}

// codexTurnStateFromFrame searches known metadata locations after a cheap
// substring check, avoiding JSON parsing for unrelated WebSocket frames.
func codexTurnStateFromFrame(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	mentioned := false
	for _, needle := range codexTurnStateFrameNeedles {
		if bytes.Contains(payload, needle) {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return ""
	}
	for _, path := range []string{"headers", "response.headers", "response.client_metadata", "client_metadata", "response.metadata", "metadata", "response", ""} {
		object := root
		if path != "" {
			object = root.Get(path)
		}
		if !object.IsObject() {
			continue
		}
		state := ""
		object.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), codexTurnStateHeader) && value.Type == gjson.String {
				state = value.String()
				return false
			}
			return true
		})
		if state = observedCodexTurnState(state); state != "" {
			return state
		}
	}
	return ""
}

// ObserveCodexTurnStateFrame records and returns upstream state in the current trace.
func ObserveCodexTurnStateFrame(ctx context.Context, payload []byte) string {
	state := codexTurnStateFromFrame(payload)
	if state != "" {
		noteUpstreamTurnState(ctx, state)
	}
	return state
}
