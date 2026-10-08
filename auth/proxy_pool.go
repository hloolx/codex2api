package auth

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyHealthStatus 代理健康状态
type ProxyHealthStatus int

const (
	ProxyStatusHealthy   ProxyHealthStatus = iota // 健康
	ProxyStatusUnhealthy                          // 不健康
	ProxyStatusIsolated                           // 已隔离
)

// ProxySelectionStrategy 代理选择策略
type ProxySelectionStrategy int

const (
	StrategyRoundRobin       ProxySelectionStrategy = iota // 轮询
	StrategyWeighted                                       // 加权选择
	StrategyLeastConnections                               // 最少连接
)

// ProxyEntry 代理条目
type ProxyEntry struct {
	URL                 string
	Healthy             bool
	LastCheck           time.Time
	Latency             time.Duration
	SuccessRate         float64
	Weight              int64
	ActiveConns         atomic.Int64
	TotalRequests       atomic.Int64
	FailedRequests      atomic.Int64
	Status              ProxyHealthStatus
	IsolatedAt          time.Time
	ConsecutiveFailures int

	mu sync.RWMutex
}

// ProxyStats 代理统计信息
type ProxyStats struct {
	URL                 string        `json:"url"`
	Healthy             bool          `json:"healthy"`
	LastCheck           time.Time     `json:"last_check"`
	Latency             time.Duration `json:"latency"`
	LatencyMs           float64       `json:"latency_ms"`
	SuccessRate         float64       `json:"success_rate"`
	Weight              int64         `json:"weight"`
	ActiveConns         int64         `json:"active_conns"`
	TotalRequests       int64         `json:"total_requests"`
	FailedRequests      int64         `json:"failed_requests"`
	Status              string        `json:"status"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
}

// HealthCheckResult 健康检查结果
type HealthCheckResult struct {
	URL       string
	Healthy   bool
	Latency   time.Duration
	Error     error
	Timestamp time.Time
}

// ProxyPool 代理池管理器
type ProxyPool struct {
	proxies []*ProxyEntry
	stats   map[string]*ProxyStats
	healthy []*ProxyEntry

	mu                 sync.RWMutex
	strategy           ProxySelectionStrategy
	roundRobinIdx      atomic.Uint64
	checkInterval      time.Duration
	timeout            time.Duration
	isolationThreshold int
	isolationDuration  time.Duration
	healthCheckURL     string

	// 运行时状态
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// 回调函数
	onHealthCheck func(result *HealthCheckResult)
	onIsolation   func(entry *ProxyEntry)
	onRecovery    func(entry *ProxyEntry)
}

// ProxyPoolConfig 代理池配置
type ProxyPoolConfig struct {
	Strategy           ProxySelectionStrategy
	CheckInterval      time.Duration
	Timeout            time.Duration
	IsolationThreshold int           // 连续失败次数阈值
	IsolationDuration  time.Duration // 隔离持续时间
	HealthCheckURL     string
}

// DefaultProxyPoolConfig 返回默认配置
func DefaultProxyPoolConfig() *ProxyPoolConfig {
	return &ProxyPoolConfig{
		Strategy:           StrategyRoundRobin,
		CheckInterval:      30 * time.Second,
		Timeout:            10 * time.Second,
		IsolationThreshold: 3,
		IsolationDuration:  5 * time.Minute,
		HealthCheckURL:     "http://www.google.com/generate_204",
	}
}

// NewProxyPool 创建新的代理池
func NewProxyPool(config *ProxyPoolConfig) *ProxyPool {
	if config == nil {
		config = DefaultProxyPoolConfig()
	}

	return &ProxyPool{
		proxies:            make([]*ProxyEntry, 0),
		stats:              make(map[string]*ProxyStats),
		healthy:            make([]*ProxyEntry, 0),
		strategy:           config.Strategy,
		checkInterval:      config.CheckInterval,
		timeout:            config.Timeout,
		isolationThreshold: config.IsolationThreshold,
		isolationDuration:  config.IsolationDuration,
		healthCheckURL:     config.HealthCheckURL,
		stopCh:             make(chan struct{}),
	}
}

// AddProxy 添加代理到池
func (p *ProxyPool) AddProxy(url string, weight int64) {
	if url == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 检查是否已存在
	for _, entry := range p.proxies {
		if entry.URL == url {
			return
		}
	}

	entry := &ProxyEntry{
		URL:         url,
		Healthy:     true,
		LastCheck:   time.Time{},
		Latency:     0,
		SuccessRate: 1.0,
		Weight:      weight,
		Status:      ProxyStatusHealthy,
	}

	p.proxies = append(p.proxies, entry)
	p.stats[url] = &ProxyStats{
		URL:     url,
		Healthy: true,
		Weight:  weight,
		Status:  "healthy",
	}
	p.rebuildHealthyList()
}

// RemoveProxy 从池中移除代理
func (p *ProxyPool) RemoveProxy(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, entry := range p.proxies {
		if entry.URL == url {
			p.proxies = append(p.proxies[:i], p.proxies[i+1:]...)
			delete(p.stats, url)
			p.rebuildHealthyList()
			return
		}
	}
}

// UpdateProxyWeight 更新代理权重
func (p *ProxyPool) UpdateProxyWeight(url string, weight int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.proxies {
		if entry.URL == url {
			entry.Weight = weight
			if stats, ok := p.stats[url]; ok {
				stats.Weight = weight
			}
			return
		}
	}
}

// rebuildHealthyList 重建健康代理列表
func (p *ProxyPool) rebuildHealthyList() {
	healthy := make([]*ProxyEntry, 0, len(p.proxies))
	for _, entry := range p.proxies {
		if entry.Healthy && entry.Status == ProxyStatusHealthy {
			healthy = append(healthy, entry)
		}
	}
	p.healthy = healthy
}

// Select 选择一个代理
func (p *ProxyPool) Select() *ProxyEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.healthy) == 0 {
		return nil
	}

	return p.selectByStrategy(p.strategy)
}

// SelectWithStrategy 使用指定策略选择代理（不修改全局策略）
func (p *ProxyPool) SelectWithStrategy(strategy ProxySelectionStrategy) *ProxyEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.healthy) == 0 {
		return nil
	}

	return p.selectByStrategy(strategy)
}

// selectByStrategy 根据策略选择代理
func (p *ProxyPool) selectByStrategy(strategy ProxySelectionStrategy) *ProxyEntry {
	switch strategy {
	case StrategyWeighted:
		return p.selectWeighted()
	case StrategyLeastConnections:
		return p.selectLeastConnections()
	default: // StrategyRoundRobin
		return p.selectRoundRobin()
	}
}

// selectRoundRobin 轮询选择
func (p *ProxyPool) selectRoundRobin() *ProxyEntry {
	if len(p.healthy) == 0 {
		return nil
	}
	idx := p.roundRobinIdx.Add(1) % uint64(len(p.healthy))
	return p.healthy[idx]
}

// selectWeighted 加权随机选择
func (p *ProxyPool) selectWeighted() *ProxyEntry {
	if len(p.healthy) == 0 {
		return nil
	}

	// 计算总权重
	var totalWeight int64
	for _, entry := range p.healthy {
		if entry.Weight <= 0 {
			totalWeight += 1
		} else {
			totalWeight += entry.Weight
		}
	}

	if totalWeight <= 0 {
		return p.healthy[0]
	}

	// 加权随机选择
	r := rand.Int63n(totalWeight)
	var currentWeight int64
	for _, entry := range p.healthy {
		w := entry.Weight
		if w <= 0 {
			w = 1
		}
		currentWeight += w
		if r < currentWeight {
			return entry
		}
	}

	return p.healthy[len(p.healthy)-1]
}

// selectLeastConnections 最少连接选择
func (p *ProxyPool) selectLeastConnections() *ProxyEntry {
	if len(p.healthy) == 0 {
		return nil
	}

	var best *ProxyEntry
	var minConns int64 = math.MaxInt64

	for _, entry := range p.healthy {
		conns := entry.ActiveConns.Load()
		// 考虑成功率作为调节因子，需要在锁外读取
		entry.mu.RLock()
		successRate := entry.SuccessRate
		entry.mu.RUnlock()
		if successRate <= 0 {
			successRate = 0.1
		}
		// 调节后的连接数 = 实际连接数 / 成功率
		adjustedConns := int64(float64(conns) / successRate)

		if adjustedConns < minConns {
			minConns = adjustedConns
			best = entry
		}
	}

	return best
}

// MarkSuccess 标记代理成功
func (p *ProxyPool) MarkSuccess(url string) {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	entry.mu.Lock()
	entry.TotalRequests.Add(1)
	entry.ConsecutiveFailures = 0

	// 更新成功率（指数移动平均）
	if entry.TotalRequests.Load() == 1 {
		entry.SuccessRate = 1.0
	} else {
		entry.SuccessRate = entry.SuccessRate*0.9 + 0.1
	}

	// 如果之前被隔离，尝试恢复
	if entry.Status == ProxyStatusIsolated {
		entry.Status = ProxyStatusHealthy
		entry.IsolatedAt = time.Time{}
		if p.onRecovery != nil {
			p.onRecovery(entry)
		}
	}

	// 在锁内更新 stats
	if stats, ok := p.stats[url]; ok {
		stats.TotalRequests = entry.TotalRequests.Load()
		stats.FailedRequests = entry.FailedRequests.Load()
		stats.SuccessRate = entry.SuccessRate
		stats.Status = p.statusString(entry.Status)
		stats.ConsecutiveFailures = entry.ConsecutiveFailures
	}

	entry.mu.Unlock()

	p.rebuildHealthyListIfNeeded()
}

// MarkFailure 标记代理失败
func (p *ProxyPool) MarkFailure(url string) {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	entry.mu.Lock()
	entry.TotalRequests.Add(1)
	entry.FailedRequests.Add(1)
	entry.ConsecutiveFailures++

	// 更新成功率
	if entry.TotalRequests.Load() == 1 {
		entry.SuccessRate = 0.0
	} else {
		entry.SuccessRate = entry.SuccessRate * 0.9
	}

	// 检查是否需要隔离
	if entry.Status != ProxyStatusIsolated && entry.ConsecutiveFailures >= p.isolationThreshold {
		entry.Status = ProxyStatusIsolated
		entry.IsolatedAt = time.Now()
		entry.Healthy = false
		if p.onIsolation != nil {
			p.onIsolation(entry)
		}
	}

	// 在锁内更新 stats
	if stats, ok := p.stats[url]; ok {
		stats.TotalRequests = entry.TotalRequests.Load()
		stats.FailedRequests = entry.FailedRequests.Load()
		stats.SuccessRate = entry.SuccessRate
		stats.Status = p.statusString(entry.Status)
		stats.ConsecutiveFailures = entry.ConsecutiveFailures
	}

	entry.mu.Unlock()

	p.rebuildHealthyListIfNeeded()
}

// findEntry 查找代理条目
func (p *ProxyPool) findEntry(url string) (*ProxyEntry, bool) {
	for _, entry := range p.proxies {
		if entry.URL == url {
			return entry, true
		}
	}
	return nil, false
}

// rebuildHealthyListIfNeeded 如果需要则重建健康列表
func (p *ProxyPool) rebuildHealthyListIfNeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rebuildHealthyList()
}

// AcquireConnection 获取连接（增加活跃连接数）
func (p *ProxyPool) AcquireConnection(url string) bool {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return false
	}

	if entry.Status != ProxyStatusHealthy {
		return false
	}

	entry.ActiveConns.Add(1)
	return true
}

// ReleaseConnection 释放连接（减少活跃连接数）
func (p *ProxyPool) ReleaseConnection(url string) {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	// 防止计数器减到负数
	if entry.ActiveConns.Load() > 0 {
		entry.ActiveConns.Add(-1)
	}
}

// HealthCheck 执行健康检查
func (p *ProxyPool) HealthCheck(ctx context.Context) {
	p.mu.RLock()
	proxies := make([]*ProxyEntry, len(p.proxies))
	copy(proxies, p.proxies)
	p.mu.RUnlock()

	var wg sync.WaitGroup
	results := make(chan *HealthCheckResult, len(proxies))

	for _, entry := range proxies {
		wg.Add(1)
		go func(e *ProxyEntry) {
			defer wg.Done()
			result := p.checkProxy(ctx, e)
			results <- result
		}(entry)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// 处理结果
	for result := range results {
		p.processHealthCheckResult(result)
	}
}

// checkProxy 检查单个代理
func (p *ProxyPool) checkProxy(ctx context.Context, entry *ProxyEntry) *HealthCheckResult {
	start := time.Now()

	// 创建带超时的 context
	checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// 构建请求
	req, err := http.NewRequestWithContext(checkCtx, "HEAD", p.healthCheckURL, nil)
	if err != nil {
		return &HealthCheckResult{
			URL:       entry.URL,
			Healthy:   false,
			Latency:   0,
			Error:     err,
			Timestamp: time.Now(),
		}
	}

	// 创建带代理的 transport
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DisableKeepAlives: true, // 禁用 keep-alive 避免连接泄漏
		MaxIdleConns:      0,
		IdleConnTimeout:   0,
	}

	// 配置代理
	if err := ConfigureTransportProxy(transport, entry.URL, &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}); err != nil {
		return &HealthCheckResult{
			URL:       entry.URL,
			Healthy:   false,
			Latency:   0,
			Error:     err,
			Timestamp: time.Now(),
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   p.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	latency := time.Since(start)

	result := &HealthCheckResult{
		URL:       entry.URL,
		Latency:   latency,
		Timestamp: time.Now(),
	}

	if err != nil {
		result.Healthy = false
		result.Error = err
	} else {
		resp.Body.Close()
		// 只有 2xx 和 3xx 状态码认为健康
		result.Healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
		if !result.Healthy {
			result.Error = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}

	return result
}

// processHealthCheckResult 处理健康检查结果
func (p *ProxyPool) processHealthCheckResult(result *HealthCheckResult) {
	p.mu.Lock()
	entry, exists := p.findEntry(result.URL)
	p.mu.Unlock()

	if !exists || entry == nil {
		return
	}

	entry.mu.Lock()
	entry.LastCheck = result.Timestamp
	entry.Latency = result.Latency

	if result.Healthy {
		entry.Healthy = true
		entry.ConsecutiveFailures = 0

		// 如果被隔离且隔离时间已过，恢复
		if entry.Status == ProxyStatusIsolated {
			if time.Since(entry.IsolatedAt) >= p.isolationDuration {
				entry.Status = ProxyStatusHealthy
				entry.IsolatedAt = time.Time{}
				if p.onRecovery != nil {
					p.onRecovery(entry)
				}
			}
		}
	} else {
		entry.Healthy = false
		entry.Status = ProxyStatusUnhealthy
		entry.ConsecutiveFailures++

		// 检查是否需要隔离
		if entry.Status != ProxyStatusIsolated && entry.ConsecutiveFailures >= p.isolationThreshold {
			entry.Status = ProxyStatusIsolated
			entry.IsolatedAt = time.Now()
			if p.onIsolation != nil {
				p.onIsolation(entry)
			}
		}
	}
	entry.mu.Unlock()

	// 更新统计
	if stats, ok := p.stats[result.URL]; ok {
		stats.LastCheck = result.Timestamp
		stats.Latency = result.Latency
		stats.LatencyMs = float64(result.Latency.Milliseconds())
		stats.Healthy = result.Healthy
		stats.Status = p.statusString(entry.Status)
		stats.ConsecutiveFailures = entry.ConsecutiveFailures
	}

	p.rebuildHealthyListIfNeeded()

	// 回调
	if p.onHealthCheck != nil {
		p.onHealthCheck(result)
	}
}

// statusString 获取状态字符串
func (p *ProxyPool) statusString(status ProxyHealthStatus) string {
	switch status {
	case ProxyStatusHealthy:
		return "healthy"
	case ProxyStatusUnhealthy:
		return "unhealthy"
	case ProxyStatusIsolated:
		return "isolated"
	default:
		return "unknown"
	}
}

// StartHealthCheck 启动定期健康检查
func (p *ProxyPool) StartHealthCheck() {
	// 校验 checkInterval，避免非正值导致 panic
	checkInterval := p.checkInterval
	if checkInterval <= 0 {
		checkInterval = 30 * time.Second
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		// 立即执行一次检查
		p.HealthCheck(context.Background())

		for {
			select {
			case <-ticker.C:
				p.HealthCheck(context.Background())
			case <-p.stopCh:
				return
			}
		}
	}()
}

// Stop 停止代理池
func (p *ProxyPool) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.wg.Wait()
	})
}

// GetStats 获取代理统计信息
func (p *ProxyPool) GetStats() map[string]*ProxyStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make(map[string]*ProxyStats)
	for url, stats := range p.stats {
		// 复制统计信息
		entry, _ := p.findEntry(url)
		if entry != nil {
			result[url] = &ProxyStats{
				URL:                 stats.URL,
				Healthy:             entry.Healthy,
				LastCheck:           entry.LastCheck,
				Latency:             entry.Latency,
				LatencyMs:           float64(entry.Latency.Milliseconds()),
				SuccessRate:         entry.SuccessRate,
				Weight:              entry.Weight,
				ActiveConns:         entry.ActiveConns.Load(),
				TotalRequests:       entry.TotalRequests.Load(),
				FailedRequests:      entry.FailedRequests.Load(),
				Status:              p.statusString(entry.Status),
				ConsecutiveFailures: entry.ConsecutiveFailures,
			}
		} else {
			result[url] = stats
		}
	}
	return result
}

// GetPoolStatus 获取代理池状态
func (p *ProxyPool) GetPoolStatus() *ProxyPoolStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := len(p.proxies)
	healthy := len(p.healthy)
	isolated := 0
	unhealthy := 0

	for _, entry := range p.proxies {
		switch entry.Status {
		case ProxyStatusIsolated:
			isolated++
		case ProxyStatusUnhealthy:
			unhealthy++
		}
	}

	return &ProxyPoolStatus{
		Total:         total,
		Healthy:       healthy,
		Unhealthy:     total - healthy - isolated,
		Isolated:      isolated,
		Strategy:      p.strategy.String(),
		CheckInterval: p.checkInterval,
		LastUpdated:   time.Now(),
	}
}

// ProxyPoolStatus 代理池状态
type ProxyPoolStatus struct {
	Total         int           `json:"total"`
	Healthy       int           `json:"healthy"`
	Unhealthy     int           `json:"unhealthy"`
	Isolated      int           `json:"isolated"`
	Strategy      string        `json:"strategy"`
	CheckInterval time.Duration `json:"check_interval"`
	LastUpdated   time.Time     `json:"last_updated"`
}

// SetStrategy 设置选择策略
func (p *ProxyPool) SetStrategy(strategy ProxySelectionStrategy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategy = strategy
}

// GetStrategy 获取当前选择策略
func (p *ProxyPool) GetStrategy() ProxySelectionStrategy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

// SetOnHealthCheck 设置健康检查回调
func (p *ProxyPool) SetOnHealthCheck(fn func(result *HealthCheckResult)) {
	p.onHealthCheck = fn
}

// SetOnIsolation 设置隔离回调
func (p *ProxyPool) SetOnIsolation(fn func(entry *ProxyEntry)) {
	p.onIsolation = fn
}

// SetOnRecovery 设置恢复回调
func (p *ProxyPool) SetOnRecovery(fn func(entry *ProxyEntry)) {
	p.onRecovery = fn
}

// String 返回策略字符串表示
func (s ProxySelectionStrategy) String() string {
	switch s {
	case StrategyRoundRobin:
		return "round_robin"
	case StrategyWeighted:
		return "weighted"
	case StrategyLeastConnections:
		return "least_connections"
	default:
		return "unknown"
	}
}

// ParseStrategy 解析策略字符串
func ParseStrategy(s string) ProxySelectionStrategy {
	switch s {
	case "weighted":
		return StrategyWeighted
	case "least_connections":
		return StrategyLeastConnections
	default:
		return StrategyRoundRobin
	}
}

// LoadProxiesFromURLs 从 URL 列表加载代理
func (p *ProxyPool) LoadProxiesFromURLs(urls []string, defaultWeight int64) {
	for _, url := range urls {
		if url != "" {
			p.AddProxy(url, defaultWeight)
		}
	}
}

// GetHealthyProxies 获取健康代理列表
func (p *ProxyPool) GetHealthyProxies() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]string, 0, len(p.healthy))
	for _, entry := range p.healthy {
		result = append(result, entry.URL)
	}
	return result
}

// GetAllProxies 获取所有代理 URL
func (p *ProxyPool) GetAllProxies() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]string, 0, len(p.proxies))
	for _, entry := range p.proxies {
		result = append(result, entry.URL)
	}
	return result
}

// IsHealthy 检查代理是否健康
func (p *ProxyPool) IsHealthy(url string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, entry := range p.healthy {
		if entry.URL == url {
			return true
		}
	}
	return false
}

// Size 返回代理池大小
func (p *ProxyPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

// HealthySize 返回健康代理数量
func (p *ProxyPool) HealthySize() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.healthy)
}

// IsEmpty 检查代理池是否为空
func (p *ProxyPool) IsEmpty() bool {
	return p.Size() == 0
}

// HasHealthyProxy 检查是否有健康代理
func (p *ProxyPool) HasHealthyProxy() bool {
	return p.HealthySize() > 0
}

// ForceHealthCheck 强制立即执行健康检查
func (p *ProxyPool) ForceHealthCheck(ctx context.Context) {
	p.HealthCheck(ctx)
}

// RecoverIsolatedProxies 尝试恢复已过隔离期的代理
func (p *ProxyPool) RecoverIsolatedProxies() {
	p.mu.Lock()

	now := time.Now()
	// 收集需要恢复的代理
	var recoveredEntries []*ProxyEntry
	for _, entry := range p.proxies {
		if entry.Status == ProxyStatusIsolated && !entry.IsolatedAt.IsZero() {
			if now.Sub(entry.IsolatedAt) >= p.isolationDuration {
				entry.Status = ProxyStatusHealthy
				entry.IsolatedAt = time.Time{}
				entry.Healthy = true
				entry.ConsecutiveFailures = 0
				recoveredEntries = append(recoveredEntries, entry)
			}
		}
	}
	p.rebuildHealthyList()
	p.mu.Unlock()

	// 在锁外执行回调，避免死锁
	for _, entry := range recoveredEntries {
		log.Printf("[ProxyPool] 代理已恢复: %s", entry.URL)
		if p.onRecovery != nil {
			p.onRecovery(entry)
		}
	}
}
