package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetAPIKeyConcurrency 读取内存快照，避免轮询时重复聚合历史用量。
func (h *Handler) GetAPIKeyConcurrency(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"concurrency": h.authCacheProxy.APIKeyConcurrencySnapshot()})
}
