package cli

import (
	"fmt"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

func (a *app) writeRequestStats(sessions []session) error {
	var requests, makerRequests, storageRequests, failures, responseBytes, scriptHits, pageHits uint64
	var elapsed time.Duration
	seen := map[*maker.Client]bool{}
	for _, s := range sessions {
		if s.client == nil || seen[s.client] {
			continue
		}
		seen[s.client] = true
		stats := s.client.RequestStats()
		requests += stats.TotalRequests
		makerRequests += stats.MakerRequests
		storageRequests += stats.StorageRequests
		failures += stats.FailedRequests
		responseBytes += stats.ResponseBytes
		scriptHits += stats.ScriptCacheHits
		pageHits += stats.PageCacheHits
		elapsed += stats.RequestTime
	}
	_, err := fmt.Fprintf(a.o.Err, "HTTP: %d requests (Maker %d, storage %d), HTTP failures %d; cache hits: scripts %d, pages %d; response bodies %d bytes; summed request time %s\n", requests, makerRequests, storageRequests, failures, scriptHits, pageHits, responseBytes, elapsed.Round(time.Millisecond))
	return err
}
