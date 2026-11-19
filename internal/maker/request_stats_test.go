package maker

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestRequestStatsAggregate(t *testing.T) {
	c := &Client{}
	c.recordRequest(true, "GET", 10*time.Millisecond, 120, false)
	c.recordRequest(false, "POST", 20*time.Millisecond, 80, true)
	c.recordRequest(true, "PRIVATE-TOKEN", -time.Second, -1, false)
	c.recordCacheHit("script")
	c.recordCacheHit("page")
	c.recordCacheHit("PRIVATE-TOKEN")
	got := c.RequestStats()
	want := RequestStats{TotalRequests: 3, MakerRequests: 2, StorageRequests: 1, FailedRequests: 1, ResponseBytes: 200, RequestTime: 30 * time.Millisecond, ScriptCacheHits: 1, PageCacheHits: 1, GetRequests: 1, PostRequests: 1, OtherMethodRequests: 1}
	if got != want {
		t.Fatalf("RequestStats() = %+v, want %+v", got, want)
	}
	// A returned snapshot is independent of subsequent changes.
	c.recordCacheHit("page")
	if got.PageCacheHits != 1 {
		t.Fatal("snapshot changed")
	}
}

func TestRequestStatsConcurrent(t *testing.T) {
	const workers, perWorker = 32, 500
	c := &Client{}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				c.recordRequest(true, "GET", time.Microsecond, 2, true)
				c.recordCacheHit("script")
				_ = c.RequestStats()
			}
		}()
	}
	wg.Wait()
	got := c.RequestStats()
	const n = workers * perWorker
	if got.TotalRequests != n || got.MakerRequests != n || got.GetRequests != n || got.FailedRequests != n || got.ResponseBytes != 2*n || got.RequestTime != n*time.Microsecond || got.ScriptCacheHits != n {
		t.Fatalf("unexpected concurrent counts: %+v", got)
	}
}

func TestRequestStatsSaturates(t *testing.T) {
	c := &Client{}
	c.stats.values.TotalRequests = math.MaxUint64
	c.stats.values.ResponseBytes = math.MaxUint64 - 1
	c.stats.values.RequestTime = time.Duration(math.MaxInt64 - 1)
	c.recordRequest(true, "GET", 2, 2, false)
	got := c.RequestStats()
	if got.TotalRequests != math.MaxUint64 || got.ResponseBytes != math.MaxUint64 || got.RequestTime != time.Duration(math.MaxInt64) {
		t.Fatalf("counters did not saturate: %+v", got)
	}
}
