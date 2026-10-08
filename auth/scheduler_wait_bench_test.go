package auth

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// BenchmarkSchedulerSaturatedWait measures local dispatch work and handoff
// latency with one occupied slot and an already-established backlog. It does
// not model upstream latency or distributed concurrency limits.
func BenchmarkSchedulerSaturatedWait(b *testing.B) {
	for _, backlog := range []int{100, 1000} {
		b.Run(strconv.Itoa(backlog), func(b *testing.B) {
			b.ReportAllocs()
			var selections, wakeups uint64
			var latencies []time.Duration
			var cancelTime time.Duration
			const grants = 32
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 90, 1)
				store := (&Store{accounts: []*Account{acc}, schedulerMetrics: newSchedulerRuntimeMetrics()}).withMaxConcurrency(1)
				store.rebuildAccountIndex()
				store.SetSchedulerEngine("indexed")
				held := store.Next()
				if held == nil {
					b.Fatal("failed to fill account capacity")
				}
				ctx, cancel := context.WithCancel(context.Background())
				acquired := make(chan *Account, backlog)
				var done sync.WaitGroup
				for j := 0; j < backlog; j++ {
					done.Add(1)
					go func(key int64) {
						defer done.Done()
						a, _ := store.WaitForSessionAvailable(ctx, "", 10*time.Second, key, nil)
						if a != nil {
							acquired <- a
						}
					}(int64(j%8 + 1))
				}
				until := time.Now().Add(5 * time.Second)
				for store.GetSchedulerMetrics().Waiters != int64(backlog) && time.Now().Before(until) {
					time.Sleep(time.Millisecond)
				}
				if store.GetSchedulerMetrics().Waiters != int64(backlog) {
					cancel()
					done.Wait()
					b.Fatal("backlog failed to register")
				}
				time.Sleep(20 * time.Millisecond)
				before := store.GetSchedulerMetrics()
				b.StartTimer()
				for j := 0; j < grants; j++ {
					start := time.Now()
					store.Release(held)
					select {
					case held = <-acquired:
						latencies = append(latencies, time.Since(start))
					case <-time.After(5 * time.Second):
						cancel()
						done.Wait()
						b.Fatal("released capacity did not reach a waiter")
					}
				}
				b.StopTimer()
				after := store.GetSchedulerMetrics()
				selections += after.SelectionTotal - before.SelectionTotal
				wakeups += after.WaitWakeups - before.WaitWakeups
				start := time.Now()
				cancel()
				done.Wait()
				cancelTime += time.Since(start)
				store.Release(held)
				close(acquired)
				for a := range acquired {
					store.Release(a)
				}
				store.Stop()
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			b.ReportMetric(float64(selections)/float64(b.N*grants), "selections/grant")
			b.ReportMetric(float64(wakeups)/float64(b.N*grants), "wakeups/grant")
			b.ReportMetric(float64(latencies[len(latencies)*95/100].Nanoseconds())/1000, "p95-us/grant")
			b.ReportMetric(float64(latencies[len(latencies)*99/100].Nanoseconds())/1000, "p99-us/grant")
			b.ReportMetric(float64(cancelTime.Nanoseconds())/float64(b.N)/1000, "cancel-us/backlog")
		})
	}
}

func BenchmarkSchedulerUncontended(b *testing.B) {
	store := newBenchStore(1000)
	b.Cleanup(store.Stop)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		account := store.Next()
		if account == nil {
			b.Fatal("idle pool admission failed")
		}
		store.Release(account)
	}
}
