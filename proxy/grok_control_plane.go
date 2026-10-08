package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type GrokControlPlaneFactKind string

const (
	GrokControlPlaneUser      GrokControlPlaneFactKind = "user"
	GrokControlPlaneSettings  GrokControlPlaneFactKind = "settings"
	GrokControlPlaneBilling   GrokControlPlaneFactKind = "billing"
	GrokControlPlaneAutoTopup GrokControlPlaneFactKind = "auto_topup"
)

type GrokControlPlaneFactResult struct {
	Kind        GrokControlPlaneFactKind `json:"kind"`
	StatusCode  int                      `json:"status_code"`
	Body        []byte                   `json:"body,omitempty"`
	ObservedAt  time.Time                `json:"observed_at"`
	HTTPETag    string                   `json:"http_etag,omitempty"`
	NotModified bool                     `json:"not_modified,omitempty"`
}

func grokControlPlanePath(kind GrokControlPlaneFactKind) (string, bool) {
	switch kind {
	case GrokControlPlaneUser:
		return "/v1/user?include=subscription", true
	case GrokControlPlaneSettings:
		return "/v1/settings", true
	case GrokControlPlaneBilling:
		return "/v1/billing?format=credits", true
	case GrokControlPlaneAutoTopup:
		return "/v1/auto-topup-rule", true
	default:
		return "", false
	}
}

// applyGrokControlPlaneHeaders mirrors the endpoint-specific first-party
// request shapes. Control-plane GETs deliberately do not inherit inference
// tracking, compaction or content headers.
func applyGrokControlPlaneHeaders(req *http.Request, account *auth.Account, bearer string, kind GrokControlPlaneFactKind) {
	if req == nil || account == nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokUserAgent())
	req.Header.Set("x-grok-client-version", grokClientVersion)
	req.Header.Set("x-grok-client-mode", grokClientMode)
	if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
		req.Header.Set("X-XAI-Token-Auth", grokTokenAuth)
		// First-party settings discovery carries the optional account email when
		// the authenticated profile exposes one. Keep it endpoint-scoped: user,
		// billing and auto-topup have different request shapes.
		if kind == GrokControlPlaneSettings {
			account.Mu().RLock()
			email := strings.TrimSpace(account.Email)
			account.Mu().RUnlock()
			if email != "" {
				req.Header.Set("x-email", email)
			}
		}
	}
	if kind == GrokControlPlaneSettings {
		req.Header.Set("x-grok-client-identifier", grokClientIdentifier)
	}
	if kind == GrokControlPlaneBilling || kind == GrokControlPlaneAutoTopup {
		if userID := account.GrokUserID(); userID != "" {
			req.Header.Set("x-userid", userID)
		}
	}
	applyAccountCustomHeaders(req, account)
}

// FetchGrokControlPlaneFact returns every received HTTP response (including
// auth/gate/rate-limit failures) as a result so callers can persist the fact.
// Only transport, size and invalid-JSON failures return an error.
func FetchGrokControlPlaneFact(ctx context.Context, account *auth.Account, proxyURL string, kind GrokControlPlaneFactKind, ifNoneMatch string) (GrokControlPlaneFactResult, error) {
	result := GrokControlPlaneFactResult{Kind: kind, ObservedAt: time.Now()}
	path, ok := grokControlPlanePath(kind)
	if !ok {
		return result, fmt.Errorf("unsupported Grok control-plane fact %q", kind)
	}
	if account == nil {
		return result, fmt.Errorf("Grok account is nil")
	}
	ctx, finish := grokReadContext(ctx, account.ID(), string(kind))
	defer func() { finish(result.StatusCode) }()
	baseURL, bearer := account.GrokCredentials()
	if baseURL == "" || bearer == "" {
		return result, fmt.Errorf("Grok account has no usable credential")
	}
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, err
	}
	applyGrokControlPlaneHeaders(req, account, bearer, kind)
	if ifNoneMatch = strings.TrimSpace(ifNoneMatch); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		return result, fmt.Errorf("request Grok %s failed: %w", kind, err)
	}
	defer resp.Body.Close()
	decodeGrokResponseEncoding(resp)
	result.StatusCode = resp.StatusCode
	result.HTTPETag = strings.TrimSpace(resp.Header.Get("ETag"))
	if resp.StatusCode == http.StatusNotModified {
		result.NotModified = true
		return result, nil
	}
	const maxFactBody = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFactBody+1))
	if err != nil {
		return result, fmt.Errorf("read Grok %s failed: %w", kind, err)
	}
	if len(body) > maxFactBody {
		return result, fmt.Errorf("Grok %s response exceeds %d bytes", kind, maxFactBody)
	}
	if resp.StatusCode != http.StatusNoContent && len(strings.TrimSpace(string(body))) > 0 && !gjson.ValidBytes(body) {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return result, fmt.Errorf("Grok %s response is not JSON (status %d)", kind, resp.StatusCode)
		}
		// Error pages produced by an edge/WAF are often HTML or plain text. The
		// status code is still useful control-plane evidence, but retaining the
		// body would risk persisting or surfacing arbitrary provider content.
		// Publish the bounded HTTP observation with an empty body so callers can
		// classify 401/402/403/426/429 without logging or forwarding that page.
		result.Body = nil
		return result, nil
	}
	result.Body = body
	return result, nil
}
