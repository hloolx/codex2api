package imageproc

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	stdimage "image"
	"image/png"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	_ "image/gif"
	_ "image/jpeg"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	UpscaleNone = ""
	Upscale2K   = "2k"
	Upscale4K   = "4k"

	ResizeFitInside = "inside"
	ResizeFitPad    = "pad"
	ResizeFitCover  = "cover"
)

var ErrUpscaleDecode = errors.New("image upscale: decode source failed")

func NormalizeUpscale(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case Upscale2K:
		return Upscale2K
	case Upscale4K:
		return Upscale4K
	default:
		return UpscaleNone
	}
}

func UpscaleLongSide(scale string) int {
	switch NormalizeUpscale(scale) {
	case Upscale2K:
		return 2560
	case Upscale4K:
		return 3840
	default:
		return 0
	}
}

func ComputeUpscaleCacheKey(src []byte, scale string) string {
	hash := sha256.Sum256(src)
	return hex.EncodeToString(hash[:16]) + "-" + NormalizeUpscale(scale)
}

func DoUpscale(src []byte, scale string) ([]byte, string, error) {
	scale = NormalizeUpscale(scale)
	target := UpscaleLongSide(scale)
	if target == 0 || len(src) == 0 {
		return src, "", nil
	}

	srcImg, _, err := stdimage.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUpscaleDecode, err)
	}

	bounds := srcImg.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, "", ErrUpscaleDecode
	}

	longSide := sw
	if sh > longSide {
		longSide = sh
	}
	if longSide >= target {
		return src, "", nil
	}

	dw, dh := scaledDimensions(sw, sh, target)
	return encodeUpscaled(srcImg, bounds, dw, dh)
}

// DoUpscaleTo resamples to an explicit target box instead of a tier long side,
// so a caller that knows the size the user actually requested (for example
// 2048x2048 rather than the 2K tier's 2560 long side) gets exactly that.
//
// When exact is false, or when the source aspect ratio does not match the box,
// the result is fitted inside the box with its aspect ratio preserved: this
// path never crops, so a mismatched upstream aspect yields a smaller image
// rather than silently losing content.
func DoUpscaleTo(src []byte, targetWidth, targetHeight int, exact bool) ([]byte, string, error) {
	if len(src) == 0 || targetWidth <= 0 || targetHeight <= 0 {
		return src, "", nil
	}

	srcImg, _, err := stdimage.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUpscaleDecode, err)
	}

	bounds := srcImg.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, "", ErrUpscaleDecode
	}
	if sw >= targetWidth && sh >= targetHeight {
		return src, "", nil
	}

	dw, dh := targetWidth, targetHeight
	if !exact || !sameAspectRatio(sw, sh, targetWidth, targetHeight) {
		dw, dh = fitInsideDimensions(sw, sh, targetWidth, targetHeight)
	}
	return encodeUpscaled(srcImg, bounds, dw, dh)
}

// NormalizeResizeFit returns the supported resize policy. Strict resize calls
// default to padding so the requested canvas is always exact without silently
// cropping content; legacy calls keep the historical fit-inside behavior.
func NormalizeResizeFit(value string, strict bool) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ResizeFitCover:
		return ResizeFitCover
	case ResizeFitPad:
		return ResizeFitPad
	case ResizeFitInside:
		if !strict {
			return ResizeFitInside
		}
	}
	if strict {
		return ResizeFitPad
	}
	return ResizeFitInside
}

// DoResizeTo produces an exact target canvas. Pad preserves the complete
// source and centers it on a transparent canvas; cover fills the canvas and
// crops equally from the two overflowing sides. Inside remains available for
// callers that explicitly need the legacy non-exact behavior.
func DoResizeTo(src []byte, targetWidth, targetHeight int, fit string) ([]byte, string, error) {
	if len(src) == 0 || targetWidth <= 0 || targetHeight <= 0 {
		return src, "", nil
	}

	srcImg, _, err := stdimage.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUpscaleDecode, err)
	}
	bounds := srcImg.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, "", ErrUpscaleDecode
	}

	fit = strings.ToLower(strings.TrimSpace(fit))
	if fit != ResizeFitInside && fit != ResizeFitCover && fit != ResizeFitPad {
		fit = ResizeFitPad
	}
	if sw == targetWidth && sh == targetHeight {
		return src, "", nil
	}
	if fit == ResizeFitInside {
		return DoUpscaleTo(src, targetWidth, targetHeight, false)
	}

	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, targetWidth, targetHeight))
	if fit == ResizeFitCover {
		crop := bounds
		sourceAspect := float64(sw) / float64(sh)
		targetAspect := float64(targetWidth) / float64(targetHeight)
		if sourceAspect > targetAspect {
			cropWidth := max(1, int(float64(sh)*targetAspect+0.5))
			left := bounds.Min.X + (sw-cropWidth)/2
			crop = stdimage.Rect(left, bounds.Min.Y, left+cropWidth, bounds.Max.Y)
		} else if sourceAspect < targetAspect {
			cropHeight := max(1, int(float64(sw)/targetAspect+0.5))
			top := bounds.Min.Y + (sh-cropHeight)/2
			crop = stdimage.Rect(bounds.Min.X, top, bounds.Max.X, top+cropHeight)
		}
		draw.CatmullRom.Scale(dst, dst.Bounds(), srcImg, crop, draw.Src, nil)
	} else {
		dw, dh := fitInsideDimensions(sw, sh, targetWidth, targetHeight)
		left := (targetWidth - dw) / 2
		top := (targetHeight - dh) / 2
		draw.CatmullRom.Scale(dst, stdimage.Rect(left, top, left+dw, top+dh), srcImg, bounds, draw.Src, nil)
	}

	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, dst); err != nil {
		return nil, "", fmt.Errorf("image resize: png encode: %w", err)
	}
	return buf.Bytes(), "image/png", nil
}

func encodeUpscaled(srcImg stdimage.Image, bounds stdimage.Rectangle, dw, dh int) ([]byte, string, error) {
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, dw, dh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), srcImg, bounds, draw.Src, nil)

	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, dst); err != nil {
		return nil, "", fmt.Errorf("image upscale: png encode: %w", err)
	}
	return buf.Bytes(), "image/png", nil
}

// sameAspectRatio tolerates the rounding that upstream sizes introduce, so a
// 1536x864 source still counts as 16:9 against a 2560x1440 target.
func sameAspectRatio(sw, sh, tw, th int) bool {
	const tolerance = 0.01
	source := float64(sw) / float64(sh)
	target := float64(tw) / float64(th)
	if target == 0 {
		return false
	}
	delta := source/target - 1
	return delta > -tolerance && delta < tolerance
}

func fitInsideDimensions(sw, sh, boxW, boxH int) (int, int) {
	scale := float64(boxW) / float64(sw)
	if heightScale := float64(boxH) / float64(sh); heightScale < scale {
		scale = heightScale
	}
	dw := int(float64(sw)*scale + 0.5)
	dh := int(float64(sh)*scale + 0.5)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	return dw, dh
}

func scaledDimensions(sw, sh, targetLongSide int) (int, int) {
	if sw >= sh {
		dw := targetLongSide
		dh := int(float64(sh) * float64(targetLongSide) / float64(sw))
		if dh < 1 {
			dh = 1
		}
		return dw, dh
	}
	dh := targetLongSide
	dw := int(float64(sw) * float64(targetLongSide) / float64(sh))
	if dw < 1 {
		dw = 1
	}
	return dw, dh
}

type UpscaleCache struct {
	mu       sync.Mutex
	items    map[string]*list.Element
	order    *list.List
	maxBytes int64
	curBytes int64
	sem      chan struct{}
}

type cacheEntry struct {
	key         string
	data        []byte
	contentType string
	size        int64
}

func NewUpscaleCache(maxBytes int64, concurrency int) *UpscaleCache {
	if maxBytes <= 0 {
		maxBytes = 512 * 1024 * 1024
	}
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
		if concurrency < 2 {
			concurrency = 2
		}
	}
	return &UpscaleCache{
		items:    make(map[string]*list.Element),
		order:    list.New(),
		maxBytes: maxBytes,
		sem:      make(chan struct{}, concurrency),
	}
}

func (c *UpscaleCache) Get(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return nil, "", false
	}
	c.order.MoveToFront(elem)
	entry := elem.Value.(*cacheEntry)
	return entry.data, entry.contentType, true
}

func (c *UpscaleCache) Put(key string, data []byte, contentType string) {
	size := int64(len(data))
	if key == "" || size == 0 || size > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*cacheEntry)
		c.curBytes -= entry.size
		entry.data = data
		entry.contentType = contentType
		entry.size = size
		c.curBytes += size
		c.order.MoveToFront(elem)
		c.evictLocked()
		return
	}

	entry := &cacheEntry{key: key, data: data, contentType: contentType, size: size}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	c.curBytes += size
	c.evictLocked()
}

func (c *UpscaleCache) evictLocked() {
	for c.curBytes > c.maxBytes {
		elem := c.order.Back()
		if elem == nil {
			return
		}
		entry := elem.Value.(*cacheEntry)
		delete(c.items, entry.key)
		c.curBytes -= entry.size
		c.order.Remove(elem)
	}
}

func (c *UpscaleCache) Acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *UpscaleCache) Release() {
	select {
	case <-c.sem:
	default:
	}
}

var (
	globalUpscaleCache     *UpscaleCache
	globalUpscaleCacheOnce sync.Once
)

func GlobalUpscaleCache() *UpscaleCache {
	globalUpscaleCacheOnce.Do(func() {
		budget := int64(512)
		if value, err := strconv.ParseInt(os.Getenv("IMAGE_UPSCALE_CACHE_MB"), 10, 64); err == nil && value >= 1 && value <= 4096 {
			budget = value
		}
		globalUpscaleCache = NewUpscaleCache(budget*1024*1024, 4)
	})
	return globalUpscaleCache
}
