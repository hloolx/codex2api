package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imageproc"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/internal/imageupscale"
	"github.com/codex2api/proxy"
	"github.com/google/uuid"
)

// Save each output before requesting the next. Neither input Base64 nor output
// buffers from previous outputs survive a wait on another upstream request.
func (h *Handler) runPipelinedImageJob(parent context.Context, id int64, owner string, req imageGenerationJobPayload, key *database.APIKeyRow) {
	ctx, cancel := context.WithTimeout(parent, imageJobTimeout(req.N))
	defer cancel()
	ctx, settle := proxy.DeferImageJobBilling(ctx)
	delivered := 0
	defer func() { settle(delivered) }()
	start := time.Now()
	ctx, p, err := proxy.NewImagePipeline(ctx, imageAssetDir(), id)
	if err != nil {
		h.markImageJobFailedDetached(id, "cannot create image pipeline: "+err.Error(), 0, owner)
		return
	}
	defer p.Close()
	count, err := normalizeImageJobOutputCount(req.N)
	if err != nil {
		h.markImageJobFailedDetached(id, err.Error(), 0, owner)
		return
	}
	var warnings []string
	for index := 0; index < count; index++ {
		if ctx.Err() != nil {
			warnings = append(warnings, ctx.Err().Error())
			break
		}
		n, notes, err := h.runPipelineOutput(ctx, id, req, key, p)
		p.Release()
		delivered += n
		warnings = append(warnings, notes...)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("output %d: %v", index+1, err))
		}
	}
	elapsed := int(time.Since(start).Milliseconds())
	if delivered == 0 {
		h.markImageJobFailedDetached(id, strings.Join(warnings, "; "), elapsed, owner)
	} else {
		h.markImageJobSucceededDetached(id, strings.Join(warnings, "; "), elapsed, owner)
	}
	log.Printf("[image-pipeline] job=%d finished requested=%d saved=%d duration_ms=%d", id, count, delivered, elapsed)
}

func (h *Handler) runPipelineOutput(ctx context.Context, id int64, req imageGenerationJobPayload, key *database.APIKeyRow, p *proxy.ImagePipeline) (int, []string, error) {
	req.N = 1
	saved := 0
	var notes []string
	p.Output = func(outputCtx context.Context, result proxy.QueuedImageResult) error {
		warnings, err := h.savePipelineImage(outputCtx, id, req, result)
		notes = append(notes, warnings...)
		if err == nil {
			saved++
		}
		return err
	}
	defer func() { p.Output = nil }()
	call := func(payload imageGenerationJobPayload) ([]byte, int, error) {
		return h.imageProxy.GenerateQueuedImage(ctx, func() ([]byte, error) {
			return buildPipelineJobPayload(ctx, payload)
		}, len(payload.InputImages) > 0, key)
	}
	body, status, err := call(req)
	if shouldFallbackImageJobToJPEG(req, status, err) && len(body) == 0 {
		req = jpegFallbackImageJobRequest(req)
		body, status, err = call(req)
	}
	if err == nil && saved == 0 {
		err = fmt.Errorf("upstream returned no image")
	}
	return saved, notes, err
}

func buildPipelineJobPayload(ctx context.Context, payload imageGenerationJobPayload) ([]byte, error) {
	payload.InputImages = append([]string(nil), payload.InputImages...)
	if err := loadQueueInputs(ctx, &payload); err != nil {
		return nil, err
	}
	if len(payload.InputImages) > 0 {
		return buildAdminImageEditRequest(payload)
	}
	return buildAdminImageGenerationRequest(payload)
}

// Read compressed image bytes only if a requested transformation is necessary.
// Normal saves stream the file to local/S3 storage without pixel decoding.
func (h *Handler) savePipelineImage(ctx context.Context, id int64, req imageGenerationJobPayload, result proxy.QueuedImageResult) ([]string, error) {
	backend, err := imagestore.Primary()
	if err != nil {
		return nil, err
	}
	scale := imageproc.NormalizeUpscale(req.Upscale)
	targetW, targetH, exact := imageupscale.ParseSize(req.Size)
	strict := strictImageJobSize(req)
	resize := false
	if strict && exact {
		resize = result.Width != targetW || result.Height != targetH
	} else if scale != "" {
		targetW, targetH, _ = imageupscale.TargetDimensions(result.Width, result.Height, imageproc.UpscaleLongSide(scale), req.Size)
		resize = result.Width < targetW || result.Height < targetH
	}
	source := io.ReadSeeker(result.File)
	size := result.Bytes
	format := result.Format
	width, height := result.Width, result.Height
	mime := mimeTypeForPipeline(format)
	var warnings []string
	if resize {
		data, err := io.ReadAll(result.File)
		if err != nil {
			return nil, err
		}
		output, outputMime, err := h.upscaleImageJobAsset(ctx, id, 1, data, req.Upscale, req.Size, req.UpscaleFit, strict)
		if err != nil {
			warnings = append(warnings, "upscale degraded; original image preserved: "+err.Error())
		}
		if len(output) > 0 && outputMime != "" {
			data = output
			mime = outputMime
			format = extensionFromMimeType(mime)
			width, height = imageDimensions(data)
		}
		source = bytes.NewReader(data)
		size = int64(len(data))
	}
	filename := fmt.Sprintf("%d-%s.%s", id, uuid.NewString(), safeImageExtension(format, mime))
	var ref string
	if streaming, ok := backend.(interface {
		SaveReader(context.Context, string, io.ReadSeeker, int64, string) (string, error)
	}); ok {
		ref, err = streaming.SaveReader(ctx, filename, source, size, mime)
	} else {
		var data []byte
		data, err = io.ReadAll(source)
		if err == nil {
			ref, err = backend.Save(ctx, filename, data, mime)
		}
	}
	if err != nil {
		return warnings, err
	}
	input := database.ImageAssetInput{JobID: id, TemplateID: req.TemplateID, Filename: filename, StoragePath: ref, MimeType: mime, Bytes: int(size), Width: width, Height: height, Model: firstNonEmpty(result.Model, req.Model), RequestedSize: req.Size, ActualSize: fmt.Sprintf("%dx%d", width, height), Quality: firstNonEmpty(result.Quality, req.Quality), OutputFormat: format, RevisedPrompt: result.RevisedPrompt}
	applyImageStoragePolicy(&input, req, time.Now())
	_, err = h.db.InsertImageAsset(ctx, input)
	if err != nil {
		_ = backend.Delete(ctx, ref)
		return warnings, err
	}
	log.Printf("[image-pipeline] job=%d stage=saved bytes=%d resized=%t", id, size, resize)
	return warnings, nil
}
func mimeTypeForPipeline(format string) string {
	switch format {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "image/png"
	}
}
