package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/google/uuid"
)

type imageRetention struct{ assets, jobs int }

func imageRetentionFromEnv() (imageRetention, error) {
	cfg := imageRetention{}
	for key, dest := range map[string]*int{"IMAGE_ASSET_RETENTION_DAYS": &cfg.assets, "IMAGE_JOB_RETENTION_DAYS": &cfg.jobs} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 3650 {
			return cfg, fmt.Errorf("%s must be 0..3650", key)
		}
		*dest = v
	}
	if cfg.jobs > 0 && (cfg.assets == 0 || cfg.jobs < cfg.assets) {
		return cfg, fmt.Errorf("job retention requires asset retention and must be at least as long")
	}
	return cfg, nil
}

// StartImageMaintenance schedules bounded expiration and orphan-file cleanup.
func (h *Handler) StartImageMaintenance(ctx context.Context) error {
	cfg, err := imageRetentionFromEnv()
	if err != nil {
		return err
	}
	if err = h.db.InitImageMaintenance(ctx); err != nil {
		return err
	}
	go func() {
		// Delay initial work until startup/migrations have completed. Work is bounded
		// and retried hourly; no vacuum or full-table rewrite on the request path.
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		var nextFullSweep time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			run, stop := context.WithTimeout(ctx, 5*time.Minute)
			now := time.Now()
			if err := h.cleanupDueImageAssets(run, now); err != nil {
				log.Printf("[image-retention] caller expiry cleanup failed: %v", err)
			}
			if !now.Before(nextFullSweep) {
				if err := h.cleanupImageStorage(run, cfg, now); err != nil {
					log.Printf("[image-retention] cleanup failed: %v", err)
				}
				nextFullSweep = now.Add(time.Hour)
			}
			stop()
			timer.Reset(time.Minute)
		}
	}()
	log.Printf("[image-retention] configured assets_days=%d jobs_days=%d temporary_grace_hours=24", cfg.assets, cfg.jobs)
	return nil
}

func (h *Handler) cleanupImageStorage(ctx context.Context, cfg imageRetention, now time.Time) error {
	deletedAssets, deletedJobs := 0, int64(0)
	var reclaimed int64
	if cfg.assets > 0 {
		cutoff := now.Add(-time.Duration(cfg.assets) * 24 * time.Hour)
		var after int64
		for batch := 0; batch < 10; batch++ {
			assets, err := h.db.ExpiredImageAssets(ctx, cutoff, after, 100)
			if err != nil {
				return err
			}
			if len(assets) == 0 {
				break
			}
			for _, asset := range assets {
				after = asset.ID
				if !imageAssetPathAllowed(asset.StoragePath) {
					log.Printf("[image-retention] skip disallowed asset=%d", asset.ID)
					continue
				}
				backend, err := imagestore.Resolve(asset.StoragePath)
				if err == nil {
					err = backend.Delete(ctx, asset.StoragePath)
				}
				if err != nil {
					log.Printf("[image-retention] delete failed asset=%d: %v", asset.ID, err)
					continue
				}
				// Keep the row if storage deletion fails, so another pass can retry it.
				if err = h.db.DeleteImageAsset(ctx, asset.ID); err != nil {
					return err
				}
				thumbCache.Invalidate(asset.ID)
				removeDiskThumbnails(asset.ID)
				deletedAssets++
				reclaimed += int64(asset.Bytes)
			}
		}
		after = 0
		for i := 0; i < 1000; i++ {
			id, err := h.db.PruneExpiredImageInputs(ctx, cutoff, after)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			after = id
		}
	}
	if cfg.jobs > 0 {
		var err error
		deletedJobs, err = h.db.DeleteExpiredImageJobs(ctx, now.Add(-time.Duration(cfg.jobs)*24*time.Hour), 1000)
		if err != nil {
			return err
		}
	}
	temp, err := h.cleanupOrphanImageInputs(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	pipelineRemoved, err := proxy.CleanupImagePipelineFiles(ctx, imageAssetDir(), now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	temp += pipelineRemoved
	log.Printf("[image-retention] completed assets_deleted=%d jobs_deleted=%d temporary_deleted=%d image_bytes_reclaimed=%d", deletedAssets, deletedJobs, temp, reclaimed)
	return nil
}

func (h *Handler) cleanupOrphanImageInputs(ctx context.Context, cutoff time.Time) (int, error) {
	active, err := h.db.ActiveImageQueueInputs(ctx)
	if err != nil {
		return 0, err
	}
	dir, err := os.Open(filepath.Join(imageAssetDir(), "queue-inputs"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	removed := 0
	for {
		entries, readErr := dir.ReadDir(100)
		if readErr != nil && readErr != io.EOF {
			return removed, readErr
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return removed, err
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if _, err = uuid.Parse(entry.Name()); err != nil {
				continue
			}
			token := queueInputPrefix + entry.Name()
			if active[token] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return removed, err
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			path, err := queueInputPath(token)
			if err != nil {
				return removed, err
			}
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return removed, err
			}
			removed++
		}
		if readErr == io.EOF {
			break
		}
	}
	return removed, nil
}
