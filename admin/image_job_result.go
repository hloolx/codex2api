package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// Explicit response types prevent persistence fields and image caches from
// leaking into this lightweight polling API when the database model grows.
type externalImageJobResult struct {
	ID           int64                         `json:"id"`
	Status       string                        `json:"status"`
	Assets       []externalImageJobResultAsset `json:"assets"`
	ErrorMessage string                        `json:"error_message"`
	Warning      string                        `json:"warning,omitempty"`
	DurationMs   int                           `json:"duration_ms"`
	CreatedAt    time.Time                     `json:"created_at"`
	StartedAt    *time.Time                    `json:"started_at,omitempty"`
	CompletedAt  *time.Time                    `json:"completed_at,omitempty"`
}

type externalImageJobResultAsset struct {
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	DeleteAfterRead bool   `json:"delete_after_read,omitempty"`
	ID              int64  `json:"id"`
	ProxyURL        string `json:"proxy_url"`
	ThumbnailURL    string `json:"thumbnail_url,omitempty"`
	MimeType        string `json:"mime_type"`
	Bytes           int    `json:"bytes"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	Model           string `json:"model"`
	OutputFormat    string `json:"output_format"`
}

type externalImageJobResultsRequest struct {
	IDs    []int64 `json:"ids"`
	JobIDs []int64 `json:"job_ids"`
}

type externalImageJobResultsResponse struct {
	Jobs       []externalImageJobResult `json:"jobs"`
	MissingIDs []int64                  `json:"missing_ids,omitempty"`
}

// GetExternalImageJobResult returns only status and output metadata. Inputs and
// cached output Base64 are omitted, including when include_cache=1 is supplied.
func (h *Handler) GetExternalImageJobResult(c *gin.Context) {
	apiKey := proxy.APIKeyRowFromContext(c)
	if apiKey == nil {
		writeExternalImageError(c, http.StatusUnauthorized, "Missing or invalid API key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: invalid job id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	job, err := h.db.GetImageGenerationJobResult(ctx, id, apiKey.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeExternalImageError(c, http.StatusNotFound, "Image job not found")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	decorateImageJobAssets(job)
	c.JSON(http.StatusOK, gin.H{"job": imageJobResultPayload(job)})
}

// GetExternalImageJobResults returns the result projection for a batch of jobs.
// Failed jobs are returned with status=failed so clients can mark them as
// terminal and skip image downloads; they are never retried by this endpoint.
func (h *Handler) GetExternalImageJobResults(c *gin.Context) {
	apiKey := proxy.APIKeyRowFromContext(c)
	if apiKey == nil {
		writeExternalImageError(c, http.StatusUnauthorized, "Missing or invalid API key")
		return
	}
	var request externalImageJobResultsRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: body must be valid JSON")
		return
	}
	ids := request.IDs
	if len(ids) == 0 {
		ids = request.JobIDs
	}
	if len(ids) == 0 || len(ids) > 500 {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: ids must contain 1 to 500 job ids")
		return
	}
	seen := make(map[int64]struct{}, len(ids))
	uniqueIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			writeExternalImageError(c, http.StatusBadRequest, "Invalid request: job ids must be positive")
			return
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	ids = uniqueIDs
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	jobs, err := h.db.ListImageGenerationJobResults(ctx, ids, apiKey.ID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	found := make(map[int64]struct{}, len(jobs))
	result := make([]externalImageJobResult, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		found[job.ID] = struct{}{}
		decorateImageJobAssets(job)
		result = append(result, imageJobResultPayload(job))
	}
	missing := make([]int64, 0)
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	c.JSON(http.StatusOK, externalImageJobResultsResponse{Jobs: result, MissingIDs: missing})
}

func imageJobResultPayload(job *database.ImageGenerationJob) externalImageJobResult {
	assets := make([]externalImageJobResultAsset, 0, len(job.Assets))
	for _, asset := range job.Assets {
		assets = append(assets, externalImageJobResultAsset{
			ExpiresAt: asset.ExpiresAt, DeleteAfterRead: asset.DeleteAfterRead,
			ID: asset.ID, ProxyURL: asset.ProxyURL, ThumbnailURL: asset.ThumbnailURL,
			MimeType: asset.MimeType, Bytes: asset.Bytes, Width: asset.Width, Height: asset.Height,
			Model: asset.Model, OutputFormat: asset.OutputFormat,
		})
	}
	return externalImageJobResult{
		ID: job.ID, Status: job.Status, Assets: assets, ErrorMessage: job.ErrorMessage,
		Warning: job.Warning, DurationMs: job.DurationMs, CreatedAt: job.CreatedAt,
		StartedAt: job.StartedAt, CompletedAt: job.CompletedAt,
	}
}
