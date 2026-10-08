package auth

// withMaxConcurrency 供测试构造 Store:maxConcurrency 是 atomic.Int64,不能写进复合字面量。
func (s *Store) withMaxConcurrency(n int64) *Store {
	s.maxConcurrency.Store(n)
	return s
}
