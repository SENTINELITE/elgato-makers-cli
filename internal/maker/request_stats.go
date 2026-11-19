package maker

import (
	"math"
	"strings"
	"sync"
	"time"
)

// RequestStats contains aggregate HTTP activity for a client. It never contains
// request identifiers, destinations, credentials, or payloads.
type RequestStats struct {
	TotalRequests, MakerRequests, StorageRequests uint64
	FailedRequests, ResponseBytes                 uint64
	RequestTime                                   time.Duration
	ScriptCacheHits, PageCacheHits                uint64
	GetRequests, PostRequests, PutRequests        uint64
	PatchRequests, DeleteRequests, HeadRequests   uint64
	OptionsRequests, OtherMethodRequests          uint64
}

type requestCounters struct {
	mu     sync.Mutex
	values RequestStats
}

// RequestStats returns a concurrency-safe snapshot of aggregate activity.
func (c *Client) RequestStats() RequestStats {
	c.stats.mu.Lock()
	defer c.stats.mu.Unlock()
	return c.stats.values
}

func addCounter(value, amount uint64) uint64 {
	if math.MaxUint64-value < amount {
		return math.MaxUint64
	}
	return value + amount
}

func (c *Client) recordRequest(maker bool, method string, duration time.Duration, responseBytes int64, failed bool) {
	c.stats.mu.Lock()
	defer c.stats.mu.Unlock()
	s := &c.stats.values
	s.TotalRequests = addCounter(s.TotalRequests, 1)
	if maker {
		s.MakerRequests = addCounter(s.MakerRequests, 1)
	} else {
		s.StorageRequests = addCounter(s.StorageRequests, 1)
	}
	if failed {
		s.FailedRequests = addCounter(s.FailedRequests, 1)
	}
	if responseBytes > 0 {
		s.ResponseBytes = addCounter(s.ResponseBytes, uint64(responseBytes))
	}
	if duration > 0 {
		if duration > time.Duration(math.MaxInt64)-s.RequestTime {
			s.RequestTime = time.Duration(math.MaxInt64)
		} else {
			s.RequestTime += duration
		}
	}
	var count *uint64
	switch strings.ToUpper(method) {
	case "GET":
		count = &s.GetRequests
	case "POST":
		count = &s.PostRequests
	case "PUT":
		count = &s.PutRequests
	case "PATCH":
		count = &s.PatchRequests
	case "DELETE":
		count = &s.DeleteRequests
	case "HEAD":
		count = &s.HeadRequests
	case "OPTIONS":
		count = &s.OptionsRequests
	default:
		count = &s.OtherMethodRequests
	}
	*count = addCounter(*count, 1)
}

func (c *Client) recordCacheHit(kind string) {
	c.stats.mu.Lock()
	defer c.stats.mu.Unlock()
	switch kind {
	case "script":
		c.stats.values.ScriptCacheHits = addCounter(c.stats.values.ScriptCacheHits, 1)
	case "page":
		c.stats.values.PageCacheHits = addCounter(c.stats.values.PageCacheHits, 1)
	}
}
