package admin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imageproc"
	"github.com/codex2api/internal/imagestore"
)

// Thumbnail misses must not fan out into dozens of full image decodes.
var thumbnailRenderSlot = make(chan struct{}, 1)

func diskThumbnailPath(id int64, kb int) string {
	return filepath.Join(imageAssetDir(), ".thumbnails", strconv.FormatInt(id, 10), strconv.Itoa(kb)+".jpg")
}

func removeDiskThumbnails(id int64) {
	if id <= 0 {
		return
	}
	root, err := filepath.Abs(filepath.Join(imageAssetDir(), ".thumbnails"))
	if err != nil {
		return
	}
	dir := filepath.Join(root, strconv.FormatInt(id, 10))
	if filepath.Dir(dir) == root {
		_ = os.RemoveAll(dir)
	}
}

func cachedImageThumbnail(ctx context.Context, asset *database.ImageAsset, backend imagestore.Backend, kb int) ([]byte, string, error) {
	key := imagestore.ThumbKey(asset.ID, kb)
	read := func() ([]byte, string, bool) {
		if b, m, ok := thumbCache.Get(key); ok {
			return b, m, true
		}
		// Thumbnail files are small; never cache source images here.
		if f, e := os.Open(diskThumbnailPath(asset.ID, kb)); e == nil {
			defer f.Close()
			b, e := io.ReadAll(io.LimitReader(f, 2<<20))
			if e == nil && len(b) > 0 {
				thumbCache.Put(key, "image/jpeg", b)
				return b, "image/jpeg", true
			}
		}
		return nil, "", false
	}
	if b, m, ok := read(); ok {
		return b, m, nil
	}
	select {
	case thumbnailRenderSlot <- struct{}{}:
		defer func() { <-thumbnailRenderSlot }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if b, m, ok := read(); ok {
		return b, m, nil
	}
	rc, size, err := backend.Open(ctx, asset.StoragePath)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	if size > 64<<20 {
		return nil, "", fmt.Errorf("thumbnail source exceeds 64 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(rc, (64<<20)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > 64<<20 {
		return nil, "", fmt.Errorf("thumbnail source exceeds 64 MiB")
	}
	if err = checkQueueImage(data); err != nil {
		return nil, "", err
	}
	thumb, mime, ok := imageproc.MakeThumbnail(data, kb)
	if !ok {
		return nil, "", fmt.Errorf("thumbnail generation failed")
	}
	thumbCache.Put(key, mime, thumb)
	path := diskThumbnailPath(asset.ID, kb)
	if mime == "image/jpeg" && os.MkdirAll(filepath.Dir(path), 0755) == nil {
		if f, e := os.CreateTemp(filepath.Dir(path), ".thumb-*"); e == nil {
			name := f.Name()
			_, e = f.Write(thumb)
			closeErr := f.Close()
			if e == nil && closeErr == nil {
				_ = os.Rename(name, path)
			}
			_ = os.Remove(name)
		}
	}
	return thumb, mime, nil
}
