package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/egressipv6"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetIPv6Egress(c *gin.Context) {
	cfg, err := h.db.IPv6EgressConfig(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "无法读取 IPv6 出口配置"})
		return
	}
	local, err := egressipv6.Discover()
	discoveryError := err != nil
	if discoveryError {
		local = []string{}
	}
	bindings, cooldowns, err := h.db.IPv6EgressStatus(c.Request.Context(), time.Now().Unix())
	if err != nil {
		c.JSON(500, gin.H{"error": "无法读取 IPv6 出口状态"})
		return
	}
	names := map[int64]string{}
	for _, a := range h.store.Accounts() {
		a.Mu().RLock()
		name := a.Email
		a.Mu().RUnlock()
		names[a.ID()] = name
	}
	c.JSON(200, gin.H{"discovery_error": discoveryError, "account_names": names, "config": cfg, "local_ips": local, "bindings": bindings, "cooldowns": cooldowns, "resin_enabled": proxy.IsResinEnabled()})
}

func (h *Handler) UpdateIPv6Egress(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)
	var cfg database.IPv6EgressConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(400, gin.H{"error": "无效的 IPv6 出口配置"})
		return
	}
	if cfg.Enabled && proxy.IsResinEnabled() {
		c.JSON(400, gin.H{"error": "请先关闭 Resin，避免出口设置互相覆盖"})
		return
	}
	var err error
	if cfg.Enabled {
		local, discoverErr := egressipv6.Discover()
		if discoverErr != nil {
			c.JSON(500, gin.H{"error": "无法读取主机 IPv6 地址"})
			return
		}
		if len(local) == 0 {
			c.JSON(400, gin.H{"error": "容器内没有可用的公网 IPv6 地址，请先配置主机网络"})
			return
		}
		cfg.SourceIPs, err = egressipv6.ValidateSources(cfg.SourceIPs, local)
		if err != nil {
			c.JSON(400, gin.H{"error": "地址池只能包含当前容器可见的主机公网 IPv6 地址"})
			return
		}
	}
	if cfg.CooldownSeconds < 30 || cfg.CooldownSeconds > 86400 || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 5 {
		c.JSON(400, gin.H{"error": "冷却时间需为 30–86400 秒，每次请求最多尝试 1–5 个地址"})
		return
	}
	if err = h.db.SaveIPv6EgressConfig(c.Request.Context(), cfg); err != nil {
		status := 500
		if errors.Is(err, database.ErrIPv6ConfigConflict) {
			status = 409
		}
		c.JSON(status, gin.H{"error": "IPv6 配置保存失败，请刷新后重试"})
		return
	}
	if m := egressipv6.Current(); m != nil {
		_ = m.Refresh(c.Request.Context())
	}
	h.GetIPv6Egress(c)
}
