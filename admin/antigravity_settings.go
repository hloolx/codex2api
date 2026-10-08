package admin

import (
	"log"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"

	"github.com/gin-gonic/gin"
)

// Antigravity 渠道设置：
//   - 模型重定向：下游请求不带思考强度后缀的逻辑模型（gemini-3.8-flash）时，自动按
//     配置落到某个固定档位（gemini-3.8-flash-high）。默认只补位缺失的
//     reasoning.effort，开启覆盖后连显式 effort 也按重定向走。
//   - 思考内容下发：开启后 OAuth 账号的 Gemini thought 片段作为 reasoning 输出下发。

type antigravitySettingsResponse struct {
	ModelRedirects          map[string]string                 `json:"model_redirects"`
	RedirectOverridesEffort bool                              `json:"redirect_overrides_effort"`
	ExposeThoughts          bool                              `json:"expose_thoughts"`
	Choices                 []proxy.AntigravityRedirectChoice `json:"choices"`
}

func buildAntigravitySettingsResponse(settings auth.AntigravitySettings) antigravitySettingsResponse {
	redirects := settings.ModelRedirects
	if redirects == nil {
		redirects = map[string]string{}
	}
	return antigravitySettingsResponse{
		ModelRedirects:          redirects,
		RedirectOverridesEffort: settings.RedirectOverridesEffort,
		ExposeThoughts:          settings.ExposeThoughts,
		Choices:                 proxy.AntigravityRedirectChoices(),
	}
}

// GetAntigravitySettings 返回当前 Antigravity 渠道设置与可选的重定向档位。
// GET /api/admin/settings/antigravity
func (h *Handler) GetAntigravitySettings(c *gin.Context) {
	c.JSON(http.StatusOK, buildAntigravitySettingsResponse(auth.ConfiguredAntigravitySettings()))
}

// UpdateAntigravitySettings 保存 Antigravity 渠道设置。只更新请求里出现的字段；
// model_redirects 整体替换，值为空的条目表示取消该模型的重定向。
// PUT /api/admin/settings/antigravity
// {"model_redirects":{"gemini-3.8-flash":"gemini-3.8-flash-high"},"redirect_overrides_effort":false,"expose_thoughts":true}
func (h *Handler) UpdateAntigravitySettings(c *gin.Context) {
	var req struct {
		ModelRedirects          *map[string]string `json:"model_redirects"`
		RedirectOverridesEffort *bool              `json:"redirect_overrides_effort"`
		ExposeThoughts          *bool              `json:"expose_thoughts"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.ModelRedirects == nil && req.RedirectOverridesEffort == nil && req.ExposeThoughts == nil {
		writeError(c, http.StatusBadRequest, "缺少 model_redirects、redirect_overrides_effort 或 expose_thoughts 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	settings := auth.ConfiguredAntigravitySettings()
	if req.ModelRedirects != nil {
		settings.ModelRedirects = *req.ModelRedirects
	}
	if req.RedirectOverridesEffort != nil {
		settings.RedirectOverridesEffort = *req.RedirectOverridesEffort
	}
	if req.ExposeThoughts != nil {
		settings.ExposeThoughts = *req.ExposeThoughts
	}
	normalized, err := auth.NormalizeAntigravitySettings(settings)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := proxy.ValidateAntigravityModelRedirects(normalized.ModelRedirects); err != nil {
		writeError(c, http.StatusBadRequest, "model_redirects 非法: "+err.Error())
		return
	}
	encoded, err := auth.EncodeAntigravitySettings(normalized)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.db.SaveAntigravityConfig(c.Request.Context(), encoded); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	auth.SetConfiguredAntigravitySettings(normalized)
	log.Printf("设置已更新: antigravity_config redirects=%d overrides_effort=%v expose_thoughts=%v", len(normalized.ModelRedirects), normalized.RedirectOverridesEffort, normalized.ExposeThoughts)
	c.JSON(http.StatusOK, buildAntigravitySettingsResponse(normalized))
}
