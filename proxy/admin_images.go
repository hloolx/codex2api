package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// GenerateImageOnceForAdmin executes the existing Images API handler in-process.
// It keeps model aliasing, account dispatch, usage logging, and image parsing in one code path.
func (h *Handler) GenerateImageOnceForAdmin(ctx context.Context, rawBody []byte, apiKey *database.APIKeyRow, sharedAPIKeyConcurrency bool) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("image proxy handler is not initialized")
	}

	recorder := newAdminImageResponseRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(rawBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != nil && strings.TrimSpace(apiKey.Key) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey.Key)
		ginCtx.Set(contextAPIKeyRow, apiKey)
		ginCtx.Set(contextAPIKeyID, apiKey.ID)
		ginCtx.Set(contextAPIKeyName, strings.TrimSpace(apiKey.Name))
		ginCtx.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKey.Key))
	}
	ginCtx.Request = req

	// 每个批量输出都是一次真实上游请求，因此复用标准 Images handler 的
	// API Key 限额。并发槽由调用方决定：入队时已经为整个任务占过槽的路径
	// 传 true，否则这里会为同一个 Key 再占一次，把上限打对折。
	if sharedAPIKeyConcurrency {
		ginCtx.Set(contextAPIKeyConcurrencyInherited, true)
	}
	if status, msg := h.applyAdminScopeBudget(ctx, ginCtx, apiKey); status != 0 {
		return nil, status, fmt.Errorf("%s", msg)
	}

	h.ImagesGenerations(ginCtx)

	body := recorder.Body.Bytes()
	if recorder.Code < 200 || recorder.Code >= 300 {
		msg := strings.TrimSpace(extractAdminImageErrorMessage(body))
		if msg == "" {
			msg = fmt.Sprintf("image generation failed with HTTP %d", recorder.Code)
		}
		return body, recorder.Code, fmt.Errorf("%s", msg)
	}
	return body, recorder.Code, nil
}

func extractAdminImageErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	message := strings.TrimSpace(gjsonGetString(body, "error.message"))
	if message != "" {
		return message
	}
	message = strings.TrimSpace(gjsonGetString(body, "error"))
	if message != "" {
		return message
	}
	return strings.TrimSpace(string(body))
}

// GenerateImageEditForAdmin executes the ImagesEdits handler in-process for
// image-to-image (edit) jobs from the admin image studio.
func (h *Handler) GenerateImageEditForAdmin(ctx context.Context, rawBody []byte, apiKey *database.APIKeyRow, sharedAPIKeyConcurrency bool) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("image proxy handler is not initialized")
	}

	recorder := newAdminImageResponseRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(rawBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != nil && strings.TrimSpace(apiKey.Key) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey.Key)
		ginCtx.Set(contextAPIKeyRow, apiKey)
		ginCtx.Set(contextAPIKeyID, apiKey.ID)
		ginCtx.Set(contextAPIKeyName, strings.TrimSpace(apiKey.Name))
		ginCtx.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKey.Key))
	}
	ginCtx.Request = req

	if sharedAPIKeyConcurrency {
		ginCtx.Set(contextAPIKeyConcurrencyInherited, true)
	}
	if status, msg := h.applyAdminScopeBudget(ctx, ginCtx, apiKey); status != 0 {
		return nil, status, fmt.Errorf("%s", msg)
	}

	h.ImagesEdits(ginCtx)

	body := recorder.Body.Bytes()
	if recorder.Code < 200 || recorder.Code >= 300 {
		msg := strings.TrimSpace(extractAdminImageErrorMessage(body))
		if msg == "" {
			msg = fmt.Sprintf("image edit failed with HTTP %d", recorder.Code)
		}
		return body, recorder.Code, fmt.Errorf("%s", msg)
	}
	return body, recorder.Code, nil
}

func gjsonGetString(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}

// Unlike a network ResponseWriter, httptest.ResponseRecorder commits the first
// informational response. Image keepalives send 102 before the final response;
// do not let that heartbeat turn a successful image into a failed job.
type adminImageResponseRecorder struct {
	*httptest.ResponseRecorder
}

func newAdminImageResponseRecorder() *adminImageResponseRecorder {
	return &adminImageResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (w *adminImageResponseRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	w.ResponseRecorder.WriteHeader(code)
}

// applyAdminScopeBudget 为 in-process 的管理端生图任务计算 scope 预算闸门：
// reject 类超额直接返回 429，skip 类挂到该 gin context 上供账号过滤链剔除候选。
func (h *Handler) applyAdminScopeBudget(ctx context.Context, ginCtx *gin.Context, apiKey *database.APIKeyRow) (int, string) {
	if apiKey == nil || len(apiKey.Limits.ScopeLimits) == 0 {
		return 0, ""
	}
	gate, rejectMsg := h.evaluateAPIKeyScopeBudgets(ctx, apiKey)
	if rejectMsg != "" {
		return http.StatusTooManyRequests, rejectMsg
	}
	if gate != nil {
		ginCtx.Set(contextScopeBudgetGate, gate)
	}
	return 0, ""
}
