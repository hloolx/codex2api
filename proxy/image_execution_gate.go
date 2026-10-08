package proxy

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type imageExecutionGate struct{ slots chan struct{} }
type imageExecutionContextKey struct{}

var processImageGate atomic.Pointer[imageExecutionGate]

// Configure once before accepting traffic. Shared by all keys and both direct
// image endpoints and durable workers, including preprocessing/postprocessing.
func ConfigureImageExecutionLimit(limit int) {
	if limit <= 0 {
		processImageGate.Store(nil)
		return
	}
	processImageGate.Store(&imageExecutionGate{slots: make(chan struct{}, limit)})
}

func AcquireImageExecution(ctx context.Context) (context.Context, func(), error) {
	gate := processImageGate.Load()
	if gate == nil {
		return ctx, func() {}, nil
	}
	select {
	case gate.slots <- struct{}{}:
		return context.WithValue(ctx, imageExecutionContextKey{}, true), func() { <-gate.slots }, nil
	case <-ctx.Done():
		return ctx, nil, ctx.Err()
	}
}

func admitDirectImageExecution(c *gin.Context) (func(), bool) {
	if inherited, _ := c.Request.Context().Value(imageExecutionContextKey{}).(bool); inherited {
		return func() {}, true
	}
	memoryGate := processImagePipeline.Load()
	if memoryGate != nil {
		select {
		case memoryGate.slots <- struct{}{}:
		default:
			c.Header("Retry-After", "2")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "Image processing busy; use /v1/images/jobs"}})
			return nil, false
		}
	}
	releaseMemory := func() {
		if memoryGate != nil {
			<-memoryGate.slots
		}
	}
	gate := processImageGate.Load()
	if gate == nil {
		return releaseMemory, true
	}
	select {
	case gate.slots <- struct{}{}:
		return func() { <-gate.slots; releaseMemory() }, true
	default:
		releaseMemory()
		c.Header("Retry-After", "2")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "Image workers are busy; submit /v1/images/jobs to queue the request", "type": "rate_limit_error"}})
		return nil, false
	}
}

// Workers reserve per-key concurrency before downloading inputs. Saturation
// means wait, not creation of a failed job.
func (h *Handler) TryAcquireImageJobKey(row *database.APIKeyRow) (func(), bool) {
	if row == nil || row.Limits.MaxConcurrency <= 0 {
		return func() {}, true
	}
	release, _, ok := h.apiKeyConcurrencyLimiter().acquire(row.ID, row.Limits.MaxConcurrency)
	return release, ok
}
