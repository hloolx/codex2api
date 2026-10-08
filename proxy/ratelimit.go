package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// ============ 核心数据结构 ============

// RateLimitLevel 限流级别
type RateLimitLevel int

const (
	LevelGlobal  RateLimitLevel = 0 // 全局限流
	LevelAccount RateLimitLevel = 1 // 账号级限流
	LevelModel   RateLimitLevel = 2 // 模型级限流
)

// RateLimitStatus 限流状态
type RateLimitStatus int

const (
	StatusAllow RateLimitStatus = 0 // 允许通过
	StatusDeny  RateLimitStatus = 1 // 拒绝请求
)

// LimitSnapshot 限流快照（用于持久化）
type LimitSnapshot struct {
	Key        string    `json:"key"`         // 限流键
	Level      string    `json:"level"`       // 级别: global/account/model
	CurrentRPM int64     `json:"current_rpm"` // 当前RPM
	LimitRPM   int64     `json:"limit_rpm"`   // 限制RPM
	Blocked    bool      `json:"blocked"`     // 是否被阻塞
	BlockedAt  time.Time `json:"blocked_at"`  // 阻塞时间
	ResetAt    time.Time `json:"reset_at"`    // 重置时间
}

// LimitMetrics 限流指标
type LimitMetrics struct {
	TotalRequests   int64     `json:"total_requests"`   // 总请求数
	AllowedRequests int64     `json:"allowed_requests"` // 允许请求数
	BlockedRequests int64     `json:"blocked_requests"` // 阻塞请求数
	CurrentRPM      int64     `json:"current_rpm"`      // 当前RPM
	LimitRPM        int64     `json:"limit_rpm"`        // 限制RPM
	CooldownLevel   int       `json:"cooldown_level"`   // 当前冷却等级
	NextResetAt     time.Time `json:"next_reset_at"`    // 下次重置时间
	LastUpdatedAt   time.Time `json:"last_updated_at"`  // 最后更新时间
}

// ============ 令牌桶限流器 ============

// tokenBucket 令牌桶限流器
type tokenBucket struct {
	mu         sync.RWMutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // 每秒补充的令牌数
	lastRefill time.Time
}

func newTokenBucket(rpm int) *tokenBucket {
	rps := float64(rpm) / 60.0
	return &tokenBucket{
		tokens:     float64(rpm),
		maxTokens:  float64(rpm),
		refillRate: rps,
		lastRefill: time.Now(),
	}
}

// allow 尝试获取一个令牌
func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = now

	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// allowN 尝试获取N个令牌
func (tb *tokenBucket) allowN(n int) bool {
	if n <= 0 {
		return true
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = now

	if tb.tokens >= float64(n) {
		tb.tokens -= float64(n)
		return true
	}
	return false
}

// getRate 获取当前速率
func (tb *tokenBucket) getRate() float64 {
	tb.mu.RLock()
	defer tb.mu.RUnlock()
	return tb.refillRate * 60 // 转换为 RPM
}

// getTokens 获取当前令牌数
func (tb *tokenBucket) getTokens() float64 {
	tb.mu.RLock()
	defer tb.mu.RUnlock()

	elapsed := time.Since(tb.lastRefill).Seconds()
	tokens := tb.tokens + elapsed*tb.refillRate
	if tokens > tb.maxTokens {
		tokens = tb.maxTokens
	}
	return tokens
}

// updateRPM 动态更新RPM
func (tb *tokenBucket) updateRPM(rpm int) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	rps := float64(rpm) / 60.0
	// 先保存旧值用于计算比例
	oldMaxTokens := tb.maxTokens
	tb.maxTokens = float64(rpm)
	tb.refillRate = rps
	// 保持当前令牌比例
	if oldMaxTokens > 0 {
		tb.tokens = (tb.tokens / oldMaxTokens) * float64(rpm)
	} else {
		tb.tokens = float64(rpm)
	}
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = time.Now()
}

// ============ 智能冷却策略 ============

// cooldownManager 智能冷却管理器
type cooldownManager struct {
	mu          sync.RWMutex
	level       int           // 当前冷却等级
	blockedAt   time.Time     // 阻塞开始时间
	resetAt     time.Time     // 预计重置时间
	lastBackoff time.Duration // 上次退避时间
}

// cooldownDurations 冷却时间配置（指数退避）
var cooldownDurations = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	64 * time.Second,
	128 * time.Second,
	256 * time.Second,
	512 * time.Second,
	1024 * time.Second, // ~17min
	1800 * time.Second, // 30min (max)
}

// newCooldownManager 创建冷却管理器
func newCooldownManager() *cooldownManager {
	return &cooldownManager{
		level: -1, // 初始无冷却
	}
}

// computeCooldown 计算下次冷却时间和等级（参考CPA的backoff机制）
// 返回值: (冷却时长, 新等级)
func (cm *cooldownManager) computeCooldown() (time.Duration, int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	newLevel := cm.level + 1
	if newLevel >= len(cooldownDurations) {
		newLevel = len(cooldownDurations) - 1
	}

	duration := cooldownDurations[newLevel]
	return duration, newLevel
}

// enterCooldown 进入冷却状态
func (cm *cooldownManager) enterCooldown() time.Duration {
	duration, level := cm.computeCooldown()

	cm.mu.Lock()
	cm.level = level
	cm.blockedAt = time.Now()
	cm.resetAt = time.Now().Add(duration)
	cm.lastBackoff = duration
	cm.mu.Unlock()

	return duration
}

// reset 重置冷却状态
func (cm *cooldownManager) reset() {
	cm.mu.Lock()
	cm.level = -1
	cm.blockedAt = time.Time{}
	cm.resetAt = time.Time{}
	cm.lastBackoff = 0
	cm.mu.Unlock()
}

// isInCooldown 检查是否在冷却中
func (cm *cooldownManager) isInCooldown() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.level < 0 {
		return false
	}
	return time.Now().Before(cm.resetAt)
}

// getRemainingCooldown 获取剩余冷却时间
func (cm *cooldownManager) getRemainingCooldown() time.Duration {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.level < 0 {
		return 0
	}
	remaining := time.Until(cm.resetAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// getState 获取冷却状态
func (cm *cooldownManager) getState() (level int, blockedAt time.Time, resetAt time.Time) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.level, cm.blockedAt, cm.resetAt
}

// ============ 多级限流器 ============

// LevelLimiter 单级别限流器
type LevelLimiter struct {
	mu         sync.RWMutex
	key        string           // 限流键
	level      RateLimitLevel   // 限流级别
	bucket     *tokenBucket     // 令牌桶
	cooldown   *cooldownManager // 冷却管理器
	metrics    LimitMetrics     // 指标
	enabled    bool             // 是否启用
	lastAccess atomic.Int64     // 最后访问时间（用于 TTL 清理）
}

// newLevelLimiter 创建单级别限流器
func newLevelLimiter(key string, level RateLimitLevel, rpm int) *LevelLimiter {
	ll := &LevelLimiter{
		key:      key,
		level:    level,
		enabled:  rpm > 0,
		cooldown: newCooldownManager(),
	}
	ll.lastAccess.Store(time.Now().UnixNano())
	if ll.enabled {
		ll.bucket = newTokenBucket(rpm)
		ll.metrics.LimitRPM = int64(rpm)
	}
	return ll
}

// allow 尝试获取令牌
func (ll *LevelLimiter) allow() bool {
	ll.lastAccess.Store(time.Now().UnixNano())
	ll.mu.Lock()
	defer ll.mu.Unlock()

	// 在函数入口递增总请求数
	ll.metrics.TotalRequests++
	ll.metrics.LastUpdatedAt = time.Now()

	if !ll.enabled {
		return true
	}

	// 检查是否在冷却中
	if ll.cooldown.isInCooldown() {
		ll.metrics.BlockedRequests++
		return false
	}

	// 尝试获取令牌
	if ll.bucket.allow() {
		ll.metrics.AllowedRequests++
		ll.metrics.CurrentRPM = int64(ll.bucket.getRate())
		return true
	}

	// 令牌不足，进入冷却
	ll.enterCooldown()
	ll.metrics.BlockedRequests++
	return false
}

// allowN 尝试获取N个令牌
func (ll *LevelLimiter) allowN(n int) bool {
	ll.lastAccess.Store(time.Now().UnixNano())
	ll.mu.Lock()
	defer ll.mu.Unlock()

	// 在函数入口递增总请求数
	ll.metrics.TotalRequests += int64(n)
	ll.metrics.LastUpdatedAt = time.Now()

	if !ll.enabled {
		return true
	}

	if ll.cooldown.isInCooldown() {
		ll.metrics.BlockedRequests += int64(n)
		return false
	}

	if ll.bucket.allowN(n) {
		ll.metrics.AllowedRequests += int64(n)
		return true
	}

	ll.enterCooldown()
	ll.metrics.BlockedRequests += int64(n)
	return false
}

// enterCooldown 进入冷却状态
func (ll *LevelLimiter) enterCooldown() time.Duration {
	duration := ll.cooldown.enterCooldown()
	ll.metrics.CooldownLevel, _, ll.metrics.NextResetAt = ll.cooldown.getState()
	return duration
}

// resetCooldown 重置冷却状态
func (ll *LevelLimiter) resetCooldown() {
	ll.cooldown.reset()
	ll.metrics.CooldownLevel = -1
	ll.metrics.NextResetAt = time.Time{}
}

// updateRPM 动态更新RPM
func (ll *LevelLimiter) updateRPM(rpm int) {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	ll.enabled = rpm > 0
	ll.metrics.LimitRPM = int64(rpm)

	if ll.enabled {
		if ll.bucket == nil {
			ll.bucket = newTokenBucket(rpm)
		} else {
			ll.bucket.updateRPM(rpm)
		}
	} else {
		ll.bucket = nil
	}
}

// getMetrics 获取指标
func (ll *LevelLimiter) getMetrics() LimitMetrics {
	ll.mu.RLock()
	defer ll.mu.RUnlock()

	// 构造拷贝避免数据竞争
	metrics := ll.metrics
	if ll.bucket != nil {
		metrics.CurrentRPM = int64(ll.bucket.getRate())
	}
	metrics.CooldownLevel, _, metrics.NextResetAt = ll.cooldown.getState()
	return metrics
}

// getSnapshot 获取快照
func (ll *LevelLimiter) getSnapshot() LimitSnapshot {
	ll.mu.RLock()
	defer ll.mu.RUnlock()

	levelStr := "unknown"
	switch ll.level {
	case LevelGlobal:
		levelStr = "global"
	case LevelAccount:
		levelStr = "account"
	case LevelModel:
		levelStr = "model"
	}

	level, blockedAt, resetAt := ll.cooldown.getState()
	return LimitSnapshot{
		Key:        ll.key,
		Level:      levelStr,
		CurrentRPM: ll.metrics.CurrentRPM,
		LimitRPM:   ll.metrics.LimitRPM,
		Blocked:    level >= 0,
		BlockedAt:  blockedAt,
		ResetAt:    resetAt,
	}
}

// ============ 增强型限流管理器 ============

// EnhancedRateLimiter 增强型多级限流器
type EnhancedRateLimiter struct {
	mu sync.RWMutex

	// 各级别限流器
	globalLimiter   *LevelLimiter            // 全局限流
	accountLimiters map[string]*LevelLimiter // 账号级限流器 (key: accountID)
	modelLimiters   map[string]*LevelLimiter // 模型级限流器 (key: modelName)

	// 配置
	globalRPM  int // 全局RPM限制
	accountRPM int // 每账号RPM限制
	modelRPM   int // 每模型RPM限制
	enabled    bool

	// 持久化
	db              *database.DB
	persistInterval time.Duration
	stopCh          chan struct{}
	stopOnce        sync.Once // 保护 stopCh 只被关闭一次

	// 指标收集
	metricsEnabled bool
	totalLimited   atomic.Int64 // 被限流的总请求数
}

// NewEnhancedRateLimiter 创建增强型限流器
func NewEnhancedRateLimiter(db *database.DB, globalRPM, accountRPM, modelRPM int) *EnhancedRateLimiter {
	erl := &EnhancedRateLimiter{
		globalLimiter:   newLevelLimiter("global", LevelGlobal, globalRPM),
		accountLimiters: make(map[string]*LevelLimiter),
		modelLimiters:   make(map[string]*LevelLimiter),
		db:              db,
		globalRPM:       globalRPM,
		accountRPM:      accountRPM,
		modelRPM:        modelRPM,
		enabled:         globalRPM > 0 || accountRPM > 0 || modelRPM > 0,
		persistInterval: 30 * time.Second,
		stopCh:          make(chan struct{}),
		metricsEnabled:  true,
	}

	// 启动持久化协程
	if db != nil {
		go erl.persistenceLoop()
	}

	// 启动 limiter 清理协程（无论是否有 db 都需要）
	go erl.limiterEvictionLoop()

	return erl
}

// Allow 检查请求是否允许通过（全局限流）
func (erl *EnhancedRateLimiter) Allow() bool {
	if !erl.enabled {
		return true
	}
	return erl.globalLimiter.allow()
}

// AllowWithContext 检查请求是否允许通过（带上下文的多级限流）
func (erl *EnhancedRateLimiter) AllowWithContext(accountID, model string) bool {
	if !erl.enabled {
		return true
	}

	erl.mu.RLock()
	defer erl.mu.RUnlock()

	// 1. 检查全局限流
	if !erl.globalLimiter.allow() {
		erl.totalLimited.Add(1)
		return false
	}

	// 2. 检查账号级限流
	if erl.accountRPM > 0 && accountID != "" {
		accLimiter, exists := erl.accountLimiters[accountID]
		if !exists {
			// 需要升级锁来创建新的限流器
			erl.mu.RUnlock()
			erl.mu.Lock()
			accLimiter, exists = erl.accountLimiters[accountID]
			if !exists {
				accLimiter = newLevelLimiter(accountID, LevelAccount, erl.accountRPM)
				erl.accountLimiters[accountID] = accLimiter
			}
			erl.mu.Unlock()
			erl.mu.RLock()
		}
		if !accLimiter.allow() {
			erl.totalLimited.Add(1)
			return false
		}
	}

	// 3. 检查模型级限流
	if erl.modelRPM > 0 && model != "" {
		modelLimiter, exists := erl.modelLimiters[model]
		if !exists {
			// 需要升级锁来创建新的限流器
			erl.mu.RUnlock()
			erl.mu.Lock()
			modelLimiter, exists = erl.modelLimiters[model]
			if !exists {
				modelLimiter = newLevelLimiter(model, LevelModel, erl.modelRPM)
				erl.modelLimiters[model] = modelLimiter
			}
			erl.mu.Unlock()
			erl.mu.RLock()
		}
		if !modelLimiter.allow() {
			erl.totalLimited.Add(1)
			return false
		}
	}

	return true
}

// UpdateGlobalRPM 动态更新全局限流
func (erl *EnhancedRateLimiter) UpdateGlobalRPM(rpm int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.globalRPM = rpm
	erl.globalLimiter.updateRPM(rpm)
	erl.enabled = erl.globalRPM > 0 || erl.accountRPM > 0 || erl.modelRPM > 0
}

// UpdateAccountRPM 动态更新账号级限流
func (erl *EnhancedRateLimiter) UpdateAccountRPM(rpm int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.accountRPM = rpm
	// 更新所有已有账号限流器
	for _, limiter := range erl.accountLimiters {
		limiter.updateRPM(rpm)
	}
	erl.enabled = erl.globalRPM > 0 || erl.accountRPM > 0 || erl.modelRPM > 0
}

// UpdateModelRPM 动态更新模型级限流
func (erl *EnhancedRateLimiter) UpdateModelRPM(rpm int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.modelRPM = rpm
	// 更新所有已有模型限流器
	for _, limiter := range erl.modelLimiters {
		limiter.updateRPM(rpm)
	}
	erl.enabled = erl.globalRPM > 0 || erl.accountRPM > 0 || erl.modelRPM > 0
}

// UpdateAllRPM 动态更新所有限流级别
func (erl *EnhancedRateLimiter) UpdateAllRPM(globalRPM, accountRPM, modelRPM int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.globalRPM = globalRPM
	erl.accountRPM = accountRPM
	erl.modelRPM = modelRPM

	erl.globalLimiter.updateRPM(globalRPM)
	for _, limiter := range erl.accountLimiters {
		limiter.updateRPM(accountRPM)
	}
	for _, limiter := range erl.modelLimiters {
		limiter.updateRPM(modelRPM)
	}
	erl.enabled = globalRPM > 0 || accountRPM > 0 || modelRPM > 0
}

// GetGlobalMetrics 获取全局限流指标
func (erl *EnhancedRateLimiter) GetGlobalMetrics() LimitMetrics {
	return erl.globalLimiter.getMetrics()
}

// GetAccountMetrics 获取账号限流指标
func (erl *EnhancedRateLimiter) GetAccountMetrics(accountID string) LimitMetrics {
	erl.mu.RLock()
	limiter, exists := erl.accountLimiters[accountID]
	erl.mu.RUnlock()

	if !exists {
		return LimitMetrics{}
	}
	return limiter.getMetrics()
}

// GetModelMetrics 获取模型限流指标
func (erl *EnhancedRateLimiter) GetModelMetrics(model string) LimitMetrics {
	erl.mu.RLock()
	limiter, exists := erl.modelLimiters[model]
	erl.mu.RUnlock()

	if !exists {
		return LimitMetrics{}
	}
	return limiter.getMetrics()
}

// GetAllMetrics 获取所有限流指标
func (erl *EnhancedRateLimiter) GetAllMetrics() map[string]interface{} {
	erl.mu.RLock()
	defer erl.mu.RUnlock()

	result := map[string]interface{}{
		"global":        erl.globalLimiter.getMetrics(),
		"total_limited": erl.totalLimited.Load(),
	}

	accountMetrics := make(map[string]LimitMetrics)
	for id, limiter := range erl.accountLimiters {
		accountMetrics[id] = limiter.getMetrics()
	}
	result["accounts"] = accountMetrics

	modelMetrics := make(map[string]LimitMetrics)
	for model, limiter := range erl.modelLimiters {
		modelMetrics[model] = limiter.getMetrics()
	}
	result["models"] = modelMetrics

	return result
}

// GetAllSnapshots 获取所有限流快照（用于持久化）
func (erl *EnhancedRateLimiter) GetAllSnapshots() []LimitSnapshot {
	erl.mu.RLock()
	defer erl.mu.RUnlock()

	snapshots := []LimitSnapshot{erl.globalLimiter.getSnapshot()}

	for _, limiter := range erl.accountLimiters {
		snapshots = append(snapshots, limiter.getSnapshot())
	}

	for _, limiter := range erl.modelLimiters {
		snapshots = append(snapshots, limiter.getSnapshot())
	}

	return snapshots
}

// persistenceLoop 持久化循环
func (erl *EnhancedRateLimiter) persistenceLoop() {
	ticker := time.NewTicker(erl.persistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			erl.persistSnapshots()
		case <-erl.stopCh:
			return
		}
	}
}

// limiterEvictionLoop 清理长时间未使用的限流器（30 分钟 TTL）
func (erl *EnhancedRateLimiter) limiterEvictionLoop() {
	const (
		evictionTTL      = 30 * time.Minute
		evictionInterval = 5 * time.Minute
	)

	ticker := time.NewTicker(evictionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			erl.evictStaleLimiters(evictionTTL)
		case <-erl.stopCh:
			return
		}
	}
}

// evictStaleLimiters 清理超过 TTL 未访问的限流器
func (erl *EnhancedRateLimiter) evictStaleLimiters(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl).UnixNano()

	erl.mu.Lock()
	defer erl.mu.Unlock()

	for key, limiter := range erl.accountLimiters {
		if limiter.lastAccess.Load() < cutoff {
			delete(erl.accountLimiters, key)
		}
	}

	for key, limiter := range erl.modelLimiters {
		if limiter.lastAccess.Load() < cutoff {
			delete(erl.modelLimiters, key)
		}
	}
}

// persistSnapshots 持久化快照到数据库
func (erl *EnhancedRateLimiter) persistSnapshots() {
	// 当前实现仅获取快照但不实际持久化
	// 如需持久化，请在这里实现具体的存储逻辑
	if erl.db == nil {
		return
	}

	snapshots := erl.GetAllSnapshots()
	if len(snapshots) == 0 {
		return
	}

	// 存储到数据库（使用 system_settings 表存储）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 这里我们使用一个简单的键值存储方式
	// 实际项目中可以创建专门的限流状态表
	_, _ = erl.db.GetSystemSettings(ctx)
	// 注：限流状态作为瞬态数据，通常不需要严格持久化
	// 这里仅作演示，实际可根据需求决定是否持久化
}

// Stop 停止限流器
func (erl *EnhancedRateLimiter) Stop() {
	erl.stopOnce.Do(func() {
		close(erl.stopCh)
	})
}

// ComputeCooldown 计算冷却时间（参考CPA的nextQuotaCooldown）
// 指数退避: 1s, 2s, 4s, 8s, ... max 30min
func ComputeCooldown(prevLevel int) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = -1
	}
	newLevel := prevLevel + 1
	if newLevel >= len(cooldownDurations) {
		newLevel = len(cooldownDurations) - 1
	}
	return cooldownDurations[newLevel], newLevel
}

// ============ 兼容旧版接口 ============

const (
	requestRateBucketSeconds = int64(5)
	requestRateBucketCount   = 12
)

// requestRateMeter 用 12 个 5 秒原子桶记录最近一分钟代理请求数。
// 请求热路径只有一次 CAS，不依赖全局 RPM 限流是否开启，也不增加互斥锁。
type requestRateMeter struct {
	buckets [requestRateBucketCount]atomic.Uint64
}

func (m *requestRateMeter) mark(now time.Time) {
	if m == nil {
		return
	}
	slot := uint32(now.Unix() / requestRateBucketSeconds)
	bucket := &m.buckets[slot%requestRateBucketCount]
	for {
		old := bucket.Load()
		oldSlot := uint32(old >> 32)
		oldCount := uint32(old)
		nextCount := uint32(1)
		if oldSlot == slot {
			if oldCount == ^uint32(0) {
				return
			}
			nextCount = oldCount + 1
		}
		next := uint64(slot)<<32 | uint64(nextCount)
		if bucket.CompareAndSwap(old, next) {
			return
		}
	}
}

func (m *requestRateMeter) rpm(now time.Time) int64 {
	if m == nil {
		return 0
	}
	currentSlot := uint32(now.Unix() / requestRateBucketSeconds)
	var total uint64
	for i := range m.buckets {
		packed := m.buckets[i].Load()
		slot := uint32(packed >> 32)
		if currentSlot-slot < requestRateBucketCount {
			total += uint64(uint32(packed))
		}
	}
	if total > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(total)
}

// RateLimiter 全局限流器（向后兼容）
type RateLimiter struct {
	enhanced *EnhancedRateLimiter
	traffic  requestRateMeter
	active   atomic.Int64
}

// NewRateLimiter 创建限流器（向后兼容）
func NewRateLimiter(rpm int) *RateLimiter {
	return &RateLimiter{
		enhanced: NewEnhancedRateLimiter(nil, rpm, 0, 0),
	}
}

// UpdateRPM 动态更新 RPM 限制（向后兼容）
func (rl *RateLimiter) UpdateRPM(rpm int) {
	if rl.enhanced != nil {
		rl.enhanced.UpdateGlobalRPM(rpm)
	}
}

// GetRPM 获取当前 RPM（向后兼容）
func (rl *RateLimiter) GetRPM() int {
	if rl.enhanced != nil {
		return rl.enhanced.globalRPM
	}
	return 0
}

// GetCurrentRPM 返回最近 60 秒实际进入代理路由的请求数。
func (rl *RateLimiter) GetCurrentRPM() int64 {
	if rl == nil {
		return 0
	}
	return rl.traffic.rpm(time.Now())
}

// GetActiveRequests 返回当前仍在处理的代理请求数（包括长连接和流式响应）。
func (rl *RateLimiter) GetActiveRequests() int64 {
	if rl == nil {
		return 0
	}
	return rl.active.Load()
}

// Middleware 返回 Gin 中间件（向后兼容）
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		// 只限制代理请求，不限制管理后台和健康检查
		if path == "/health" ||
			(len(path) >= 4 && path[:4] == "/api") ||
			(len(path) >= 6 && path[:6] == "/admin") {
			c.Next()
			return
		}

		rl.traffic.mark(time.Now())
		rl.active.Add(1)
		defer rl.active.Add(-1)
		if rl.enhanced != nil && !rl.enhanced.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"message": "请求过于频繁，请稍后重试",
					"type":    "rate_limit_error",
					"code":    "rate_limit_exceeded",
				},
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// GetEnhancedLimiter 获取增强型限流器
func (rl *RateLimiter) GetEnhancedLimiter() *EnhancedRateLimiter {
	return rl.enhanced
}

// ============ 限流错误 ============

// RateLimitError 限流错误
type RateLimitError struct {
	Level      RateLimitLevel
	Key        string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	levelStr := "unknown"
	switch e.Level {
	case LevelGlobal:
		levelStr = "global"
	case LevelAccount:
		levelStr = "account"
	case LevelModel:
		levelStr = "model"
	}
	return fmt.Sprintf("rate limit exceeded: level=%s, key=%s, retry_after=%v", levelStr, e.Key, e.RetryAfter)
}

// HTTPStatusCode 返回HTTP状态码
func (e *RateLimitError) HTTPStatusCode() int {
	return http.StatusTooManyRequests
}
