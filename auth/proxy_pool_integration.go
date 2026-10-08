package auth

import (
	"context"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

// EnhancedProxyPool 增强的代理池管理器
type EnhancedProxyPool struct {
	pool        *ProxyPool
	db          *database.DB
	enabled     atomic.Bool
	initialized atomic.Bool
}

// NewEnhancedProxyPool 创建增强代理池
func NewEnhancedProxyPool(db *database.DB, config *ProxyPoolConfig) *EnhancedProxyPool {
	return &EnhancedProxyPool{
		pool: NewProxyPool(config),
		db:   db,
	}
}

// Init 初始化代理池
func (e *EnhancedProxyPool) Init(ctx context.Context) error {
	if e.db == nil {
		return nil
	}

	// 从数据库加载代理
	proxies, err := e.db.ListEnabledProxies(ctx)
	if err != nil {
		return err
	}

	// 添加到代理池
	for _, p := range proxies {
		// 使用延迟作为权重的基础（延迟越低，权重越高）
		weight := int64(100)
		if p.TestLatencyMs > 0 {
			// 延迟越小，权重越高
			weight = int64(10000 / (p.TestLatencyMs + 1))
			if weight < 1 {
				weight = 1
			}
		}
		e.pool.AddProxy(p.URL, weight)
	}

	// 设置回调
	e.pool.SetOnIsolation(func(entry *ProxyEntry) {
		log.Printf("[ProxyPool] 代理被隔离: %s (连续失败 %d 次)", entry.URL, entry.ConsecutiveFailures)
	})

	e.pool.SetOnRecovery(func(entry *ProxyEntry) {
		log.Printf("[ProxyPool] 代理已恢复: %s", entry.URL)
	})

	e.pool.SetOnHealthCheck(func(result *HealthCheckResult) {
		if !result.Healthy && result.Error != nil {
			log.Printf("[ProxyPool] 代理健康检查失败: %s, error: %v", result.URL, result.Error)
		}
	})

	// 启动健康检查
	e.pool.StartHealthCheck()
	e.enabled.Store(true)
	e.initialized.Store(true)

	log.Printf("[ProxyPool] 增强代理池已初始化，共 %d 个代理", len(proxies))
	return nil
}

// Enabled 返回代理池是否启用
func (e *EnhancedProxyPool) Enabled() bool {
	return e.enabled.Load()
}

// SetEnabled 设置代理池启用状态
func (e *EnhancedProxyPool) SetEnabled(enabled bool) {
	e.enabled.Store(enabled)
}

// NextProxy 获取下一个代理（兼容原有接口）
func (e *EnhancedProxyPool) NextProxy() string {
	if !e.enabled.Load() || !e.initialized.Load() {
		return ""
	}

	entry := e.pool.Select()
	if entry == nil {
		return ""
	}
	return entry.URL
}

// SelectWithStrategy 使用指定策略选择代理
func (e *EnhancedProxyPool) SelectWithStrategy(strategy ProxySelectionStrategy) *ProxyEntry {
	if !e.enabled.Load() || !e.initialized.Load() {
		return nil
	}

	return e.pool.SelectWithStrategy(strategy)
}

// MarkProxySuccess 标记代理成功
func (e *EnhancedProxyPool) MarkProxySuccess(url string) {
	if !e.initialized.Load() {
		return
	}
	e.pool.MarkSuccess(url)
}

// MarkProxyFailure 标记代理失败
func (e *EnhancedProxyPool) MarkProxyFailure(url string) {
	if !e.initialized.Load() {
		return
	}
	e.pool.MarkFailure(url)
}

// AcquireConnection 获取代理连接
func (e *EnhancedProxyPool) AcquireConnection(url string) bool {
	if !e.initialized.Load() {
		return false
	}
	return e.pool.AcquireConnection(url)
}

// ReleaseConnection 释放代理连接
func (e *EnhancedProxyPool) ReleaseConnection(url string) {
	if !e.initialized.Load() {
		return
	}
	e.pool.ReleaseConnection(url)
}

// ReloadFromDB 从数据库重新加载代理
func (e *EnhancedProxyPool) ReloadFromDB(ctx context.Context) error {
	if e.db == nil {
		return nil
	}

	proxies, err := e.db.ListEnabledProxies(ctx)
	if err != nil {
		return err
	}

	// 获取当前所有代理
	currentProxies := e.pool.GetAllProxies()
	currentMap := make(map[string]bool)
	for _, url := range currentProxies {
		currentMap[url] = true
	}

	// 添加新代理
	for _, p := range proxies {
		if !currentMap[p.URL] {
			weight := int64(100)
			if p.TestLatencyMs > 0 {
				weight = int64(10000 / (p.TestLatencyMs + 1))
				if weight < 1 {
					weight = 1
				}
			}
			e.pool.AddProxy(p.URL, weight)
			log.Printf("[ProxyPool] 新增代理: %s", p.URL)
		}
	}

	// 移除已禁用的代理
	newMap := make(map[string]bool)
	for _, p := range proxies {
		newMap[p.URL] = true
	}
	for _, url := range currentProxies {
		if !newMap[url] {
			e.pool.RemoveProxy(url)
			log.Printf("[ProxyPool] 移除代理: %s", url)
		}
	}

	return nil
}

// GetStats 获取代理统计信息
func (e *EnhancedProxyPool) GetStats() map[string]*ProxyStats {
	if !e.initialized.Load() {
		return make(map[string]*ProxyStats)
	}
	return e.pool.GetStats()
}

// GetPoolStatus 获取代理池状态
func (e *EnhancedProxyPool) GetPoolStatus() *ProxyPoolStatus {
	if !e.initialized.Load() {
		return &ProxyPoolStatus{
			Total:    0,
			Healthy:  0,
			Strategy: StrategyRoundRobin.String(),
		}
	}
	return e.pool.GetPoolStatus()
}

// SetStrategy 设置选择策略
func (e *EnhancedProxyPool) SetStrategy(strategy ProxySelectionStrategy) {
	if !e.initialized.Load() {
		return
	}
	e.pool.SetStrategy(strategy)
}

// ForceHealthCheck 强制健康检查
func (e *EnhancedProxyPool) ForceHealthCheck(ctx context.Context) {
	if !e.initialized.Load() {
		return
	}
	e.pool.ForceHealthCheck(ctx)
}

// Stop 停止代理池
func (e *EnhancedProxyPool) Stop() {
	if !e.initialized.Load() {
		return
	}
	e.pool.Stop()
	e.enabled.Store(false)
}

// GetHealthyProxies 获取健康代理列表
func (e *EnhancedProxyPool) GetHealthyProxies() []string {
	if !e.initialized.Load() {
		return []string{}
	}
	return e.pool.GetHealthyProxies()
}

// HasHealthyProxy 检查是否有健康代理
func (e *EnhancedProxyPool) HasHealthyProxy() bool {
	if !e.initialized.Load() {
		return false
	}
	return e.pool.HasHealthyProxy()
}

// UpdateProxyStats 更新代理统计数据到数据库
func (e *EnhancedProxyPool) UpdateProxyStats(ctx context.Context) error {
	if e.db == nil || !e.initialized.Load() {
		return nil
	}

	stats := e.pool.GetStats()
	for url, stat := range stats {
		// 这里可以扩展数据库模型来存储更详细的统计信息
		_ = url
		_ = stat
		// TODO: 实现数据库更新逻辑
	}
	return nil
}

// SetProxyWeight 设置代理权重
func (e *EnhancedProxyPool) SetProxyWeight(url string, weight int64) {
	if !e.initialized.Load() {
		return
	}
	e.pool.UpdateProxyWeight(url, weight)
}

// StoreProxyPoolIntegration Store 与代理池集成
type StoreProxyPoolIntegration struct {
	store        *Store
	enhancedPool *EnhancedProxyPool
	useEnhanced  atomic.Bool
}

// NewStoreProxyPoolIntegration 创建 Store 与代理池集成
func NewStoreProxyPoolIntegration(store *Store, db *database.DB, settings *database.SystemSettings) *StoreProxyPoolIntegration {
	integration := &StoreProxyPoolIntegration{
		store: store,
	}

	// 如果启用代理池，创建增强代理池
	if settings != nil && settings.ProxyPoolEnabled {
		config := &ProxyPoolConfig{
			Strategy:           ParseStrategy(getEnv("PROXY_POOL_STRATEGY", "round_robin")),
			CheckInterval:      parseDuration(getEnv("PROXY_POOL_CHECK_INTERVAL", "30s"), 30*time.Second),
			Timeout:            parseDuration(getEnv("PROXY_POOL_TIMEOUT", "10s"), 10*time.Second),
			IsolationThreshold: parseInt(getEnv("PROXY_POOL_ISOLATION_THRESHOLD", "3"), 3),
			IsolationDuration:  parseDuration(getEnv("PROXY_POOL_ISOLATION_DURATION", "5m"), 5*time.Minute),
			HealthCheckURL:     getEnv("PROXY_POOL_HEALTH_CHECK_URL", "http://www.google.com/generate_204"),
		}

		integration.enhancedPool = NewEnhancedProxyPool(db, config)
		integration.useEnhanced.Store(true)
	}

	return integration
}

// Init 初始化
func (s *StoreProxyPoolIntegration) Init(ctx context.Context) error {
	if s.enhancedPool != nil {
		return s.enhancedPool.Init(ctx)
	}
	return nil
}

// NextProxy 获取下一个代理
func (s *StoreProxyPoolIntegration) NextProxy() string {
	if s.useEnhanced.Load() && s.enhancedPool != nil && s.enhancedPool.Enabled() {
		return s.enhancedPool.NextProxy()
	}
	// 回退到原有实现
	if s.store != nil {
		return s.store.NextProxy()
	}
	return ""
}

// MarkProxySuccess 标记代理成功
func (s *StoreProxyPoolIntegration) MarkProxySuccess(url string) {
	if s.useEnhanced.Load() && s.enhancedPool != nil {
		s.enhancedPool.MarkProxySuccess(url)
	}
}

// MarkProxyFailure 标记代理失败
func (s *StoreProxyPoolIntegration) MarkProxyFailure(url string) {
	if s.useEnhanced.Load() && s.enhancedPool != nil {
		s.enhancedPool.MarkProxyFailure(url)
	}
}

// GetEnhancedPool 获取增强代理池
func (s *StoreProxyPoolIntegration) GetEnhancedPool() *EnhancedProxyPool {
	return s.enhancedPool
}

// UseEnhanced 是否使用增强代理池
func (s *StoreProxyPoolIntegration) UseEnhanced() bool {
	return s.useEnhanced.Load()
}

// SetUseEnhanced 设置是否使用增强代理池
func (s *StoreProxyPoolIntegration) SetUseEnhanced(use bool) {
	s.useEnhanced.Store(use)
}

// Stop 停止
func (s *StoreProxyPoolIntegration) Stop() {
	if s.enhancedPool != nil {
		s.enhancedPool.Stop()
	}
}

// 辅助函数
func getEnv(key, defaultValue string) string {
	if value := getEnvFunc(key); value != "" {
		return value
	}
	return defaultValue
}

// 用于测试注入
var getEnvFunc = os.Getenv

func parseDuration(s string, defaultValue time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultValue
	}
	return d
}

func parseInt(s string, defaultValue int) int {
	var result int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			result = result*10 + int(c-'0')
		} else {
			break
		}
	}
	if result == 0 {
		return defaultValue
	}
	return result
}
