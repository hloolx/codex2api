package admin

import (
	"context"
	"github.com/codex2api/database"
	"sync/atomic"
	"testing"
	"time"
)

func TestGrokQueueRefillsWithoutExceeding64WhileCapabilityQueueIsSlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capStarted := make(chan struct{}, 4)
	capDone := make(chan struct{})
	go func() {
		defer close(capDone)
		n := 0
		runGrokQueueLoop(ctx, database.MaintenanceJobGrokCapability, 4, func(_ context.Context, slots int) ([]database.MaintenanceJob, error) {
			jobs := []database.MaintenanceJob{}
			for n < 4 && len(jobs) < slots {
				n++
				jobs = append(jobs, database.MaintenanceJob{EntityID: int64(n)})
			}
			return jobs, nil
		}, func(ctx context.Context, _ database.MaintenanceJob) error {
			capStarted <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	timeout := time.After(10 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case <-capStarted:
		case <-timeout:
			t.Fatal("capability queue did not start")
		}
	}
	started := make(chan struct{}, 65)
	release := make(chan struct{})
	finished := make(chan struct{})
	var active, maximum, badClaim atomic.Int64
	go func() {
		defer close(finished)
		claimed := 0
		runGrokQueueLoop(ctx, database.MaintenanceJobGrokFreshness, 64, func(_ context.Context, slots int) ([]database.MaintenanceJob, error) {
			if slots > 64 || slots < 1 {
				badClaim.Store(1)
			}
			jobs := []database.MaintenanceJob{}
			for claimed < 65 && len(jobs) < slots {
				claimed++
				jobs = append(jobs, database.MaintenanceJob{EntityID: int64(claimed)})
			}
			return jobs, nil
		}, func(ctx context.Context, _ database.MaintenanceJob) error {
			n := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); n > old; old = maximum.Load() {
				if maximum.CompareAndSwap(old, n) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	for i := 0; i < 64; i++ {
		select {
		case <-started:
		case <-timeout:
			t.Fatal("64 control-plane slots did not start independently")
		}
	}
	select {
	case <-started:
		t.Fatal("exceeded 64")
	default:
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-timeout:
		t.Fatal("free slot was not refilled until batch drained")
	}
	if maximum.Load() != 64 || badClaim.Load() != 0 {
		t.Fatalf("maximum=%d badclaim=%d", maximum.Load(), badClaim.Load())
	}
	cancel()
	for _, done := range []chan struct{}{finished, capDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("queue shutdown blocked")
		}
	}
}
