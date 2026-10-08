package admin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// GetExternalImageJobOutput streams Base64 without aggregating decoded images
// or Base64 JSON in memory. Only this explicit delivery route consumes images
// whose caller opted into delete_after_read; polling/preview routes never do.
func (h *Handler) GetExternalImageJobOutput(c *gin.Context) {
	job := h.ownedCompletedImageJob(c)
	if job == nil {
		return
	}
	c.Header("Cache-Control", "no-store")
	if len(job.Assets) == 0 {
		writeExternalImageError(c, http.StatusGone, "Image output is unavailable, expired or already consumed")
		return
	}
	if len(job.Assets) > maxImageJobOutputCount {
		writeExternalImageError(c, http.StatusRequestEntityTooLarge, "Too many outputs; download individual assets")
		return
	}
	readers := make([]io.ReadCloser, 0, len(job.Assets))
	closeReaders := func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
		readers = nil
	}
	defer closeReaders()
	for _, asset := range job.Assets {
		if imageAssetExpired(&asset, time.Now()) {
			writeExternalImageError(c, http.StatusGone, "Image output has expired")
			return
		}
		if !imageAssetPathAllowed(asset.StoragePath) {
			writeExternalImageError(c, http.StatusNotFound, "Image output not found")
			return
		}
		backend, err := imagestore.Resolve(asset.StoragePath)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		reader, size, err := backend.Open(c.Request.Context(), asset.StoragePath)
		if err != nil {
			writeExternalImageError(c, http.StatusGone, "Image output file is unavailable")
			return
		}
		readers = append(readers, reader)
		if asset.Bytes <= 0 || (size >= 0 && size != int64(asset.Bytes)) {
			writeExternalImageError(c, http.StatusInternalServerError, "Image output size mismatch")
			return
		}
	}
	c.Header("Content-Type", "application/json")
	c.Status(http.StatusOK)
	err := streamImageJobOutput(c.Request.Context(), c.Writer, job, readers)
	closeReaders() // Windows requires closed files before removing them.
	if err != nil || c.Request.Context().Err() != nil {
		// A failed/partial delivery must retain the images for retry until TTL.
		if err != nil {
			logImageJobError(job.ID, err)
		}
		return
	}
	for _, asset := range job.Assets {
		if asset.DeleteAfterRead {
			ctx, cancel := imageJobStatusContext()
			if err := h.removeRetainedImageAsset(ctx, asset); err != nil {
				logImageJobError(job.ID, err)
			}
			cancel()
		}
	}
}

func streamImageJobOutput(ctx context.Context, w io.Writer, job *database.ImageGenerationJob, readers []io.ReadCloser) error {
	if _, err := fmt.Fprintf(w, `{"job_id":%d,"data":[`, job.ID); err != nil {
		return err
	}
	for i, asset := range job.Assets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		metadata, err := json.Marshal(struct {
			ID       int64  `json:"id"`
			MimeType string `json:"mime_type"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
		}{asset.ID, asset.MimeType, asset.Width, asset.Height})
		if err != nil {
			return err
		}
		if _, err = w.Write(metadata[:len(metadata)-1]); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `,"b64_json":"`); err != nil {
			return err
		}
		encoder := base64.NewEncoder(base64.StdEncoding, w)
		if _, err = io.CopyN(encoder, readers[i], int64(asset.Bytes)); err != nil {
			return err
		}
		if err = encoder.Close(); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `"}`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]}")
	return err
}

// AcknowledgeExternalImageJobOutput lets URL-based clients explicitly confirm
// durable receipt. It is idempotent and never deletes legacy/temporary outputs.
func (h *Handler) AcknowledgeExternalImageJobOutput(c *gin.Context) {
	job := h.ownedCompletedImageJob(c)
	if job == nil {
		return
	}
	for _, asset := range job.Assets {
		if !asset.DeleteAfterRead {
			continue
		}
		if err := h.removeRetainedImageAsset(c.Request.Context(), asset); err != nil {
			writeInternalError(c, err)
			return
		}
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ownedCompletedImageJob(c *gin.Context) *database.ImageGenerationJob {
	key := proxy.APIKeyRowFromContext(c)
	if key == nil {
		writeExternalImageError(c, http.StatusUnauthorized, "Missing or invalid API key")
		return nil
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid job id")
		return nil
	}
	job, err := h.db.GetImageGenerationJobResult(c.Request.Context(), id, key.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeExternalImageError(c, http.StatusNotFound, "Image job not found")
		return nil
	}
	if err != nil {
		writeInternalError(c, err)
		return nil
	}
	if job.Status == database.ImageJobQueued || job.Status == database.ImageJobRunning {
		writeExternalImageError(c, http.StatusConflict, "Image job is not complete")
		return nil
	}
	return job
}
