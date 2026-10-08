package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const daybreakOriginalKey = "daybreak_original_body"
const daybreakRequestKey = "daybreak_request"

type daybreakContextKey struct{}
type daybreakGuard struct {
	Request daybreakRequest
	Limits  database.APIKeyLimits
}

type daybreakRequest struct {
	Requested string
	Model     string
	Program   string
}

// model 保留下游请求名，effective_model 记录基础模型，Daybreak 程序单独记录。
func applyDaybreakUsageModel(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	value, _ := c.Get(daybreakRequestKey)
	request, _ := value.(daybreakRequest)
	if request.Program == "" {
		return
	}
	if input.Model == "" {
		input.Model = request.Requested
	}
	input.EffectiveModel = request.Model
	input.DaybreakProgram = request.Program
}

// 仅缓存路由字段，不能延长图片/base64 等大请求体的生命周期。
func rememberDaybreakRequest(c *gin.Context, body []byte) {
	c.Set(daybreakOriginalKey, daybreakRoutingMetadata(body))
}

func daybreakRoutingMetadata(body []byte) []byte {
	metadata, _ := sjson.SetBytes([]byte(`{}`), "model", gjson.GetBytes(body, "model").String())
	if cyber := gjson.GetBytes(body, "access_programs.cyber"); cyber.Exists() {
		metadata, _ = sjson.SetRawBytes(metadata, "access_programs.cyber", []byte(cyber.Raw))
	}
	return metadata
}

func unwrapDaybreakBody(body []byte) ([]byte, string, bool) {
	model := gjson.GetBytes(body, "model").String()
	base, program := auth.ParseDaybreakAlias(model)
	if program == "" {
		return body, model, false
	}
	body, _ = sjson.SetBytes(body, "model", base)
	body, _ = sjson.SetBytes(body, "access_programs.cyber", program)
	return body, base, true
}

func (h *Handler) applyConfiguredModelMappingToBody(body []byte, supported []string) ([]byte, string, string, bool) {
	body, original, effective, mapped := h.applyBaseModelMappingToBody(body, supported)
	body, base, daybreak := unwrapDaybreakBody(body)
	if daybreak {
		effective, mapped = base, true
	}
	return body, original, effective, mapped
}

func (h *Handler) resolveDaybreakRequest(c *gin.Context, fallback string) (daybreakRequest, error) {
	raw, _ := rawRequestBodyFromContext(c)
	if value, ok := c.Get(daybreakOriginalKey); ok {
		raw, _ = value.([]byte)
	}
	requested := gjson.GetBytes(raw, "model").String()
	if strings.HasSuffix(strings.ToLower(requested), "-daybreak") {
		return daybreakRequest{}, fmt.Errorf("use -daybreak-blue or -daybreak-red")
	}
	if h.store != nil && c.Request != nil && strings.HasSuffix(c.Request.URL.Path, "/messages") {
		if target, ok := resolveConfiguredModelMapping(requested, h.store.GetModelMapping(), nil); ok {
			raw, _ = sjson.SetBytes(raw, "model", target)
		}
	}
	mapped, _, _, _ := h.applyBaseModelMappingToBody(raw, nil)
	model, program := auth.ParseDaybreakAlias(gjson.GetBytes(mapped, "model").String())
	if strings.HasSuffix(model, "-daybreak") {
		return daybreakRequest{}, fmt.Errorf("use -daybreak-blue or -daybreak-red")
	}
	if program == "" {
		_, program = auth.ParseDaybreakAlias(requested)
	}
	program, err := resolveDaybreakProgram(program, gjson.GetBytes(raw, "access_programs.cyber"))
	if err != nil {
		return daybreakRequest{}, err
	}
	if program == "" {
		return daybreakRequest{}, nil
	}
	if fallback != "" {
		model = fallback
	}
	model, _ = auth.ParseDaybreakAlias(model)
	return daybreakRequest{Requested: requested, Model: model, Program: program}, nil
}

func resolveDaybreakProgram(program string, explicit gjson.Result) (string, error) {
	if program != "" && explicit.Exists() && explicit.String() != program {
		return "", fmt.Errorf("model conflicts with access_programs.cyber")
	}
	if program == "" && (explicit.String() == auth.DaybreakBlue || explicit.String() == auth.DaybreakRed) {
		return explicit.String(), nil
	}
	return program, nil
}

func (h *Handler) checkDaybreakRequest(c *gin.Context, model string) (string, int, string) {
	if c == nil {
		return model, 0, ""
	}
	c.Set(daybreakRequestKey, daybreakRequest{})
	request, err := h.resolveDaybreakRequest(c, model)
	if err != nil {
		return model, http.StatusBadRequest, err.Error()
	}
	guard := daybreakGuard{Request: request}
	if row := apiKeyRowFromContext(c); row != nil {
		guard.Limits = row.Limits
	}
	if c.Request != nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), daybreakContextKey{}, guard))
	}
	if request.Program == "" {
		return model, 0, ""
	}
	alias := auth.DaybreakAlias(request.Model, request.Program)
	if row := apiKeyRowFromContext(c); row != nil {
		for _, name := range []string{request.Requested, alias} {
			if msg := checkAPIKeyModel(name, row.Limits); msg != "" {
				return model, http.StatusForbidden, msg
			}
		}
	}
	c.Set(daybreakRequestKey, request)
	return request.Requested, 0, ""
}

func daybreakAccountFilter(c *gin.Context, filter auth.AccountFilter) auth.AccountFilter {
	if c == nil {
		return filter
	}
	value, _ := c.Get(daybreakRequestKey)
	request, _ := value.(daybreakRequest)
	if request.Program == "" {
		return filter
	}
	return func(account *auth.Account) bool {
		return account.SupportsDaybreak(request.Model, request.Program) && (filter == nil || filter(account))
	}
}

func (h *Handler) daybreakMappedModelDenied(model string, limits database.APIKeyLimits) bool {
	body, _ := sjson.SetBytes([]byte(`{}`), "model", model)
	if target, ok := resolveConfiguredModelMapping(model, h.store.GetModelMapping(), nil); ok {
		body, _ = sjson.SetBytes(body, "model", target)
	}
	body, _, _, _ = h.applyBaseModelMappingToBody(body, nil)
	base, program := auth.ParseDaybreakAlias(gjson.GetBytes(body, "model").String())
	return program != "" && checkAPIKeyModel(auth.DaybreakAlias(base, program), limits) != ""
}
