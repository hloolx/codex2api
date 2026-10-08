package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type codexRouteKey struct{}
type codexRouteLimitsKey struct{}
type codexRouteSharedKey struct{}
type codexRouteShared struct {
	mu       sync.Mutex
	decision *CodexRouteDecision
}
type codexAttemptKey struct{}

// CodexRouteAttempt describes transport facts without credentials or request text.
type CodexRouteAttempt struct {
	AccountID      int64  `json:"account_id"`
	Upstream       string `json:"upstream"`
	Model          string `json:"model"`
	HTTPStatus     int    `json:"http_status"`
	ReportedStatus int    `json:"reported_status"`
	ErrorCode      string `json:"error_code,omitempty"`
	Source         string `json:"source,omitempty"`
	Reason         string `json:"reason,omitempty"`
	SwitchBlocked  string `json:"switch_blocked,omitempty"`
}

// One decision is shared by ordinary, encrypted, continuation and route retries.
// The account scheduler still owns authorization, accounting and concurrency.
type CodexRouteDecision struct {
	mu                   sync.Mutex
	RequestedModel       string
	EffectiveModel       string
	Policy               string
	CapabilityFilter     string
	AllowedGroupIDs      []int64
	NoAffinityGroupIDs   []int64
	Preferred            string
	Paths                []string
	Attempts             []CodexRouteAttempt
	Remaining            int
	Switched             bool
	Committed            bool
	NoSwitch             bool
	HistoryLockReason    string
	pinnedPath           string
	historyError         *Error
	policyError          *Error
	historyCache         cache.TokenCache
	historyOwner         string
	schedulerSelected    bool
	routeConstrained     bool
	selectionReasons     map[int64][]string
	recordSelectionError func(*Error)
	selectionLogged      bool
	FinalPath            string
	Reason               string
	client               context.Context
	limits               database.APIKeyLimits
}

type codexRouteAttemptState struct {
	decision   *CodexRouteDecision
	account    *auth.Account
	path       string
	model      string
	started    time.Time
	release    func()
	failure    *codexRouteFailure
	inspected  bool
	websocket  bool
	generation int64
}

func codexRouteFromContext(ctx context.Context) *CodexRouteDecision {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(codexRouteKey{}).(*CodexRouteDecision)
	return d
}

func codexAttemptFromContext(ctx context.Context) *codexRouteAttemptState {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(codexAttemptKey{}).(*codexRouteAttemptState)
	return a
}

func newCodexRouteDecision(ctx context.Context, requested, model string, limits database.APIKeyLimits, budget int) *CodexRouteDecision {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := limits.CodexRoutePolicy
	if policy == "" || policy == database.CodexRouteInherit {
		policy = strings.TrimSpace(os.Getenv("CODEX_ROUTE_POLICY"))
	}
	if policy == "" || policy == database.CodexRouteInherit {
		policy = database.CodexRouteNativeOnly
		if basispointsActiveForModel(model) {
			policy = database.CodexRouteBasispointsPrefer
		}
	}
	d := &CodexRouteDecision{routeConstrained: limits.CodexRoutePolicy != "" && limits.CodexRoutePolicy != "inherit" || limits.CodexCapabilityFilter != "" && limits.CodexCapabilityFilter != "any", RequestedModel: requested, EffectiveModel: model, Policy: policy, CapabilityFilter: limits.CodexCapabilityFilter, Remaining: budget, client: ctx, limits: limits}
	switch policy {
	case database.CodexRouteBasispointsModelsOnly:
		// Catalog membership is independent of account health and the BPS switch.
		d.routeConstrained = true
		d.Paths = []string{database.CodexPathNative}
		if basispointsModelAllowed(model) {
			d.Paths = []string{database.CodexPathBasispoints}
		}
	case database.CodexRouteNativeOnly:
		d.Paths = []string{database.CodexPathNative}
	case database.CodexRouteBasispointsOnly:
		d.Paths = []string{database.CodexPathBasispoints}
	case database.CodexRouteNativePrefer:
		d.Paths = []string{database.CodexPathNative, database.CodexPathBasispoints}
	case database.CodexRouteBasispointsPrefer:
		d.Paths = []string{database.CodexPathBasispoints}
		if basispointsNativeFallbackEnabled() {
			d.Paths = append(d.Paths, database.CodexPathNative)
		}
	}
	if len(d.Paths) > 0 {
		d.Preferred = d.Paths[0]
	}
	return d
}

func routeLocalError(code, message string) *Error {
	return &Error{Code: code, Message: message, Type: ErrorTypeInvalidRequest, HTTPStatus: 503}
}

func nativeStateEligible(account *auth.Account, model string) bool {
	f := withNativeRequiredStateFilter(model, nil)
	return f == nil || f(account)
}

func (d *CodexRouteDecision) pathEligible(account *auth.Account, path, model string, body []byte) bool {
	return d.pathIneligibleReason(account, path, model, body) == ""
}

func (d *CodexRouteDecision) pathIneligibleReason(account *auth.Account, path, model string, body []byte) string {
	d.mu.Lock()
	blocked := d.historyError != nil || d.policyError != nil || d.pinnedPath != "" && d.pinnedPath != path
	d.mu.Unlock()
	if blocked {
		return "history_path"
	}
	if d.Policy == database.CodexRouteBasispointsModelsOnly && (path != d.Preferred || basispointsModelAllowed(model) != d.requiresVerifiedBasispoints()) {
		return "model_route_policy"
	}
	if account == nil || account.IsRelayStyle() {
		return "account_identity"
	}
	if err := account.RefreshCodexRoutes(d.client, time.Now()); err != nil {
		return "route_configuration"
	}
	if path == database.CodexPathBasispoints {
		if !basispointsActiveForModel(model) || account.IsCodexAgentIdentity() || account.GetAccessToken() == "" || account.EffectiveAccountID() == "" {
			return "basispoints_unavailable"
		}
		if len(body) > 0 {
			if reason := basispoints.NativeCodexReason(body, basispointsImageHostAvailable()); reason != "" {
				return "requires_native_" + reason
			}
		}
	} else if !nativeStateEligible(account, model) {
		return "native_state"
	}
	s := account.CodexPathSnapshot(path, model, time.Now())
	if s.Health == "unavailable" {
		return "route_configuration"
	}
	if !s.Allowed {
		return "path_disabled"
	}
	if s.Capability == database.CapabilityUnsupported {
		return "capability_unsupported"
	}
	if s.Health == "cooldown" || s.Health == "recovering" {
		return "path_" + s.Health
	}
	if d.requiresVerifiedBasispoints() && !s.ExactModelSupported {
		return "exact_model_unverified"
	}
	supported := func(p string) bool {
		return account.CodexPathSnapshot(p, model, time.Now()).Capability == database.CapabilitySupported
	}
	allowed := false
	switch d.CapabilityFilter {
	case "supported":
		allowed = supported(path)
	case "dual_supported":
		allowed = supported(database.CodexPathNative) && supported(database.CodexPathBasispoints)
	case "codex_supported":
		allowed = supported(database.CodexPathNative)
	case "basispoints_supported":
		allowed = supported(database.CodexPathBasispoints)
	case "", "any":
		allowed = true
	}
	if !allowed {
		return "capability_filter"
	}
	return ""
}

func (d *CodexRouteDecision) eligiblePaths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.FinalPath != "" {
		return []string{d.FinalPath}
	}
	if d.pinnedPath != "" {
		return []string{d.pinnedPath}
	}
	return append([]string(nil), d.Paths...)
}

func (h *Handler) withCodexRouteFilter(c *gin.Context, requested, model string, body []byte, filter auth.AccountFilter) auth.AccountFilter {
	limits := database.APIKeyLimits{}
	row := apiKeyRowFromContext(c)
	if row != nil {
		limits = row.Limits
	}
	budget := 3 + max(0, h.getMaxRetries()) + max(0, h.getMaxRateLimitRetries())
	if h.getMaxRetries() < 0 || h.getMaxRateLimitRetries() < 0 || continuousRetryPolicyForRequest(c).Enabled {
		budget = 8
	}
	budget = min(budget, 32)
	d := newCodexRouteDecision(c.Request.Context(), requested, model, limits, budget)
	d.historyCache = h.cache
	d.historyOwner = responseCacheOwner(requestAPIKeyID(c))
	d.recordSelectionError = func(err *Error) { h.logCodexRouteSelectionError(c, d, err) }
	d.bindHistory(body)
	d.validateModelPolicy(body)
	if row != nil {
		d.AllowedGroupIDs = append([]int64(nil), row.AllowedGroupIDs...)
		d.NoAffinityGroupIDs = append([]int64(nil), row.Limits.NoAffinityGroupIDs...)
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), codexRouteKey{}, d))
	base := filter
	if h.store != nil {
		base = h.store.WithModelCooldownFilter(model, base)
	}
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if base != nil && !base(account) {
			d.rememberSelectionReasons(account.ID(), []string{"model_or_account_filter"})
			return false
		}
		if account.IsRelayStyle() {
			if d.relayViolatesModelPolicy(account, c.Request.URL.Path, body) {
				d.rememberSelectionReasons(account.ID(), []string{"basispoints:relay_forbidden"})
				return false
			}
			// Known direct-upstream state cannot be replayed through an arbitrary relay.
			if d.hasKnownHistoryRoute() {
				d.rememberSelectionReasons(account.ID(), []string{"history_relay"})
				return false
			}
			d.rememberSelectionReasons(account.ID(), nil)
			return true
		}
		reasons := []string{}
		for _, p := range d.eligiblePaths() {
			reason := d.pathIneligibleReason(account, p, model, body)
			if reason == "" {
				d.rememberSelectionReasons(account.ID(), nil)
				return true
			}
			reasons = append(reasons, p+":"+reason)
		}
		d.rememberSelectionReasons(account.ID(), reasons)
		d.mu.Lock()
		d.routeConstrained = true
		d.mu.Unlock()
		return false
	}
}

func (d *CodexRouteDecision) begin(account *auth.Account, path, model string) error {
	if err := d.client.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Remaining <= 0 {
		return routeLocalError("codex_route_budget_exhausted", "Upstream attempt budget exhausted")
	}
	if d.historyError != nil {
		return d.historyError
	}
	if d.policyError != nil {
		return d.policyError
	}
	if d.Policy == database.CodexRouteBasispointsModelsOnly && path != d.Preferred {
		return routeLocalError("codex_route_model_policy_conflict", "The model routing policy does not permit this upstream")
	}
	if d.pinnedPath != "" && d.pinnedPath != path {
		return routeLocalError("codex_route_history_pinned", "History is pinned to its original upstream")
	}
	if d.Switched && d.FinalPath != path {
		return routeLocalError("codex_route_loop_blocked", "An upstream switch has already been attempted")
	}
	d.Remaining--
	d.FinalPath = path
	d.Attempts = append(d.Attempts, CodexRouteAttempt{AccountID: account.ID(), Upstream: path, Model: model})
	return nil
}

func (d *CodexRouteDecision) commit() { d.mu.Lock(); d.Committed = true; d.mu.Unlock() }

func safeCodexSwitchBody(body []byte) ([]byte, error) {
	if err := codexHistoryReplayError(body); err != nil {
		return nil, err
	}
	return append([]byte(nil), body...), nil
}

func codexHistoryReplayError(body []byte) error {
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return routeLocalError("codex_route_history_required", "Complete normalized history is required to switch upstreams")
	}
	pending := map[string]string{}
	seen := map[string]bool{}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if gjsonResultHasEncryptedCompaction(item) || item.Get("type").String() == "item_reference" || item.Get("encrypted_content").String() != "" || hasNestedCodexEncryptedContent(item) {
			return routeLocalError("codex_route_history_incompatible", "Opaque reasoning or compact history cannot be replayed across upstreams; supply complete plaintext history")
		}
		switch kind := item.Get("type").String(); kind {
		case "function_call", "custom_tool_call":
			id := item.Get("call_id").String()
			if id == "" || seen[id] {
				return routeLocalError("codex_route_history_incompatible", "Complete, paired tool history is required to switch upstreams")
			}
			pending[id] = kind
			seen[id] = true
		case "function_call_output", "custom_tool_call_output":
			id := item.Get("call_id").String()
			if pending[id] != strings.TrimSuffix(kind, "_output") {
				return routeLocalError("codex_route_history_incompatible", "A tool result without its matching call cannot be replayed across upstreams")
			}
			delete(pending, id)
		}
	}
	if len(pending) > 0 {
		return routeLocalError("codex_route_history_incompatible", "Pending tool calls cannot be replayed across upstreams")
	}
	return nil
}

type codexNativeExecutor func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header) (*http.Response, error)

func executeCodexRoute(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *DeviceProfileConfig, headers http.Header, compact bool, native codexNativeExecutor) (*http.Response, error) {
	if account == nil || account.IsRelayStyle() {
		return nil, ErrNoAvailableAccount()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d := codexRouteFromContext(ctx)
	model := gjson.GetBytes(body, "model").String()
	if !compact && !responsesBodyRequestsImageGeneration(body) && !isCodexDetectorRequest(ctx) {
		RecordObservedInstructions(body, headers)
		body = ApplyPayloadRulesToBody(body, model, headers, PayloadRuleIdentityFromContext(ctx))
		body = sanitizeServiceTierForUpstream(body)
	}
	if d == nil {
		limits, _ := ctx.Value(codexRouteLimitsKey{}).(database.APIKeyLimits)
		if shared, ok := ctx.Value(codexRouteSharedKey{}).(*codexRouteShared); ok {
			shared.mu.Lock()
			if shared.decision == nil {
				shared.decision = newCodexRouteDecision(ctx, model, model, limits, 8)
			}
			d = shared.decision
			shared.mu.Unlock()
		} else {
			d = newCodexRouteDecision(ctx, model, model, limits, 3)
		}
		ctx = context.WithValue(ctx, codexRouteKey{}, d)
	}
	d.bindHistory(body)
	d.validateModelPolicy(body)
	if err := codexRouteBudgetError(ctx); err != nil {
		return nil, err
	}
	if d.Preferred == database.CodexPathBasispoints && (account.IsCodexAgentIdentity() || account.GetAccessToken() == "" || account.EffectiveAccountID() == "") {
		return nil, ErrBadRequest("Basispoints requires a ChatGPT OAuth access token and account ID")
	}
	paths := d.Paths
	selected := ""
	for _, path := range d.eligiblePaths() {
		if d.pathEligible(account, path, model, body) {
			selected = path
			break
		}
	}
	if selected == "" && d.Preferred == database.CodexPathBasispoints && d.pathEligible(account, database.CodexPathBasispoints, model, nil) {
		// Preserve the bridge's actionable local protocol/hosting diagnostics.
		if _, _, imageErr := rewriteBasispointsImages(ctx, body); imageErr != nil {
			return nil, newBasispointsPreparationError(imageErr)
		}
		if _, _, prepareErr := basispoints.Prepare(body, "route-validation", &basispoints.ReplayCache{}); prepareErr != nil {
			return nil, newBasispointsPreparationError(prepareErr)
		}
	}
	if selected == "" {
		return nil, routeLocalError("codex_route_no_eligible_path", "No permitted upstream path satisfies capability, protocol and exact-model State requirements")
	}
	canonical := append([]byte(nil), body...)
	nativeReason := ""
	if selected == database.CodexPathNative && d.Preferred == database.CodexPathBasispoints {
		nativeReason = basispoints.NativeCodexReason(canonical, basispointsImageHostAvailable())
	}
	// Durable image jobs must not retain a replay copy while the native executor
	// waits upstream. Keep the canonical body on disk and reload under its memory
	// gate only for dispatch or a permitted fallback decision.
	var canonicalFile *os.File
	if pipeline := pipelineFromContext(ctx); pipeline != nil {
		var err error
		canonicalFile, err = pipeline.spool(canonical)
		if err != nil {
			return nil, ErrInternalError("cannot spool upstream route request", err)
		}
		defer removePipelineFile(canonicalFile)
		canonical = nil
	}
	replayBody := func() ([]byte, error) {
		if canonicalFile == nil {
			return append([]byte(nil), canonical...), nil
		}
		if err := pipelineFromContext(ctx).Acquire(ctx); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(canonicalFile.Name())
		if err != nil {
			return nil, ErrInternalError("cannot read spooled upstream route request", err)
		}
		return data, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := d.begin(account, selected, model); err != nil {
			return nil, err
		}
		release, ok := account.BeginCodexPath(selected, model, time.Now())
		if !ok {
			return nil, routeLocalError("codex_route_recovery_busy", "The upstream path is cooling down or already being checked")
		}
		a := &codexRouteAttemptState{decision: d, account: account, path: selected, model: model, started: time.Now(), release: release, generation: account.GetCredentialGeneration()}
		attemptCtx := context.WithValue(ctx, codexAttemptKey{}, a)
		attemptBody, err := replayBody()
		if err != nil {
			release()
			return nil, err
		}
		attemptHeaders := headers.Clone()
		var resp *http.Response
		if selected == database.CodexPathBasispoints {
			var images basispointsImageRewrite
			attemptBody, images, err = rewriteBasispointsImages(attemptCtx, attemptBody)
			if images.converted+images.reused > 0 {
				log.Printf("[Basispoints] stage=images result=hosted converted=%d reused=%d account=%d", images.converted, images.reused, account.ID())
			}
			if err != nil && !d.NoSwitch && !d.Switched && d.Remaining > 0 {
				fallbackBody, replayErr := replayBody()
				if replayErr != nil {
					release()
					return nil, replayErr
				}
				for _, path := range paths {
					if path == database.CodexPathNative && d.pathEligible(account, path, model, fallbackBody) {
						release()
						selected, nativeReason = path, basispoints.RouteImageInput
						d.mu.Lock()
						d.Switched = true
						d.FinalPath = path
						d.Reason = nativeReason
						d.mu.Unlock()
						break
					}
				}
				if selected == database.CodexPathNative {
					continue
				}
			}
			if err == nil {
				if compact {
					resp, err = executeBasispointsCompactRequest(attemptCtx, account, attemptBody, sessionID, proxyURL, apiKey, attemptHeaders)
				} else {
					resp, err = executeBasispointsRequest(attemptCtx, account, attemptBody, sessionID, proxyURL, apiKey, attemptHeaders)
				}
			} else {
				err = newBasispointsPreparationError(err)
			}
		} else {
			attemptCtx = context.WithValue(attemptCtx, statePoolBypassKey{}, false)
			attemptBody = prepareNativeCodexRouteBody(attemptBody, d.limits, compact)
			resp, err = native(attemptCtx, account, attemptBody, sessionID, proxyURL, apiKey, device, attemptHeaders)
			if resp != nil {
				if resp.Header == nil {
					resp.Header = make(http.Header)
				}
				resp.Header.Set("X-Codex2API-Upstream", database.CodexPathNative)
				inspectCodexRouteResponse(attemptCtx, resp)
				markBasispointsNativeRoute(resp, nativeReason)
			}
		}
		if err != nil {
			release()
			return resp, err
		}
		if resp == nil {
			release()
			return nil, routeLocalError("codex_route_empty_response", "Upstream returned no response")
		}
		failure := a.failure
		if failure != nil {
			a.recordFailure(*failure)
			alternate := ""
			d.mu.Lock()
			canSwitch := failure.Switch && !d.NoSwitch && !d.Switched && !d.Committed && d.Remaining > 0
			d.mu.Unlock()
			if canSwitch && d.client.Err() == nil && ctx.Err() == nil {
				fallbackBody, replayErr := replayBody()
				if replayErr != nil {
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					release()
					return nil, replayErr
				}
				for _, p := range paths {
					if p != selected && d.pathEligible(account, p, model, fallbackBody) {
						alternate = p
						break
					}
				}
			}
			if alternate != "" {
				if errClose := resp.Body.Close(); errClose != nil {
					log.Printf("[CodexRoute] response close failed account=%d", account.ID())
				}
				release()
				d.mu.Lock()
				d.Switched = true
				d.FinalPath = alternate
				d.Reason = failure.Category
				d.mu.Unlock()
				log.Printf("[CodexRoute] preferred=%s final=%s reason=%s account=%d model=%s", d.Preferred, alternate, failure.Category, account.ID(), model)
				selected = alternate
				continue
			}
		}
		resp.Body = observeCodexRouteBody(resp.Body, a, resp.Header.Get("Content-Type"))
		return resp, nil
	}
}

func (a *codexRouteAttemptState) recordFailure(f codexRouteFailure) {
	d := a.decision
	d.mu.Lock()
	if f.Category == "explicit_safety_policy" {
		d.NoSwitch = true
	}
	if len(d.Attempts) > 0 {
		last := &d.Attempts[len(d.Attempts)-1]
		last.HTTPStatus = f.HTTPStatus
		last.ReportedStatus = f.ReportedStatus
		last.Source = f.Source
		last.ErrorCode = f.Code
		last.Reason = f.Category
		if f.Switch && d.NoSwitch {
			last.SwitchBlocked = d.HistoryLockReason
		}
	}
	blocked := d.HistoryLockReason
	d.mu.Unlock()
	log.Printf("[CodexRoute] preferred=%s path=%s account=%d http_status=%d reported_status=%d source=%s code=%s reason=%s switch_blocked=%s", d.Preferred, a.path, a.account.ID(), f.HTTPStatus, f.ReportedStatus, f.Source, f.Code, f.Category, blocked)
	if f.Category == "upstream_access" {
		a.account.SetCodexPathCooldown(a.path, a.model, f.Category, a.started, time.Now().Add(30*time.Second))
	}
	if f.Category == "ambiguous_usage_rejection" {
		a.account.NoteCodexPathUsageRejection(a.path, a.model, a.started, time.Now())
	}
	if f.Category == "model_access" {
		a.account.ObserveCodexPath(d.client, database.CodexCapability{Upstream: a.path, Model: a.model, Capability: database.CapabilityUnsupported, Source: f.Source, Reason: f.Code, ObservedAt: a.started.UnixNano(), CredentialGeneration: a.generation})
	}
}

// Keep the native executor entry points explicit; route fallback never recurses.
func ExecuteRequest(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
	native := func(ctx context.Context, a *auth.Account, b []byte, s, p, k string, d *DeviceProfileConfig, h http.Header) (*http.Response, error) {
		return executeNativeCodexRequest(ctx, a, b, s, p, k, d, h, useWebsocket...)
	}
	return executeCodexRoute(ctx, account, body, sessionID, proxyURL, apiKey, device, headers, false, native)
}

func ExecuteCompactRequest(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *DeviceProfileConfig, headers http.Header) (*http.Response, error) {
	return executeCodexRoute(ctx, account, body, sessionID, proxyURL, apiKey, device, headers, true, executeNativeCodexCompactRequest)
}

func requestCodexBasispointsCandidate(ctx context.Context, model string) bool {
	if d := codexRouteFromContext(ctx); d != nil {
		for _, p := range d.eligiblePaths() {
			if p == database.CodexPathBasispoints && basispointsActiveForModel(model) {
				return true
			}
		}
		return false
	}
	return basispointsActiveForModel(model)
}

func codexRouteBudgetError(ctx context.Context) error {
	if d := codexRouteFromContext(ctx); d != nil {
		if err := d.client.Err(); err != nil {
			return err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.historyError != nil {
			return d.historyError
		}
		if d.policyError != nil {
			return d.policyError
		}
		if d.Remaining <= 0 {
			return routeLocalError("codex_route_budget_exhausted", fmt.Sprintf("Upstream attempt budget exhausted after %d attempts", len(d.Attempts)))
		}
	}
	return nil
}

func prepareNativeCodexRouteBody(body []byte, limits database.APIKeyLimits, compact bool) []byte {
	body = stripBasispointsRoutingFields(body)
	if compact || limits.ResolveImageGenerationPolicy() != database.ImageGenerationPolicyAllow || !basispointsActiveForModel(gjson.GetBytes(body, "model").String()) {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if shouldAutoInjectNativeResponsesImageGenerationTool(obj) {
		ensureResponsesImageGenerationTool(obj)
		moveTopLevelResponsesImageOptions(obj)
		normalizeResponsesImageGenerationTools(obj, "")
		applyResponsesImageGenerationBridgeInstructions(obj)
		if result, err := json.Marshal(obj); err == nil {
			return result
		}
	}
	return body
}

// Only reject a truly empty authorized intersection; busy leases remain queued.
func (h *Handler) codexRouteHasCandidates(ctx context.Context, keyID int64, filter auth.AccountFilter) bool {
	if codexRouteFromContext(ctx) == nil {
		return true
	}
	for _, account := range h.store.Accounts() {
		if account.AllowsAPIKey(keyID) && h.store.APIKeyAllowsAccount(keyID, account) && (filter == nil || filter(account)) {
			return true
		}
	}
	return false
}
