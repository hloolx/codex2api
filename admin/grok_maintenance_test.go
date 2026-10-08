package admin

import (
	"testing"
	"time"
)

func TestGrokMaintenanceBackoffSchedule(t *testing.T) {
	for attempt, minutes := range []int{1, 2, 4, 8, 15, 15, 15} {
		for i := 0; i < 100; i++ {
			got := grokMaintenanceRetryDelay(int64(attempt + 1))
			base := time.Duration(minutes) * time.Minute
			if got < base || got > base+base/5 {
				t.Fatalf("attempt=%d got=%s", attempt+1, got)
			}
		}
	}
	if grokMaintenanceBatchSize != 64 {
		t.Fatalf("batch=%d", grokMaintenanceBatchSize)
	}
}
