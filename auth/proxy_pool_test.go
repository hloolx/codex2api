package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewProxyPool(t *testing.T) {
	config := &ProxyPoolConfig{
		Strategy:           StrategyRoundRobin,
		CheckInterval:      30 * time.Second,
		Timeout:            10 * time.Second,
		IsolationThreshold: 3,
		IsolationDuration:  5 * time.Minute,
	}

	pool := NewProxyPool(config)
	if pool == nil {
		t.Fatal("NewProxyPool() returned nil")
	}

	if pool.strategy != StrategyRoundRobin {
		t.Errorf("expected strategy RoundRobin, got %v", pool.strategy)
	}
}

func TestDefaultProxyPoolConfig(t *testing.T) {
	config := DefaultProxyPoolConfig()
	if config == nil {
		t.Fatal("DefaultProxyPoolConfig() returned nil")
	}

	if config.Strategy != StrategyRoundRobin {
		t.Errorf("expected default strategy RoundRobin, got %v", config.Strategy)
	}
	if config.CheckInterval != 30*time.Second {
		t.Errorf("expected check interval 30s, got %v", config.CheckInterval)
	}
	if config.IsolationThreshold != 3 {
		t.Errorf("expected isolation threshold 3, got %d", config.IsolationThreshold)
	}
}

func TestProxyPoolAddProxy(t *testing.T) {
	pool := NewProxyPool(nil)

	pool.AddProxy("http://proxy1:8080", 10)
	if pool.Size() != 1 {
		t.Errorf("expected size 1, got %d", pool.Size())
	}

	// 重复添加应该被忽略
	pool.AddProxy("http://proxy1:8080", 10)
	if pool.Size() != 1 {
		t.Errorf("expected size 1 after duplicate add, got %d", pool.Size())
	}

	pool.AddProxy("http://proxy2:8080", 5)
	if pool.Size() != 2 {
		t.Errorf("expected size 2, got %d", pool.Size())
	}
}

func TestProxyPoolRemoveProxy(t *testing.T) {
	pool := NewProxyPool(nil)

	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 5)

	pool.RemoveProxy("http://proxy1:8080")
	if pool.Size() != 1 {
		t.Errorf("expected size 1, got %d", pool.Size())
	}

	// 移除不存在的代理
	pool.RemoveProxy("http://proxy3:8080")
	if pool.Size() != 1 {
		t.Errorf("expected size 1, got %d", pool.Size())
	}
}

func TestProxyPoolSelectRoundRobin(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.SetStrategy(StrategyRoundRobin)

	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 5)

	selected1 := pool.Select()
	if selected1 == nil {
		t.Fatal("Select() returned nil")
	}

	selected2 := pool.Select()
	if selected2 == nil {
		t.Fatal("Select() returned nil")
	}

	// 轮询应该交替选择
	if selected1.URL == selected2.URL {
		t.Error("RoundRobin should alternate between proxies")
	}
}

func TestProxyPoolSelectWeighted(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.SetStrategy(StrategyWeighted)

	pool.AddProxy("http://proxy1:8080", 100)
	pool.AddProxy("http://proxy2:8080", 1)

	// 统计选择次数
	counts := make(map[string]int)
	for i := 0; i < 1000; i++ {
		selected := pool.Select()
		if selected == nil {
			t.Fatal("Select() returned nil")
		}
		counts[selected.URL]++
	}

	// 权重高的应该被选择更多次
	if counts["http://proxy1:8080"] <= counts["http://proxy2:8080"] {
		t.Error("Weighted selection should prefer higher weights")
	}
}

func TestProxyPoolSelectLeastConnections(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.SetStrategy(StrategyLeastConnections)

	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 10)

	// 模拟 proxy1 有更多连接
	entry1, _ := pool.findEntry("http://proxy1:8080")
	entry2, _ := pool.findEntry("http://proxy2:8080")
	if entry1 == nil || entry2 == nil {
		t.Fatal("Failed to find proxy entries")
	}

	entry1.ActiveConns.Store(10)
	entry2.ActiveConns.Store(1)

	// 应该选择连接少的
	selected := pool.Select()
	if selected == nil {
		t.Fatal("Select() returned nil")
	}
	if selected.URL != "http://proxy2:8080" {
		t.Error("LeastConnections should select proxy with fewer connections")
	}
}

func TestProxyPoolSelectEmpty(t *testing.T) {
	pool := NewProxyPool(nil)

	selected := pool.Select()
	if selected != nil {
		t.Error("Select() should return nil for empty pool")
	}
}

func TestProxyPoolMarkSuccess(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)

	pool.MarkSuccess("http://proxy1:8080")
	pool.MarkSuccess("http://proxy1:8080")

	stats := pool.GetStats()
	if stats["http://proxy1:8080"].TotalRequests != 2 {
		t.Errorf("expected 2 total requests, got %d", stats["http://proxy1:8080"].TotalRequests)
	}
	if stats["http://proxy1:8080"].SuccessRate != 1.0 {
		t.Errorf("expected success rate 1.0, got %f", stats["http://proxy1:8080"].SuccessRate)
	}
}

func TestProxyPoolMarkFailure(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)

	// 多次标记失败
	for i := 0; i < 5; i++ {
		pool.MarkFailure("http://proxy1:8080")
	}

	stats := pool.GetStats()
	if stats["http://proxy1:8080"].FailedRequests != 5 {
		t.Errorf("expected 5 failed requests, got %d", stats["http://proxy1:8080"].FailedRequests)
	}

	// 检查是否被隔离
	entry, _ := pool.findEntry("http://proxy1:8080")
	if entry.Status != ProxyStatusIsolated {
		t.Error("Proxy should be isolated after consecutive failures")
	}
}

func TestProxyPoolAcquireReleaseConnection(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)

	// 获取连接
	if !pool.AcquireConnection("http://proxy1:8080") {
		t.Error("AcquireConnection should succeed for healthy proxy")
	}

	stats := pool.GetStats()
	if stats["http://proxy1:8080"].ActiveConns != 1 {
		t.Errorf("expected 1 active connection, got %d", stats["http://proxy1:8080"].ActiveConns)
	}

	// 释放连接
	pool.ReleaseConnection("http://proxy1:8080")

	stats = pool.GetStats()
	if stats["http://proxy1:8080"].ActiveConns != 0 {
		t.Errorf("expected 0 active connections after release, got %d", stats["http://proxy1:8080"].ActiveConns)
	}
}

func TestProxyPoolGetStats(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 5)

	stats := pool.GetStats()
	if len(stats) != 2 {
		t.Errorf("expected 2 stats entries, got %d", len(stats))
	}

	if stats["http://proxy1:8080"] == nil {
		t.Error("expected stats for proxy1")
	}
	if stats["http://proxy1:8080"].Weight != 10 {
		t.Errorf("expected weight 10, got %d", stats["http://proxy1:8080"].Weight)
	}
}

func TestProxyPoolGetPoolStatus(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 5)

	status := pool.GetPoolStatus()
	if status.Total != 2 {
		t.Errorf("expected total 2, got %d", status.Total)
	}
	if status.Healthy != 2 {
		t.Errorf("expected healthy 2, got %d", status.Healthy)
	}
	if status.Strategy != "round_robin" {
		t.Errorf("expected strategy round_robin, got %s", status.Strategy)
	}
}

func TestProxyPoolGetHealthyProxies(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 5)

	healthy := pool.GetHealthyProxies()
	if len(healthy) != 2 {
		t.Errorf("expected 2 healthy proxies, got %d", len(healthy))
	}
}

func TestProxyPoolIsHealthy(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)

	if !pool.IsHealthy("http://proxy1:8080") {
		t.Error("IsHealthy should return true for healthy proxy")
	}

	if pool.IsHealthy("http://nonexistent:8080") {
		t.Error("IsHealthy should return false for non-existent proxy")
	}
}

func TestProxyPoolSize(t *testing.T) {
	pool := NewProxyPool(nil)
	if pool.Size() != 0 {
		t.Errorf("expected size 0, got %d", pool.Size())
	}

	pool.AddProxy("http://proxy1:8080", 10)
	if pool.Size() != 1 {
		t.Errorf("expected size 1, got %d", pool.Size())
	}
}

func TestProxyPoolIsEmpty(t *testing.T) {
	pool := NewProxyPool(nil)
	if !pool.IsEmpty() {
		t.Error("IsEmpty should return true for empty pool")
	}

	pool.AddProxy("http://proxy1:8080", 10)
	if pool.IsEmpty() {
		t.Error("IsEmpty should return false for non-empty pool")
	}
}

func TestProxyPoolHasHealthyProxy(t *testing.T) {
	pool := NewProxyPool(nil)
	if pool.HasHealthyProxy() {
		t.Error("HasHealthyProxy should return false for empty pool")
	}

	pool.AddProxy("http://proxy1:8080", 10)
	if !pool.HasHealthyProxy() {
		t.Error("HasHealthyProxy should return true when healthy proxy exists")
	}
}

func TestProxyPoolLoadProxiesFromURLs(t *testing.T) {
	pool := NewProxyPool(nil)
	urls := []string{
		"http://proxy1:8080",
		"http://proxy2:8080",
		"",
		"http://proxy1:8080", // 重复
	}

	pool.LoadProxiesFromURLs(urls, 10)
	if pool.Size() != 2 {
		t.Errorf("expected size 2 (excluding empty and duplicate), got %d", pool.Size())
	}
}

func TestProxyPoolUpdateProxyWeight(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)

	pool.UpdateProxyWeight("http://proxy1:8080", 20)

	stats := pool.GetStats()
	if stats["http://proxy1:8080"].Weight != 20 {
		t.Errorf("expected weight 20, got %d", stats["http://proxy1:8080"].Weight)
	}
}

func TestProxyPoolRecoverIsolatedProxies(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.isolationDuration = 50 * time.Millisecond

	pool.AddProxy("http://proxy1:8080", 10)

	// 多次失败使代理被隔离
	for i := 0; i < 5; i++ {
		pool.MarkFailure("http://proxy1:8080")
	}

	entry, _ := pool.findEntry("http://proxy1:8080")
	if entry.Status != ProxyStatusIsolated {
		t.Fatal("Proxy should be isolated")
	}

	// 轮询等待隔离期结束，而不是固定 sleep
	start := time.Now()
	for entry.Status != ProxyStatusHealthy {
		if time.Since(start) > 2*time.Second {
			t.Fatal("Timeout waiting for proxy recovery")
		}
		time.Sleep(10 * time.Millisecond)
		pool.RecoverIsolatedProxies()
	}
}

func TestProxySelectionStrategyString(t *testing.T) {
	tests := []struct {
		strategy ProxySelectionStrategy
		expected string
	}{
		{StrategyRoundRobin, "round_robin"},
		{StrategyWeighted, "weighted"},
		{StrategyLeastConnections, "least_connections"},
		{ProxySelectionStrategy(999), "unknown"},
	}

	for _, test := range tests {
		if got := test.strategy.String(); got != test.expected {
			t.Errorf("String() = %v, want %v", got, test.expected)
		}
	}
}

func TestParseStrategy(t *testing.T) {
	tests := []struct {
		input    string
		expected ProxySelectionStrategy
	}{
		{"round_robin", StrategyRoundRobin},
		{"weighted", StrategyWeighted},
		{"least_connections", StrategyLeastConnections},
		{"unknown", StrategyRoundRobin},
		{"", StrategyRoundRobin},
	}

	for _, test := range tests {
		if got := ParseStrategy(test.input); got != test.expected {
			t.Errorf("ParseStrategy(%s) = %v, want %v", test.input, got, test.expected)
		}
	}
}

func TestProxyPoolConcurrency(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)
	pool.AddProxy("http://proxy2:8080", 5)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selected := pool.Select()
			if selected != nil {
				pool.MarkSuccess(selected.URL)
			}
		}()
	}
	wg.Wait()

	stats := pool.GetStats()
	totalRequests := stats["http://proxy1:8080"].TotalRequests + stats["http://proxy2:8080"].TotalRequests
	if totalRequests != 100 {
		t.Errorf("expected 100 total requests, got %d", totalRequests)
	}
}

func TestProxyPoolCallbacks(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.isolationThreshold = 1

	isolated := false
	recovered := false

	pool.SetOnIsolation(func(entry *ProxyEntry) {
		isolated = true
	})

	pool.SetOnRecovery(func(entry *ProxyEntry) {
		recovered = true
	})

	pool.AddProxy("http://proxy1:8080", 10)

	// 触发隔离
	pool.MarkFailure("http://proxy1:8080")
	if !isolated {
		t.Error("OnIsolation callback should be called")
	}

	// 触发恢复
	pool.MarkSuccess("http://proxy1:8080")
	if !recovered {
		t.Error("OnRecovery callback should be called")
	}
}

func TestProxyPoolHealthCheck(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.healthCheckURL = "http://127.0.0.1:1/generate_204" // 无效地址

	pool.AddProxy("http://127.0.0.1:1", 10)

	// 执行健康检查
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool.HealthCheck(ctx)

	// 由于地址无效，代理应该被标记为不健康
	entry, _ := pool.findEntry("http://127.0.0.1:1")
	if entry.Healthy {
		t.Error("Proxy should be marked unhealthy after failed health check")
	}
}

func TestProxyPoolForceHealthCheck(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://127.0.0.1:1", 10)

	// 强制健康检查不应该 panic
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool.ForceHealthCheck(ctx)
}

func TestProxyPoolStartStop(t *testing.T) {
	pool := NewProxyPool(nil)
	pool.AddProxy("http://proxy1:8080", 10)

	// 启动健康检查
	pool.StartHealthCheck()

	// 等待一小段时间
	time.Sleep(100 * time.Millisecond)

	// 停止
	pool.Stop()
}
