package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestImageExecutionGateBoundsThousandConcurrentJobs(t *testing.T) {
	ConfigureImageExecutionLimit(2)
	defer ConfigureImageExecutionLimit(0)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := AcquireImageExecution(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			n := active.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
		}()
	}
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak=%d", peak.Load())
	}
}

func TestImageExecutionGateDirectEndpointsAndInheritance(t *testing.T) {
	ConfigureImageExecutionLimit(1)
	defer ConfigureImageExecutionLimit(0)
	ctx, release, err := AcquireImageExecution(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		h := &Handler{}
		if path == "/v1/images/edits" {
			h.ImagesEdits(c)
		} else {
			h.ImagesGenerations(c)
		}
		if w.Code != 429 {
			t.Fatalf("busy endpoint %s status=%d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/images/edits", nil).WithContext(ctx)
	free, ok := admitDirectImageExecution(c)
	if !ok {
		t.Fatal("worker double counted")
	}
	free()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = AcquireImageExecution(cancelled); err == nil {
		t.Fatal("cancellation ignored")
	}
}
