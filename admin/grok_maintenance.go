package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/google/uuid"
)

func grokMaintenanceRetryDelay(attempt int64) time.Duration {
	minutes := int64(1) << min(max(attempt-1, 0), 4)
	base := time.Duration(min(minutes, 15)) * time.Minute
	// Positive jitter never shortens the declared minimum delay.
	return base + time.Duration(rand.Int64N(int64(base/5)+1))
}

func grokSyncErrors(failures map[string]string) error {
	keys := make([]string, 0, len(failures))
	for key := range failures {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+failures[key])
	}
	return fmt.Errorf("Grok sync partial failure: %s", strings.Join(parts, "; "))
}

// Refill free slots without waiting for the slowest account in a batch.
// Claimed rows start immediately, so no local waiting queue consumes leases.
func (h *Handler) runGrokMaintenanceQueue(ctx context.Context, kind string, limit int) {
	owner := "grok-" + uuid.NewString()
	runGrokQueueLoop(ctx, kind, limit,
		func(ctx context.Context, slots int) ([]database.MaintenanceJob, error) {
			return h.db.ClaimMaintenanceJobs(ctx, kind, owner, time.Now(), grokMaintenanceLease, slots)
		},
		func(ctx context.Context, job database.MaintenanceJob) error {
			var err error
			if kind == database.MaintenanceJobGrokCapability {
				err = h.runOneGrokCapabilityJob(ctx, owner, job)
			} else {
				err = h.runOneGrokMaintenanceJob(ctx, owner, job)
			}
			if err != nil {
				failCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				persistErr := h.db.FailMaintenanceJob(failCtx, job.EntityID, kind, owner, time.Now().Add(grokMaintenanceRetryDelay(job.Attempts)), err)
				cancel()
				if persistErr != nil {
					log.Printf("[grok-maintenance] kind=%s account=%d persist failure: %v", kind, job.EntityID, persistErr)
				}
				log.Printf("[grok-maintenance] kind=%s account=%d attempt=%d error=%v", kind, job.EntityID, job.Attempts, err)
			}
			return err
		})
}

func runGrokQueueLoop(ctx context.Context, kind string, limit int,
	claim func(context.Context, int) ([]database.MaintenanceJob, error),
	work func(context.Context, database.MaintenanceJob) error) {
	limit = max(1, min(limit, grokMaintenanceBatchSize))
	done := make(chan error, limit)
	tick := time.NewTicker(grokMaintenancePollInterval)
	report := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	defer report.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	running, claimed, completed, failed := 0, 0, 0, 0
	refill := func() {
		if ctx.Err() != nil || running >= limit {
			return
		}
		jobs, err := claim(ctx, limit-running)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("[grok-maintenance] kind=%s claim failed: %v", kind, err)
			}
			return
		}
		for _, job := range jobs {
			running++
			claimed++
			wg.Add(1)
			go func(job database.MaintenanceJob) {
				defer wg.Done()
				done <- work(ctx, job)
			}(job)
		}
	}
	refill()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-done:
			running--
			if err == nil {
				completed++
			} else {
				failed++
			}
			// Drain completions together to avoid a query per account.
			for len(done) > 0 {
				err = <-done
				running--
				if err == nil {
					completed++
				} else {
					failed++
				}
			}
			refill()
		case <-tick.C:
			refill()
		case <-report.C:
			log.Printf("[grok-maintenance] kind=%s batch_limit=%d inflight=%d claimed=%d completed=%d failed=%d", kind, limit, running, claimed, completed, failed)
			claimed, completed, failed = 0, 0, 0
		}
	}
}

func (h *Handler) runOneGrokCapabilityJob(ctx context.Context, owner string, job database.MaintenanceJob) error {
	row, err := h.db.GetAccountByID(ctx, job.EntityID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (row == nil || !row.Enabled || grokRowUpstreamType(row) != "grok" || row.Status == "deleted")) {
		return h.db.DeleteMaintenanceJob(ctx, job.EntityID, job.JobKind)
	}
	if err != nil {
		return err
	}
	jobCtx, cancel := context.WithTimeout(ctx, grokProbeRunGuard)
	defer cancel()
	account := h.store.FindByID(job.EntityID)
	if account == nil {
		return fmt.Errorf("account projection unavailable")
	}
	result, err := h.runGrokCapabilityProbe(jobCtx, job.EntityID, false)
	if err != nil {
		return err
	}
	due, err := grokNextCapabilityDue(account, result.State, time.Now())
	if err != nil {
		return err
	}
	return h.db.CompleteMaintenanceJob(jobCtx, job.EntityID, job.JobKind, owner, due)
}
