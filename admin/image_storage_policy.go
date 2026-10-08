package admin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
)

const (
	imageStorageLegacy                 = "legacy"
	imageStorageTemporary              = "temporary"
	imageStorageDeleteAfterRead        = "delete_after_read"
	defaultImageRetentionSeconds int64 = 2 * 60 * 60
	maxImageRetentionSeconds     int64 = 365 * 24 * 60 * 60
)

func normalizeImageStoragePolicy(req *imageGenerationJobPayload) error {
	req.StorageMode = strings.ToLower(strings.TrimSpace(req.StorageMode))
	switch req.StorageMode {
	case "", imageStorageLegacy:
		if req.RetentionSeconds != 0 {
			return fmt.Errorf("retention_seconds requires temporary or delete_after_read storage_mode")
		}
	case imageStorageTemporary, imageStorageDeleteAfterRead:
		if req.RetentionSeconds == 0 {
			req.RetentionSeconds = defaultImageRetentionSeconds
		}
		if req.RetentionSeconds < 60 || req.RetentionSeconds > maxImageRetentionSeconds {
			return fmt.Errorf("retention_seconds must be 60..31536000")
		}
	default:
		return fmt.Errorf("storage_mode must be legacy, temporary or delete_after_read")
	}
	return nil
}

func applyImageStoragePolicy(input *database.ImageAssetInput, req imageGenerationJobPayload, now time.Time) {
	if req.StorageMode == imageStorageTemporary || req.StorageMode == imageStorageDeleteAfterRead {
		input.ExpiresAt = now.Unix() + req.RetentionSeconds
		input.DeleteAfterRead = req.StorageMode == imageStorageDeleteAfterRead
	}
}

func imageAssetExpired(asset *database.ImageAsset, now time.Time) bool {
	return asset.ExpiresAt > 0 && asset.ExpiresAt <= now.Unix()
}

// Delete storage first, retaining an expired row on failure so maintenance can
// retry. Revocation also prevents cached/thumbnail reads while deletion retries.
func (h *Handler) removeRetainedImageAsset(ctx context.Context, asset database.ImageAsset) error {
	if !imageAssetPathAllowed(asset.StoragePath) {
		return fmt.Errorf("image asset %d has a disallowed storage path", asset.ID)
	}
	if err := h.db.ExpireImageAsset(ctx, asset.ID, time.Now()); err != nil {
		return err
	}
	thumbCache.Invalidate(asset.ID)
	removeDiskThumbnails(asset.ID)
	backend, err := imagestore.Resolve(asset.StoragePath)
	if err != nil {
		return err
	}
	if err := backend.Delete(ctx, asset.StoragePath); err != nil {
		return err
	}
	return h.db.DeleteImageAsset(ctx, asset.ID)
}

func (h *Handler) cleanupDueImageAssets(ctx context.Context, now time.Time) error {
	var after int64
	for batch := 0; batch < 10; batch++ {
		assets, err := h.db.DueImageAssets(ctx, now, after, 100)
		if err != nil {
			return err
		}
		if len(assets) == 0 {
			return nil
		}
		for _, asset := range assets {
			after = asset.ID
			if err := h.removeRetainedImageAsset(ctx, asset); err != nil {
				logImageJobError(asset.JobID, err)
			}
		}
	}
	return nil
}
