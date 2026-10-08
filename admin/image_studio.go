package admin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imageproc"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/internal/imageupscale"
	"github.com/codex2api/internal/signedasset"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const defaultImageAssetDir = "/data/images"
const maxInlineImageAssetCacheBytes = 64 * 1024 * 1024
const defaultSignedImageThumbKB = 32
const maxImageJobOutputCount = 4

var errInvalidUpscaledImage = errors.New("invalid upscaled image")

// thumbCache 跨请求复用缩略图。S3 后端读源图代价高，这里用 LRU 兜一下。
var thumbCache = imagestore.NewThumbnailCache(0)

type imagePromptTemplatePayload struct {
	Name         *string  `json:"name"`
	Prompt       *string  `json:"prompt"`
	Model        *string  `json:"model"`
	Size         *string  `json:"size"`
	Quality      *string  `json:"quality"`
	OutputFormat *string  `json:"output_format"`
	Background   *string  `json:"background"`
	Style        *string  `json:"style"`
	Tags         []string `json:"tags"`
	Favorite     *bool    `json:"favorite"`
}

type imageGenerationJobPayload struct {
	StorageMode      string   `json:"storage_mode,omitempty"`
	RetentionSeconds int64    `json:"retention_seconds,omitempty"`
	Prompt           string   `json:"prompt"`
	Model            string   `json:"model"`
	Size             string   `json:"size"`
	Quality          string   `json:"quality"`
	OutputFormat     string   `json:"output_format"`
	Background       string   `json:"background"`
	Style            string   `json:"style"`
	Upscale          string   `json:"upscale"`
	StrictSize       *bool    `json:"strict_size,omitempty"`
	UpscaleFit       string   `json:"upscale_fit,omitempty"`
	N                int      `json:"n"`
	APIKeyID         int64    `json:"api_key_id"`
	TemplateID       int64    `json:"template_id"`
	InputImages      []string `json:"input_images"`
	External         bool     `json:"-"`
}

func normalizeImageJobOutputCount(value int) (int, error) {
	if value == 0 {
		return 1, nil
	}
	if value < 1 || value > maxImageJobOutputCount {
		return 0, fmt.Errorf("n must be between 1 and %d", maxImageJobOutputCount)
	}
	return value, nil
}

type imageJobResponse struct {
	Job *database.ImageGenerationJob `json:"job"`
}

type externalImageJobResponse struct {
	Job *database.ImageGenerationJob `json:"job"`
}

type imageAssetFileOptions struct {
	download        bool
	private         bool
	thumbKB         int
	requireAssetDir bool
}

type imagePromptTemplatesResponse struct {
	Templates []database.ImagePromptTemplate `json:"templates"`
}

func (h *Handler) ListImagePromptTemplates(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	templates, err := h.db.ListImagePromptTemplates(ctx, c.Query("q"), c.Query("tag"))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if templates == nil {
		templates = []database.ImagePromptTemplate{}
	}
	c.JSON(http.StatusOK, imagePromptTemplatesResponse{Templates: templates})
}

func (h *Handler) CreateImagePromptTemplate(c *gin.Context) {
	var req imagePromptTemplatePayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	input, err := templateInputFromPayload(req, nil)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id, err := h.db.InsertImagePromptTemplate(ctx, input)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	template, err := h.db.GetImagePromptTemplate(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"template": template})
}

func (h *Handler) UpdateImagePromptTemplate(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	var req imagePromptTemplatePayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	existing, err := h.db.GetImagePromptTemplate(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "模板不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	input, err := templateInputFromPayload(req, existing)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.db.UpdateImagePromptTemplate(ctx, id, input); err != nil {
		writeInternalError(c, err)
		return
	}
	template, err := h.db.GetImagePromptTemplate(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"template": template})
}

func (h *Handler) DeleteImagePromptTemplate(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.DeleteImagePromptTemplate(ctx, id); err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, "已删除")
}

func templateInputFromPayload(req imagePromptTemplatePayload, existing *database.ImagePromptTemplate) (database.ImagePromptTemplateInput, error) {
	input := database.ImagePromptTemplateInput{}
	if existing != nil {
		input = database.ImagePromptTemplateInput{
			Name:         existing.Name,
			Prompt:       existing.Prompt,
			Model:        existing.Model,
			Size:         existing.Size,
			Quality:      existing.Quality,
			OutputFormat: existing.OutputFormat,
			Background:   existing.Background,
			Style:        existing.Style,
			Tags:         existing.Tags,
			Favorite:     existing.Favorite,
		}
	}
	if req.Name != nil {
		input.Name = security.SanitizeInput(*req.Name)
	}
	if req.Prompt != nil {
		input.Prompt = strings.TrimSpace(*req.Prompt)
	}
	if req.Model != nil {
		input.Model = normalizeImageStudioModel(*req.Model)
	}
	if req.Size != nil {
		input.Size = normalizeOptionalImageParam(*req.Size)
	}
	if req.Quality != nil {
		input.Quality = normalizeOptionalImageParam(*req.Quality)
	}
	if req.OutputFormat != nil {
		input.OutputFormat = normalizeOptionalImageParam(*req.OutputFormat)
	}
	if req.Background != nil {
		input.Background = normalizeOptionalImageParam(*req.Background)
	}
	if req.Style != nil {
		input.Style = normalizeOptionalImageParam(*req.Style)
	}
	if req.Tags != nil {
		input.Tags = req.Tags
	}
	if req.Favorite != nil {
		input.Favorite = *req.Favorite
	}
	if strings.TrimSpace(input.Name) == "" {
		input.Name = "未命名模板"
	}
	if len([]rune(input.Name)) > 100 {
		return input, fmt.Errorf("模板名称不能超过 100 个字符")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return input, fmt.Errorf("提示词不能为空")
	}
	if len([]rune(input.Prompt)) > 8000 {
		return input, fmt.Errorf("提示词不能超过 8000 个字符")
	}
	if input.Model == "" {
		input.Model = "gpt-image-2"
	}
	if input.OutputFormat == "" {
		input.OutputFormat = "png"
	}
	return input, nil
}

func (h *Handler) CreateImageGenerationJob(c *gin.Context) {
	releaseIntake, admitted := h.imageQueueIntake(c)
	if !admitted {
		return
	}
	defer releaseIntake()
	var req imageGenerationJobPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	if err := normalizeImageStoragePolicy(&req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeError(c, http.StatusBadRequest, "提示词不能为空")
		return
	}
	if len([]rune(req.Prompt)) > 8000 {
		writeError(c, http.StatusBadRequest, "提示词不能超过 8000 个字符")
		return
	}
	req.Model = normalizeImageStudioModel(req.Model)
	if req.Model == "" {
		req.Model = "gpt-image-2"
	}
	req.Size = normalizeOptionalImageParam(req.Size)
	req.Quality = normalizeOptionalImageParam(req.Quality)
	req.OutputFormat = normalizeOptionalImageParam(req.OutputFormat)
	if req.OutputFormat == "" {
		req.OutputFormat = "png"
	}
	req.Background = normalizeOptionalImageParam(req.Background)
	req.Style = normalizeOptionalImageParam(req.Style)
	normalizedUpscale, err := normalizeImageJobUpscale(req.Model, req.Size, req.Upscale)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Upscale = normalizedUpscale
	count, err := normalizeImageJobOutputCount(req.N)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.N = count

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	apiKey, err := h.resolveImageJobAPIKey(ctx, req.APIKeyID)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	paramsJSON, _ := json.Marshal(req)
	keyID, keyName, keyMasked := imageJobAPIKeyMeta(apiKey)
	if h.inspectImageStudioPromptFilter(c, proxy.AppendImageStyleToPrompt(req.Prompt, req.Style), req.Model, keyID, keyName, keyMasked) {
		return
	}
	if h.imageQueue != nil {
		h.persistQueuedImageJob(c, req, apiKey, http.StatusOK, false)
		return
	}
	jobID, err := h.db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{
		Prompt:       req.Prompt,
		ParamsJSON:   string(paramsJSON),
		APIKeyID:     keyID,
		APIKeyName:   keyName,
		APIKeyMasked: keyMasked,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if req.TemplateID > 0 {
		_ = h.db.IncrementImagePromptTemplateUsage(ctx, req.TemplateID)
	}
	job, err := h.db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	log.Printf("[image-studio] job=%d queued model=%s size=%s quality=%s format=%s background=%s upscale=%s style=%t api_key=%s template=%d prompt=%q",
		jobID,
		imageLogValue(req.Model),
		imageLogValue(req.Size),
		imageLogValue(req.Quality),
		imageLogValue(req.OutputFormat),
		imageLogValue(req.Background),
		imageLogValue(req.Upscale),
		strings.TrimSpace(req.Style) != "",
		imageLogAPIKeyLabel(keyID, keyName, keyMasked),
		req.TemplateID,
		imageLogPromptPreview(req.Prompt),
	)
	go h.runImageGenerationJob(jobID, req, apiKey, imageJobRunOptions{})
	c.JSON(http.StatusOK, imageJobResponse{Job: job})
}

func (h *Handler) CreateImageEditJob(c *gin.Context) {
	releaseIntake, admitted := h.imageQueueIntake(c)
	if !admitted {
		return
	}
	defer releaseIntake()
	var req imageGenerationJobPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	if err := normalizeImageStoragePolicy(&req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeError(c, http.StatusBadRequest, "提示词不能为空")
		return
	}
	if len(req.InputImages) == 0 {
		writeError(c, http.StatusBadRequest, "图生图需要上传参考图片")
		return
	}
	if len(req.InputImages) > proxy.MaxImageEditInputCount {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("参考图片数量超过限制 (%d, 最多 %d)", len(req.InputImages), proxy.MaxImageEditInputCount))
		return
	}
	if len([]rune(req.Prompt)) > 8000 {
		writeError(c, http.StatusBadRequest, "提示词不能超过 8000 个字符")
		return
	}
	req.Model = normalizeImageStudioModel(req.Model)
	if req.Model == "" {
		req.Model = "gpt-image-2"
	}
	req.Size = normalizeOptionalImageParam(req.Size)
	req.Quality = normalizeOptionalImageParam(req.Quality)
	req.OutputFormat = normalizeOptionalImageParam(req.OutputFormat)
	if req.OutputFormat == "" {
		req.OutputFormat = "png"
	}
	req.Background = normalizeOptionalImageParam(req.Background)
	req.Style = normalizeOptionalImageParam(req.Style)
	normalizedUpscale, err := normalizeImageJobUpscale(req.Model, req.Size, req.Upscale)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Upscale = normalizedUpscale
	count, err := normalizeImageJobOutputCount(req.N)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.N = count

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	apiKey, err := h.resolveImageJobAPIKey(ctx, req.APIKeyID)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	paramsJSON, _ := json.Marshal(req)
	keyID, keyName, keyMasked := imageJobAPIKeyMeta(apiKey)
	if h.inspectImageStudioPromptFilter(c, proxy.AppendImageStyleToPrompt(req.Prompt, req.Style), req.Model, keyID, keyName, keyMasked) {
		return
	}
	if h.imageQueue != nil {
		h.persistQueuedImageJob(c, req, apiKey, http.StatusOK, false)
		return
	}
	jobID, err := h.db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{
		Prompt:       req.Prompt,
		ParamsJSON:   string(paramsJSON),
		APIKeyID:     keyID,
		APIKeyName:   keyName,
		APIKeyMasked: keyMasked,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if req.TemplateID > 0 {
		_ = h.db.IncrementImagePromptTemplateUsage(ctx, req.TemplateID)
	}
	job, err := h.db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	log.Printf("[image-studio] job=%d queued mode=edit model=%s size=%s quality=%s format=%s image_count=%d api_key=%s template=%d prompt=%q",
		jobID,
		imageLogValue(req.Model),
		imageLogValue(req.Size),
		imageLogValue(req.Quality),
		imageLogValue(req.OutputFormat),
		len(req.InputImages),
		imageLogAPIKeyLabel(keyID, keyName, keyMasked),
		req.TemplateID,
		imageLogPromptPreview(req.Prompt),
	)
	go h.runImageEditJob(jobID, req, apiKey, imageJobRunOptions{})
	c.JSON(http.StatusOK, imageJobResponse{Job: job})
}

func (h *Handler) ListImageGenerationJobs(c *gin.Context) {
	page, pageSize := paginationParams(c, 20)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var result *database.ImageJobPage
	var err error
	if c.Query("summary") == "1" {
		result, err = h.db.ListImageJobSummaries(ctx, page, pageSize, 0)
	} else {
		result, err = h.db.ListImageGenerationJobs(ctx, page, pageSize, 0)
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	decorateImageJobPage(result)
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetImageGenerationJob(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var job *database.ImageGenerationJob
	if c.Query("summary") == "1" {
		job, err = h.db.GetImageJobSummary(ctx, id)
	} else {
		job, err = h.db.GetImageGenerationJob(ctx, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if c.Query("include_cache") == "1" && c.Query("summary") != "1" {
		h.attachImageJobAssetCachePayload(job)
	}
	decorateImageJobAssets(job)
	c.JSON(http.StatusOK, imageJobResponse{Job: job})
}

func (h *Handler) DeleteImageGenerationJob(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	job, err := h.db.GetImageGenerationJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if job.Status == database.ImageJobQueued || job.Status == database.ImageJobRunning {
		writeError(c, http.StatusConflict, "任务仍在处理中，完成或失败后才能删除")
		return
	}
	if err := h.db.DeleteImageGenerationJob(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "任务不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	for _, asset := range job.Assets {
		if asset.StoragePath != "" {
			if backend, err := imagestore.Resolve(asset.StoragePath); err == nil {
				_ = backend.Delete(ctx, asset.StoragePath)
			}
		}
		thumbCache.Invalidate(asset.ID)
		removeDiskThumbnails(asset.ID)
	}
	writeMessage(c, http.StatusOK, "已删除")
}

func (h *Handler) attachImageJobAssetCachePayload(job *database.ImageGenerationJob) {
	if job == nil || len(job.Assets) == 0 {
		return
	}
	for idx := range job.Assets {
		if imageAssetExpired(&job.Assets[idx], time.Now()) {
			continue
		}
		storagePath := strings.TrimSpace(job.Assets[idx].StoragePath)
		if storagePath == "" {
			continue
		}
		backend, err := imagestore.Resolve(storagePath)
		if err != nil {
			continue
		}
		// 本地后端走 Stat 快速判断尺寸；S3 后端没有便宜的 Stat，直接尝试 Read 并按字节数过滤。
		if storedBytes := job.Assets[idx].Bytes; storedBytes > 0 && storedBytes > maxInlineImageAssetCacheBytes {
			continue
		}
		if !imagestore.IsS3Ref(storagePath) {
			info, statErr := os.Stat(storagePath)
			if statErr != nil || info.Size() <= 0 || info.Size() > maxInlineImageAssetCacheBytes {
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		data, err := backend.Read(ctx, storagePath)
		cancel()
		if err != nil || len(data) == 0 || len(data) > maxInlineImageAssetCacheBytes {
			continue
		}
		job.Assets[idx].CacheB64JSON = base64.StdEncoding.EncodeToString(data)
	}
}

func (h *Handler) ListImageAssets(c *gin.Context) {
	page, pageSize := paginationParams(c, 24)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	result, err := h.db.ListImageAssets(ctx, page, pageSize, 0)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if result != nil {
		decorateImageAssets(result.Assets)
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetImageAssetFile(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	asset, err := h.db.GetImageAsset(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.serveImageAssetFile(c, asset, imageAssetFileOptions{
		download: c.Query("download") == "1",
		private:  true,
		thumbKB:  imageproc.ClampThumbKB(queryInt(c, "thumb_kb")),
	})
}

func (h *Handler) GetSignedImageAssetFile(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	exp, err := strconv.ParseInt(strings.TrimSpace(c.Query("exp")), 10, 64)
	if err != nil || exp <= 0 {
		writeError(c, http.StatusBadRequest, "签名参数无效")
		return
	}
	thumbKB := imageproc.ClampThumbKB(queryInt(c, "thumb_kb"))
	if !signedasset.VerifyImageAssetURL(id, exp, thumbKB, strings.TrimSpace(c.Query("sig")), time.Now()) {
		writeError(c, http.StatusForbidden, "图片链接已失效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	asset, err := h.db.GetImageAsset(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.serveImageAssetFile(c, asset, imageAssetFileOptions{
		thumbKB:         thumbKB,
		requireAssetDir: true,
	})
}

func (h *Handler) serveImageAssetFile(c *gin.Context, asset *database.ImageAsset, opts imageAssetFileOptions) {
	if asset == nil || strings.TrimSpace(asset.StoragePath) == "" {
		writeError(c, http.StatusNotFound, "图片文件不存在")
		return
	}
	if imageAssetExpired(asset, time.Now()) {
		c.Header("Cache-Control", "no-store")
		writeError(c, http.StatusGone, "图片已过期")
		return
	}
	if opts.requireAssetDir && !imageAssetPathAllowed(asset.StoragePath) {
		writeError(c, http.StatusNotFound, "图片文件不存在")
		return
	}
	backend, err := imagestore.Resolve(asset.StoragePath)
	if err != nil {
		writeError(c, http.StatusNotFound, "图片文件不存在")
		return
	}

	c.Request.Header.Del("If-Modified-Since")
	c.Request.Header.Del("If-None-Match")
	if opts.private || asset.ExpiresAt > 0 {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, private")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
	} else {
		c.Header("Cache-Control", "public, max-age=86400")
	}

	disposition := "inline"
	if opts.download {
		disposition = "attachment"
	}
	filename := sanitizeDownloadFilename(asset.Filename)
	if opts.thumbKB > 0 && !opts.download {
		data, contentType, err := cachedImageThumbnail(c.Request.Context(), asset, backend, opts.thumbKB)
		if err == nil {
			c.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, thumbnailFilename(filename)))
			c.Data(http.StatusOK, contentType, data)
			return
		}
		// Preserve the original endpoint's full-image fallback for formats that
		// cannot be thumbnailed. The source is streamed without pixel decoding.
		if c.Request.Context().Err() != nil {
			return
		}
	}

	if strings.TrimSpace(asset.MimeType) != "" {
		c.Header("Content-Type", asset.MimeType)
	}
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, filename))

	rc, size, err := backend.Open(c.Request.Context(), asset.StoragePath)
	if err != nil {
		writeError(c, http.StatusNotFound, "图片文件不存在")
		return
	}
	defer rc.Close()
	contentType := strings.TrimSpace(asset.MimeType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if size < 0 {
		// S3 偶尔不带 Content-Length，退化为 chunked。
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write(nil)
		_, _ = io.Copy(c.Writer, rc)
		return
	}
	c.DataFromReader(http.StatusOK, size, contentType, rc, nil)
}

func imageAssetPathAllowed(storagePath string) bool {
	storagePath = strings.TrimSpace(storagePath)
	if imagestore.IsS3Ref(storagePath) {
		// S3 ref 不在本地目录约束内，由后端自身保证作用域。
		return true
	}
	assetPath, err := filepath.Abs(storagePath)
	if err != nil {
		return false
	}
	basePath, err := filepath.Abs(imageAssetDir())
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(basePath, assetPath)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func thumbnailFilename(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	if base == "" {
		base = "image"
	}
	return base + ".thumb.jpg"
}

func decorateImageJobPage(page *database.ImageJobPage) {
	if page == nil {
		return
	}
	for idx := range page.Jobs {
		decorateImageAssets(page.Jobs[idx].Assets)
	}
}

func decorateImageJobAssets(job *database.ImageGenerationJob) {
	if job == nil {
		return
	}
	decorateImageAssets(job.Assets)
}

func decorateImageAssets(assets []database.ImageAsset) {
	for idx := range assets {
		decorateImageAsset(&assets[idx])
	}
}

func decorateImageAsset(asset *database.ImageAsset) {
	if asset == nil || asset.ID <= 0 {
		return
	}
	asset.ProxyURL = signedasset.ImageAssetURL(asset.ID, 0)
	asset.ThumbnailURL = signedasset.ImageAssetURL(asset.ID, defaultSignedImageThumbKB)
}

func (h *Handler) DeleteImageAsset(c *gin.Context) {
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	asset, err := h.db.GetImageAsset(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if err := h.db.DeleteImageAsset(ctx, id); err != nil {
		writeInternalError(c, err)
		return
	}
	if asset.StoragePath != "" {
		if backend, err := imagestore.Resolve(asset.StoragePath); err == nil {
			_ = backend.Delete(ctx, asset.StoragePath)
		}
		thumbCache.Invalidate(asset.ID)
		removeDiskThumbnails(asset.ID)
	}
	writeMessage(c, http.StatusOK, "已删除")
}

// imageJobRunOptions carries admission state from the enqueue site into the
// background job.
type imageJobRunOptions struct {
	// sharedAPIKeyConcurrency reports that the enqueue path already reserved the
	// API key's concurrency slot for this whole job, so the in-process upstream
	// calls must not reserve a second one for the same key.
	sharedAPIKeyConcurrency bool
	queueContext            context.Context
	queueOwner              string
}

// imageJobBaseTimeout budgets one upstream output. A batch issues one call per
// output, and may repeat the whole batch once through the JPEG fallback, so the
// deadline scales with the requested count instead of capping every job at the
// single-output budget.
const imageJobBaseTimeout = 12 * time.Minute

func imageJobTimeout(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > maxImageJobOutputCount {
		n = maxImageJobOutputCount
	}
	return imageJobBaseTimeout * time.Duration(n)
}

// imageJobStatusContext backs the terminal status write. The job's own context
// may already be past its deadline by the time the job finishes, and reusing it
// would leave the row stuck in "running" until the next restart.
func imageJobStatusContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func (h *Handler) markImageJobSucceededDetached(jobID int64, warning string, durationMs int, owners ...string) {
	ctx, cancel := imageJobStatusContext()
	defer cancel()
	if len(owners) > 0 && owners[0] != "" {
		if err := h.db.FinishLeasedImageJob(ctx, jobID, owners[0], database.ImageJobSucceeded, warning, durationMs); err != nil {
			logImageJobError(jobID, err)
		}
		return
	}
	var err error
	if warning != "" {
		err = h.db.MarkImageJobSucceededWithWarning(ctx, jobID, warning, durationMs)
	} else {
		err = h.db.MarkImageJobSucceeded(ctx, jobID, durationMs)
	}
	if err != nil {
		logImageJobError(jobID, err)
	}
}

func (h *Handler) markImageJobFailedDetached(jobID int64, message string, durationMs int, owners ...string) {
	ctx, cancel := imageJobStatusContext()
	defer cancel()
	if len(owners) > 0 && owners[0] != "" {
		if err := h.db.FinishLeasedImageJob(ctx, jobID, owners[0], database.ImageJobFailed, message, durationMs); err != nil {
			logImageJobError(jobID, err)
		}
		return
	}
	if err := h.db.MarkImageJobFailed(ctx, jobID, message, durationMs); err != nil {
		logImageJobError(jobID, err)
	}
}

func (h *Handler) runImageGenerationJob(jobID int64, req imageGenerationJobPayload, apiKey *database.APIKeyRow, opts imageJobRunOptions) {
	parent := opts.queueContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, imageJobTimeout(req.N))
	defer cancel()
	ctx, settleBilling := proxy.DeferImageJobBilling(ctx)
	deliveredImages := 0
	defer func() { settleBilling(deliveredImages) }()
	start := time.Now()
	if opts.queueOwner == "" {
		if err := h.db.MarkImageJobRunning(ctx, jobID); err != nil {
			logImageJobError(jobID, err)
			return
		}
	}
	log.Printf("[image-studio] job=%d started model=%s size=%s quality=%s format=%s background=%s api_key=%s prompt_chars=%d",
		jobID,
		imageLogValue(req.Model),
		imageLogValue(req.Size),
		imageLogValue(req.Quality),
		imageLogValue(req.OutputFormat),
		imageLogValue(req.Background),
		imageLogAPIKeyFromRow(apiKey),
		len([]rune(req.Prompt)),
	)

	styledPrompt := proxy.AppendImageStyleToPrompt(req.Prompt, req.Style)
	rawBody, err := buildAdminImageGenerationRequest(req)
	if err != nil {
		durationMs := int(time.Since(start).Milliseconds())
		log.Printf("[image-studio] job=%d failed duration=%s stage=build_request error=%s", jobID, imageLogDuration(durationMs), security.SanitizeLog(err.Error()))
		h.markImageJobFailedDetached(jobID, err.Error(), durationMs, opts.queueOwner)
		return
	}
	log.Printf("[image-studio] job=%d upstream request model=%s size=%s quality=%s format=%s body_bytes=%d prompt_chars=%d%s",
		jobID,
		imageLogValue(gjson.GetBytes(rawBody, "model").String()),
		imageLogValue(gjson.GetBytes(rawBody, "size").String()),
		imageLogValue(gjson.GetBytes(rawBody, "quality").String()),
		imageLogValue(gjson.GetBytes(rawBody, "output_format").String()),
		len(rawBody),
		len([]rune(styledPrompt)),
		imageLogPromptSuffix(styledPrompt, req.External),
	)
	imageProxy := h.imageProxy
	if imageProxy == nil {
		imageProxy = proxy.NewHandler(h.store, h.db, nil, nil)
	}
	responseJSON, upstreamStatus, partialErrors, err := runImageJobBatch(req.N, func() ([]byte, int, error) {
		return imageProxy.GenerateImageOnceForAdmin(ctx, rawBody, apiKey, opts.sharedAPIKeyConcurrency)
	})
	if shouldFallbackImageJobToJPEG(req, upstreamStatus, err) && len(responseJSON) == 0 {
		pngErr := err
		pngStatus := upstreamStatus
		fallbackReq := jpegFallbackImageJobRequest(req)
		fallbackBody, buildErr := buildAdminImageGenerationRequest(fallbackReq)
		if buildErr != nil {
			durationMs := int(time.Since(start).Milliseconds())
			log.Printf("[image-studio] job=%d failed duration=%s stage=build_jpeg_fallback error=%s", jobID, imageLogDuration(durationMs), security.SanitizeLog(buildErr.Error()))
			h.markImageJobFailedDetached(jobID, buildErr.Error(), durationMs, opts.queueOwner)
			return
		}
		fallbackStyledPrompt := proxy.AppendImageStyleToPrompt(fallbackReq.Prompt, fallbackReq.Style)
		log.Printf("[image-studio] job=%d png_failed_retrying_jpeg upstream_status=%d error=%s fallback_size=%s fallback_quality=%s fallback_background=%s fallback_prompt_chars=%d%s",
			jobID,
			pngStatus,
			security.SanitizeLog(pngErr.Error()),
			imageLogValue(fallbackReq.Size),
			imageLogValue(fallbackReq.Quality),
			imageLogValue(fallbackReq.Background),
			len([]rune(fallbackStyledPrompt)),
			imageLogPromptSuffix(fallbackStyledPrompt, req.External),
		)
		responseJSON, upstreamStatus, partialErrors, err = runImageJobBatch(req.N, func() ([]byte, int, error) {
			return imageProxy.GenerateImageOnceForAdmin(ctx, fallbackBody, apiKey, opts.sharedAPIKeyConcurrency)
		})
		if err == nil {
			req = fallbackReq
			rawBody = fallbackBody
			if paramsJSON, marshalErr := json.Marshal(fallbackReq); marshalErr == nil && opts.queueOwner == "" {
				if updateErr := h.db.UpdateImageGenerationJobParamsJSON(ctx, jobID, string(paramsJSON)); updateErr != nil {
					logImageJobError(jobID, updateErr)
				}
			}
			log.Printf("[image-studio] job=%d jpeg_fallback_succeeded original_status=%d upstream_status=%d body_bytes=%d",
				jobID,
				pngStatus,
				upstreamStatus,
				len(rawBody),
			)
		} else {
			err = fmt.Errorf("PNG 生成失败: %s；自动改用 JPEG 重试仍失败: %w", pngErr.Error(), err)
		}
	}
	durationMs := int(time.Since(start).Milliseconds())
	if err != nil {
		log.Printf("[image-studio] job=%d failed duration=%s upstream_status=%d error=%s",
			jobID,
			imageLogDuration(durationMs),
			upstreamStatus,
			security.SanitizeLog(err.Error()),
		)
		h.markImageJobFailedDetached(jobID, err.Error(), durationMs, opts.queueOwner)
		return
	}
	log.Printf("[image-studio] job=%d upstream completed duration=%s upstream_status=%d response_bytes=%d",
		jobID,
		imageLogDuration(durationMs),
		upstreamStatus,
		len(responseJSON),
	)

	assets, assetWarnings, err := h.saveImageJobAssets(ctx, jobID, req, responseJSON)
	deliveredImages = len(assets)
	partialErrors = append(partialErrors, assetWarnings...)
	if err != nil {
		if len(assets) == 0 {
			log.Printf("[image-studio] job=%d failed duration=%s stage=save_assets error=%s", jobID, imageLogDuration(durationMs), security.SanitizeLog(err.Error()))
			h.markImageJobFailedDetached(jobID, err.Error(), durationMs, opts.queueOwner)
			return
		}
		// Outputs saved before the failure remain available and will be billed.
		// Report the failure as a warning so they stay reachable instead of
		// becoming orphaned files behind a failed job.
		partialErrors = append(partialErrors, err.Error())
		log.Printf("[image-studio] job=%d partial_save_assets duration=%s saved=%d error=%s", jobID, imageLogDuration(durationMs), len(assets), security.SanitizeLog(err.Error()))
	}
	if len(assets) == 0 {
		log.Printf("[image-studio] job=%d failed duration=%s stage=save_assets error=%s", jobID, imageLogDuration(durationMs), "上游未返回图片")
		h.markImageJobFailedDetached(jobID, "上游未返回图片", durationMs, opts.queueOwner)
		return
	}
	if len(partialErrors) > 0 {
		warning := strings.Join(partialErrors, "; ")
		h.markImageJobSucceededDetached(jobID, warning, durationMs, opts.queueOwner)
		log.Printf("[image-studio] job=%d partial_success requested=%d completed=%d warning=%s",
			jobID, req.N, len(assets), security.SanitizeLog(warning))
	} else {
		h.markImageJobSucceededDetached(jobID, "", durationMs, opts.queueOwner)
	}
	log.Printf("[image-studio] job=%d succeeded duration=%s assets=%d total_bytes=%d first_size=%s dir=%s",
		jobID,
		imageLogDuration(durationMs),
		len(assets),
		imageAssetsTotalBytes(assets),
		imageLogFirstAssetSize(assets),
		security.SanitizeLog(imageAssetDir()),
	)
}

func buildAdminImageEditRequest(req imageGenerationJobPayload) ([]byte, error) {
	if len(req.InputImages) == 0 {
		return nil, fmt.Errorf("input_images is required for image edit")
	}
	if len(req.InputImages) > proxy.MaxImageEditInputCount {
		return nil, fmt.Errorf("too many input images (%d, max %d)", len(req.InputImages), proxy.MaxImageEditInputCount)
	}
	body := map[string]any{
		"model":           req.Model,
		"prompt":          proxy.AppendImageStyleToPrompt(req.Prompt, req.Style),
		"response_format": "b64_json",
	}
	images := make([]map[string]string, len(req.InputImages))
	for i, img := range req.InputImages {
		images[i] = map[string]string{"image_url": img}
	}
	body["images"] = images
	if upstreamSize := upstreamImageJobSize(req); upstreamSize != "" && upstreamSize != "auto" {
		body["size"] = upstreamSize
	}
	if req.Quality != "" && req.Quality != "auto" {
		body["quality"] = req.Quality
	}
	if req.OutputFormat != "" {
		body["output_format"] = req.OutputFormat
	}
	if req.Background != "" && req.Background != "auto" {
		body["background"] = req.Background
	}
	return json.Marshal(body)
}

func (h *Handler) runImageEditJob(jobID int64, req imageGenerationJobPayload, apiKey *database.APIKeyRow, opts imageJobRunOptions) {
	parent := opts.queueContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, imageJobTimeout(req.N))
	defer cancel()
	ctx, settleBilling := proxy.DeferImageJobBilling(ctx)
	deliveredImages := 0
	defer func() { settleBilling(deliveredImages) }()
	start := time.Now()
	if opts.queueOwner == "" {
		if err := h.db.MarkImageJobRunning(ctx, jobID); err != nil {
			logImageJobError(jobID, err)
			return
		}
	}
	log.Printf("[image-studio] job=%d started mode=edit model=%s image_count=%d prompt_chars=%d",
		jobID,
		imageLogValue(req.Model),
		len(req.InputImages),
		len([]rune(req.Prompt)),
	)

	rawBody, err := buildAdminImageEditRequest(req)
	if err != nil {
		durationMs := int(time.Since(start).Milliseconds())
		log.Printf("[image-studio] job=%d failed mode=edit duration=%s stage=build_request error=%s", jobID, imageLogDuration(durationMs), security.SanitizeLog(err.Error()))
		h.markImageJobFailedDetached(jobID, err.Error(), durationMs, opts.queueOwner)
		return
	}

	imageProxy := h.imageProxy
	if imageProxy == nil {
		imageProxy = proxy.NewHandler(h.store, h.db, nil, nil)
	}
	responseJSON, upstreamStatus, partialErrors, err := runImageJobBatch(req.N, func() ([]byte, int, error) {
		return imageProxy.GenerateImageEditForAdmin(ctx, rawBody, apiKey, opts.sharedAPIKeyConcurrency)
	})
	if shouldFallbackImageJobToJPEG(req, upstreamStatus, err) && len(responseJSON) == 0 {
		pngErr := err
		pngStatus := upstreamStatus
		fallbackReq := jpegFallbackImageJobRequest(req)
		fallbackBody, buildErr := buildAdminImageEditRequest(fallbackReq)
		if buildErr != nil {
			durationMs := int(time.Since(start).Milliseconds())
			log.Printf("[image-studio] job=%d failed mode=edit duration=%s stage=build_jpeg_fallback error=%s", jobID, imageLogDuration(durationMs), security.SanitizeLog(buildErr.Error()))
			h.markImageJobFailedDetached(jobID, buildErr.Error(), durationMs, opts.queueOwner)
			return
		}
		log.Printf("[image-studio] job=%d png_failed_retrying_jpeg mode=edit upstream_status=%d error=%s",
			jobID,
			pngStatus,
			security.SanitizeLog(pngErr.Error()),
		)
		responseJSON, upstreamStatus, partialErrors, err = runImageJobBatch(req.N, func() ([]byte, int, error) {
			return imageProxy.GenerateImageEditForAdmin(ctx, fallbackBody, apiKey, opts.sharedAPIKeyConcurrency)
		})
		if err == nil {
			req = fallbackReq
			rawBody = fallbackBody
			if paramsJSON, marshalErr := json.Marshal(fallbackReq); marshalErr == nil && opts.queueOwner == "" {
				if updateErr := h.db.UpdateImageGenerationJobParamsJSON(ctx, jobID, string(paramsJSON)); updateErr != nil {
					logImageJobError(jobID, updateErr)
				}
			}
			log.Printf("[image-studio] job=%d jpeg_fallback_succeeded mode=edit original_status=%d upstream_status=%d body_bytes=%d",
				jobID,
				pngStatus,
				upstreamStatus,
				len(rawBody),
			)
		} else {
			err = fmt.Errorf("PNG 生成失败: %s；自动改用 JPEG 重试仍失败: %w", pngErr.Error(), err)
		}
	}
	durationMs := int(time.Since(start).Milliseconds())
	if err != nil {
		log.Printf("[image-studio] job=%d failed mode=edit duration=%s upstream_status=%d error=%s",
			jobID,
			imageLogDuration(durationMs),
			upstreamStatus,
			security.SanitizeLog(err.Error()),
		)
		h.markImageJobFailedDetached(jobID, err.Error(), durationMs, opts.queueOwner)
		return
	}
	log.Printf("[image-studio] job=%d upstream completed mode=edit duration=%s upstream_status=%d response_bytes=%d",
		jobID,
		imageLogDuration(durationMs),
		upstreamStatus,
		len(responseJSON),
	)

	assets, assetWarnings, err := h.saveImageJobAssets(ctx, jobID, req, responseJSON)
	deliveredImages = len(assets)
	partialErrors = append(partialErrors, assetWarnings...)
	if err != nil {
		if len(assets) == 0 {
			log.Printf("[image-studio] job=%d failed mode=edit duration=%s stage=save_assets error=%s", jobID, imageLogDuration(durationMs), security.SanitizeLog(err.Error()))
			h.markImageJobFailedDetached(jobID, err.Error(), durationMs, opts.queueOwner)
			return
		}
		partialErrors = append(partialErrors, err.Error())
		log.Printf("[image-studio] job=%d partial_save_assets mode=edit duration=%s saved=%d error=%s", jobID, imageLogDuration(durationMs), len(assets), security.SanitizeLog(err.Error()))
	}
	if len(assets) == 0 {
		log.Printf("[image-studio] job=%d failed mode=edit duration=%s stage=save_assets error=%s", jobID, imageLogDuration(durationMs), "上游未返回图片")
		h.markImageJobFailedDetached(jobID, "上游未返回图片", durationMs, opts.queueOwner)
		return
	}
	if len(partialErrors) > 0 {
		warning := strings.Join(partialErrors, "; ")
		h.markImageJobSucceededDetached(jobID, warning, durationMs, opts.queueOwner)
		log.Printf("[image-studio] job=%d partial_success mode=edit requested=%d completed=%d warning=%s",
			jobID, req.N, len(assets), security.SanitizeLog(warning))
	} else {
		h.markImageJobSucceededDetached(jobID, "", durationMs, opts.queueOwner)
	}
	log.Printf("[image-studio] job=%d succeeded mode=edit duration=%s assets=%d total_bytes=%d first_size=%s",
		jobID,
		imageLogDuration(durationMs),
		len(assets),
		imageAssetsTotalBytes(assets),
		imageLogFirstAssetSize(assets),
	)
}

func buildAdminImageGenerationRequest(req imageGenerationJobPayload) ([]byte, error) {
	body := map[string]any{
		"model":           req.Model,
		"prompt":          proxy.AppendImageStyleToPrompt(req.Prompt, req.Style),
		"response_format": "b64_json",
	}
	if upstreamSize := upstreamImageJobSize(req); upstreamSize != "" && upstreamSize != "auto" {
		body["size"] = upstreamSize
	}
	if req.Quality != "" && req.Quality != "auto" {
		body["quality"] = req.Quality
	}
	if req.OutputFormat != "" {
		body["output_format"] = req.OutputFormat
	}
	if req.Background != "" && req.Background != "auto" {
		body["background"] = req.Background
	}
	return json.Marshal(body)
}

// upstreamImageJobSize converts an exact output canvas into a size accepted by
// GPT Image 2. The upstream image tool requires both dimensions to be multiples
// of 16, while callers commonly request display sizes such as 1920x1080. The
// original req.Size remains unchanged and is applied after generation, so this
// compatibility adjustment never changes the promised final dimensions.
func upstreamImageJobSize(req imageGenerationJobPayload) string {
	size := strings.TrimSpace(req.Size)
	width, height, ok := imageupscale.ParseSize(size)
	if !ok || !strictImageJobSize(req) {
		return size
	}
	const quantum = 16
	width = ((width + quantum - 1) / quantum) * quantum
	height = ((height + quantum - 1) / quantum) * quantum
	return fmt.Sprintf("%dx%d", width, height)
}

func strictImageJobSize(req imageGenerationJobPayload) bool {
	if _, _, ok := imageupscale.ParseSize(req.Size); !ok {
		return false
	}
	return req.StrictSize == nil || *req.StrictSize
}

func runImageJobBatch(count int, execute func() ([]byte, int, error)) ([]byte, int, []string, error) {
	count, err := normalizeImageJobOutputCount(count)
	if err != nil {
		return nil, 0, nil, err
	}
	var combined map[string]any
	var combinedData []any
	var partialErrors []string
	lastStatus := 0
	successStatus := 0
	for index := 0; index < count; index++ {
		responseJSON, status, callErr := execute()
		lastStatus = status
		if callErr != nil {
			partialErrors = append(partialErrors, fmt.Sprintf("output %d: %s", index+1, callErr.Error()))
			continue
		}
		var response map[string]any
		if err := json.Unmarshal(responseJSON, &response); err != nil {
			partialErrors = append(partialErrors, fmt.Sprintf("output %d: invalid upstream response", index+1))
			continue
		}
		data, ok := response["data"].([]any)
		if !ok || len(data) == 0 {
			partialErrors = append(partialErrors, fmt.Sprintf("output %d: upstream returned no image", index+1))
			continue
		}
		if combined == nil {
			combined = response
		}
		if successStatus == 0 {
			successStatus = status
		}
		combinedData = append(combinedData, data...)
	}
	if len(combinedData) == 0 {
		if len(partialErrors) == 0 {
			return nil, lastStatus, nil, fmt.Errorf("upstream returned no image")
		}
		return nil, lastStatus, partialErrors, fmt.Errorf("%s", strings.Join(partialErrors, "; "))
	}
	combined["data"] = combinedData
	combined["requested_n"] = count
	combined["completed_n"] = len(combinedData)
	result, err := json.Marshal(combined)
	if err != nil {
		return nil, lastStatus, partialErrors, err
	}
	return result, successStatus, partialErrors, nil
}

func shouldFallbackImageJobToJPEG(req imageGenerationJobPayload, upstreamStatus int, err error) bool {
	if err == nil || !isPNGOutputFormat(req.OutputFormat) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	for _, blocked := range []string{
		"无可用账号",
		"no available account",
		"unauthorized",
		"forbidden",
		"rate limit",
		"rate_limited",
		"quota",
		"invalid_request",
		"bad request",
		"content_policy",
		"safety",
		"unsupported_country_region_territory",
	} {
		if strings.Contains(message, blocked) {
			return false
		}
	}
	for _, marker := range []string{
		"server_error",
		"processing your request",
		"image generation failed with http 5",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return upstreamStatus == http.StatusBadGateway ||
		upstreamStatus == http.StatusInternalServerError ||
		upstreamStatus == http.StatusGatewayTimeout
}

func isPNGOutputFormat(outputFormat string) bool {
	outputFormat = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(outputFormat)), ".")
	return outputFormat == "" || outputFormat == "png"
}

func jpegFallbackImageJobRequest(req imageGenerationJobPayload) imageGenerationJobPayload {
	req.OutputFormat = "jpeg"
	if strings.EqualFold(strings.TrimSpace(req.Background), "transparent") {
		req.Background = "opaque"
	}
	return req
}

func (h *Handler) saveImageJobAssets(ctx context.Context, jobID int64, req imageGenerationJobPayload, responseJSON []byte) ([]database.ImageAsset, []string, error) {
	backend, err := imagestore.Primary()
	if err != nil {
		return nil, nil, fmt.Errorf("图片存储未初始化: %w", err)
	}
	if backend.Name() == imagestore.BackendLocal {
		// LocalBackend 已在 Configure 时 mkdir，这里再保险一次以兼容 dir 在运行期被外部清理的情况。
		if err := os.MkdirAll(imageAssetDir(), 0o755); err != nil {
			return nil, nil, fmt.Errorf("创建图库目录失败: %w", err)
		}
	}
	data := gjson.GetBytes(responseJSON, "data")
	if !data.IsArray() {
		return nil, nil, fmt.Errorf("生图响应缺少 data")
	}
	responseModel := firstNonEmpty(gjson.GetBytes(responseJSON, "model").String(), req.Model)
	responseSize := firstNonEmpty(gjson.GetBytes(responseJSON, "size").String(), req.Size)
	requestedSize := strings.TrimSpace(req.Size)
	if _, _, ok := imageupscale.ParseSize(requestedSize); !ok {
		requestedSize = responseSize
	}
	responseQuality := firstNonEmpty(gjson.GetBytes(responseJSON, "quality").String(), req.Quality)
	responseFormat := firstNonEmpty(gjson.GetBytes(responseJSON, "output_format").String(), req.OutputFormat, "png")

	var saved []database.ImageAsset
	var warnings []string
	for idx, item := range data.Array() {
		imageBytes, mimeType, format, err := decodeImageDataItem(item)
		if err != nil {
			return saved, warnings, err
		}
		if h.imageQueue != nil {
			if err := checkQueueImage(imageBytes); err != nil {
				return saved, warnings, fmt.Errorf("output %d: %w", idx+1, err)
			}
		}
		strictSize := strictImageJobSize(req)
		if req.Upscale != "" || strictSize {
			upscaledBytes, upscaledMime, upscaleErr := h.upscaleImageJobAsset(ctx, jobID, idx+1, imageBytes, req.Upscale, requestedSize, req.UpscaleFit, strictSize)
			if upscaleErr != nil {
				// Undecodable upscaler output is degraded the same way a
				// transport failure is. The upstream image behind it has
				// already been generated and billed, so keep it and warn
				// rather than failing every output in the batch.
				warning := fmt.Sprintf("output %d: upscale degraded; original image preserved: %s", idx+1, security.SanitizeLog(upscaleErr.Error()))
				warnings = append(warnings, warning)
				log.Printf("[image-studio] job=%d asset_index=%d upscale_degraded scale=%s backend=%s error=%s",
					jobID, idx+1, imageLogValue(req.Upscale), imageLogValue(imageUpscalerBackend()), security.SanitizeLog(upscaleErr.Error()))
			}
			if len(upscaledBytes) > 0 && upscaledMime != "" {
				imageBytes = upscaledBytes
				mimeType = upscaledMime
				format = extensionFromMimeType(upscaledMime)
				if format == "" {
					format = "png"
				}
			}
		}
		width, height := imageDimensions(imageBytes)
		if width <= 0 || height <= 0 {
			return saved, warnings, fmt.Errorf("output %d: 上游返回的图片无法解码", idx+1)
		}
		actualSize := ""
		if width > 0 && height > 0 {
			actualSize = fmt.Sprintf("%dx%d", width, height)
		}
		if format == "" {
			format = responseFormat
		}
		if mimeType == "" {
			mimeType = mime.TypeByExtension("." + format)
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		filename := fmt.Sprintf("%d-%02d-%s.%s", jobID, idx+1, uuid.NewString()[:8], safeImageExtension(format, mimeType))
		storagePath, err := backend.Save(ctx, filename, imageBytes, mimeType)
		if err != nil {
			return saved, warnings, fmt.Errorf("保存图片失败: %w", err)
		}
		input := database.ImageAssetInput{
			JobID:         jobID,
			TemplateID:    req.TemplateID,
			Filename:      filename,
			StoragePath:   storagePath,
			MimeType:      mimeType,
			Bytes:         len(imageBytes),
			Width:         width,
			Height:        height,
			Model:         responseModel,
			RequestedSize: requestedSize,
			ActualSize:    actualSize,
			Quality:       responseQuality,
			OutputFormat:  format,
			RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		}
		applyImageStoragePolicy(&input, req, time.Now())
		assetID, err := h.db.InsertImageAsset(ctx, input)
		if err != nil {
			_ = backend.Delete(ctx, storagePath)
			return saved, warnings, err
		}
		asset, err := h.db.GetImageAsset(ctx, assetID)
		if err != nil {
			return saved, warnings, err
		}
		decorateImageAsset(asset)
		saved = append(saved, *asset)
		log.Printf("[image-studio] job=%d asset=%d saved file=%s size=%s requested=%s bytes=%d format=%s mime=%s model=%s",
			jobID,
			asset.ID,
			security.SanitizeLog(filepath.Base(asset.Filename)),
			imageLogValue(asset.ActualSize),
			imageLogValue(asset.RequestedSize),
			asset.Bytes,
			imageLogValue(asset.OutputFormat),
			imageLogValue(asset.MimeType),
			imageLogValue(asset.Model),
		)
	}
	return saved, warnings, nil
}

func (h *Handler) upscaleImageJobAsset(ctx context.Context, jobID int64, assetIndex int, imageBytes []byte, scale, requestedSize, fit string, strict bool) ([]byte, string, error) {
	scale = imageproc.NormalizeUpscale(scale)
	if (scale == "" && !strict) || len(imageBytes) == 0 {
		return nil, "", nil
	}
	fit = imageproc.NormalizeResizeFit(fit, strict)
	cache := imageproc.GlobalUpscaleCache()
	key := imageproc.ComputeUpscaleCacheKey(imageBytes, scale) + "-" + strings.ToLower(strings.TrimSpace(requestedSize)) + "-" + fit + "-" + strconv.FormatBool(strict)
	if data, contentType, ok := cache.Get(key); ok && contentType != "" {
		return data, contentType, nil
	}

	upscaleCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := cache.Acquire(upscaleCtx); err != nil {
		log.Printf("[image-studio] job=%d asset_index=%d upscale_acquire_failed scale=%s backend=%s error=%s",
			jobID,
			assetIndex,
			imageLogValue(scale),
			imageLogValue(imageUpscalerBackend()),
			security.SanitizeLog(err.Error()),
		)
		return nil, "", err
	}
	defer cache.Release()

	if data, contentType, ok := cache.Get(key); ok && contentType != "" {
		return data, contentType, nil
	}

	beforeWidth, beforeHeight := imageDimensions(imageBytes)
	upscaled, contentType, method, err := imageupscale.BytesWithFit(upscaleCtx, imageBytes, scale, requestedSize, fit, strict)
	if err != nil {
		log.Printf("[image-studio] job=%d asset_index=%d upscale_failed scale=%s backend=%s error=%s",
			jobID,
			assetIndex,
			imageLogValue(scale),
			imageLogValue(imageUpscalerBackend()),
			security.SanitizeLog(err.Error()),
		)
		return nil, "", err
	}
	if contentType == "" {
		return nil, "", nil
	}
	afterWidth, afterHeight := imageDimensions(upscaled)
	if len(upscaled) == 0 || afterWidth <= 0 || afterHeight <= 0 {
		return nil, "", fmt.Errorf("%w: upscaler returned undecodable image data", errInvalidUpscaledImage)
	}

	cache.Put(key, upscaled, contentType)
	log.Printf("[image-studio] job=%d asset_index=%d upscale=%s method=%s from=%s to=%s bytes=%d->%d",
		jobID,
		assetIndex,
		imageLogValue(scale),
		imageLogValue(method),
		imageLogDimensions(beforeWidth, beforeHeight),
		imageLogDimensions(afterWidth, afterHeight),
		len(imageBytes),
		len(upscaled),
	)
	return upscaled, contentType, nil
}

func (h *Handler) resolveImageJobAPIKey(ctx context.Context, id int64) (*database.APIKeyRow, error) {
	if id > 0 {
		key, err := h.db.GetAPIKeyByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("API Key 不存在")
		}
		return key, err
	}
	key, err := h.db.FirstAPIKey(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return key, err
}

func imageJobAPIKeyMeta(key *database.APIKeyRow) (int64, string, string) {
	if key == nil {
		return 0, "", ""
	}
	return key.ID, strings.TrimSpace(key.Name), security.MaskAPIKey(key.Key)
}

func decodeImageDataItem(item gjson.Result) ([]byte, string, string, error) {
	raw := strings.TrimSpace(item.Get("b64_json").String())
	mimeType := ""
	if raw == "" {
		url := strings.TrimSpace(item.Get("url").String())
		if strings.HasPrefix(strings.ToLower(url), "data:") {
			if comma := strings.Index(url, ","); comma >= 0 {
				meta := url[:comma]
				raw = url[comma+1:]
				mimeType = strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64"), ";")
			}
		}
	}
	if raw == "" {
		return nil, "", "", fmt.Errorf("生图响应缺少图片数据")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(raw)
	}
	var data []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		data, err = encoding.DecodeString(raw)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("图片 Base64 解码失败: %w", err)
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	format := strings.ToLower(strings.TrimSpace(item.Get("output_format").String()))
	if format == "" {
		format = extensionFromMimeType(mimeType)
	}
	return data, mimeType, format, nil
}

func imageDimensions(data []byte) (int, int) {
	return imageupscale.Dimensions(data)
}

func imageAssetDir() string {
	if dir := strings.TrimSpace(os.Getenv("IMAGE_ASSET_DIR")); dir != "" {
		return dir
	}
	return defaultImageAssetDir
}

// normalizeImageStudioModel 生图台只接受 Codex 生图模型族(gpt-image-2 及 -2k/-4k 档位
// 别名、带日期快照名);其他名字回落到 gpt-image-2。
// Flare/Sunburst、日期快照和尺寸别名均按 gpt-image-* 前缀准入。
func normalizeImageStudioModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || proxy.IsGPTImageModel(model) {
		return model
	}
	return "gpt-image-2"
}

func normalizeImageJobUpscale(model, size, upscale string) (string, error) {
	rawUpscale := strings.ToLower(strings.TrimSpace(upscale))
	// An absent value means "decide from the model and size"; "none" is the
	// explicit opt-out that suppresses that inference, so a caller can take the
	// upstream image as-is even when it asked for a size upstream cannot render.
	if rawUpscale == "none" || rawUpscale == "off" {
		return "", nil
	}
	if rawUpscale != "" {
		normalized := imageproc.NormalizeUpscale(rawUpscale)
		if normalized == "" {
			return "", fmt.Errorf("upscale must be 2k, 4k or none")
		}
		return normalized, nil
	}

	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	if strings.HasSuffix(normalizedModel, "-4k") {
		return imageproc.Upscale4K, nil
	}
	if strings.HasSuffix(normalizedModel, "-2k") {
		return imageproc.Upscale2K, nil
	}

	normalizedSize := strings.ToLower(strings.TrimSpace(size))
	if normalizedSize == imageproc.Upscale4K {
		return imageproc.Upscale4K, nil
	}
	if normalizedSize == imageproc.Upscale2K {
		return imageproc.Upscale2K, nil
	}
	parts := strings.Split(normalizedSize, "x")
	if len(parts) != 2 {
		return "", nil
	}
	width, widthErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, heightErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return "", nil
	}
	longEdge := max(width, height)
	if longEdge > 2560 {
		return imageproc.Upscale4K, nil
	}
	if longEdge >= 2048 {
		return imageproc.Upscale2K, nil
	}
	return "", nil
}

func normalizeOptionalImageParam(value string) string {
	return strings.TrimSpace(value)
}

func parsePositiveIDParam(c *gin.Context, name string) (int64, error) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func queryInt(c *gin.Context, name string) int {
	value, err := strconv.Atoi(strings.TrimSpace(c.Query(name)))
	if err != nil {
		return 0
	}
	return value
}

func paginationParams(c *gin.Context, defaultPageSize int) (int, int) {
	page := 1
	if raw := c.Query("page"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			page = parsed
		}
	}
	pageSize := defaultPageSize
	if raw := c.Query("page_size"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			pageSize = parsed
		}
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

func safeImageExtension(format string, mimeType string) string {
	format = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
	switch format {
	case "png", "jpg", "jpeg", "webp", "gif":
		if format == "jpg" {
			return "jpg"
		}
		return format
	}
	ext := strings.TrimPrefix(extensionFromMimeType(mimeType), ".")
	if ext != "" {
		return ext
	}
	return "bin"
}

func extensionFromMimeType(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch mimeType {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

func sanitizeDownloadFilename(filename string) string {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "." || filename == "/" || filename == "" {
		return "image.png"
	}
	return strings.NewReplacer(`"`, "", "\n", "", "\r", "").Replace(filename)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func imageLogValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "auto"
	}
	return security.SanitizeLog(value)
}

func imageLogPromptPreview(prompt string) string {
	prompt = strings.Join(strings.Fields(prompt), " ")
	runes := []rune(prompt)
	if len(runes) > 96 {
		prompt = string(runes[:96]) + "..."
	}
	return security.SanitizeLog(prompt)
}

func imageLogPromptSuffix(prompt string, redact bool) string {
	if redact {
		return ""
	}
	return fmt.Sprintf(" prompt=%q", imageLogPromptPreview(prompt))
}

func imageLogAPIKeyLabel(id int64, name string, masked string) string {
	if id <= 0 {
		return "none"
	}
	name = strings.TrimSpace(name)
	if name != "" {
		return fmt.Sprintf("%d/%s", id, security.SanitizeLog(name))
	}
	masked = strings.TrimSpace(masked)
	if masked != "" {
		return fmt.Sprintf("%d/%s", id, security.SanitizeLog(masked))
	}
	return fmt.Sprintf("%d", id)
}

func imageLogAPIKeyFromRow(key *database.APIKeyRow) string {
	id, name, masked := imageJobAPIKeyMeta(key)
	return imageLogAPIKeyLabel(id, name, masked)
}

func imageLogDuration(durationMs int) string {
	if durationMs <= 0 {
		return "0s"
	}
	return (time.Duration(durationMs) * time.Millisecond).String()
}

func imageAssetsTotalBytes(assets []database.ImageAsset) int {
	total := 0
	for _, asset := range assets {
		total += asset.Bytes
	}
	return total
}

func imageLogFirstAssetSize(assets []database.ImageAsset) string {
	for _, asset := range assets {
		if strings.TrimSpace(asset.ActualSize) != "" {
			return imageLogValue(asset.ActualSize)
		}
	}
	return "unknown"
}

func imageLogDimensions(width, height int) string {
	if width <= 0 || height <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%dx%d", width, height)
}

func logImageJobError(jobID int64, err error) {
	if err != nil {
		log.Printf("[image-studio] job=%d state_update_failed error=%s", jobID, security.SanitizeLog(err.Error()))
	}
}
