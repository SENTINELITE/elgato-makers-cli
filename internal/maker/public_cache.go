package maker

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// Cache only content-addressed Next.js chunks. A fresh page still selects the
// URLs for every discovery; unhashed scripts are fetched every time.
var immutableScriptName = regexp.MustCompile(`(?:^|[-.])[0-9a-fA-F]{16,}\.js$`)

const publicScriptMaxEntries = 128
const publicScriptMaxBytes = 32 << 20

type scriptFlight struct{ done chan struct{} }
type publicScriptCache struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	order   []string
	bytes   int
	flights map[string]*scriptFlight
}

func (c *Client) publicScript(ctx context.Context, u *url.URL) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cacheable := u != nil && u.User == nil && u.Fragment == "" && u.Scheme == c.base.Scheme && u.Host == c.base.Host && strings.HasPrefix(u.Path, "/_next/static/") && immutableScriptName.MatchString(u.Path)
	if !cacheable {
		return c.request(ctx, http.MethodGet, u.String(), "", "", nil, true)
	}
	key := u.String()
	cache := &c.publicScripts
	for {
		cache.mu.Lock()
		if err := ctx.Err(); err != nil {
			cache.mu.Unlock()
			return nil, err
		}
		if body, ok := cache.bodies[key]; ok {
			cache.mu.Unlock()
			c.recordCacheHit("script")
			return body, nil
		}
		if flight := cache.flights[key]; flight != nil {
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight.done:
				continue
			}
		}
		if cache.flights == nil {
			cache.flights = make(map[string]*scriptFlight)
		}
		flight := &scriptFlight{done: make(chan struct{})}
		cache.flights[key] = flight
		cache.mu.Unlock()

		body, err := c.request(ctx, http.MethodGet, key, "", "", nil, true)
		cache.mu.Lock()
		if err == nil && ctx.Err() == nil && len(body) <= publicScriptMaxBytes {
			if cache.bodies == nil {
				cache.bodies = make(map[string][]byte)
			}
			for len(cache.order) >= publicScriptMaxEntries || cache.bytes+len(body) > publicScriptMaxBytes {
				oldest := cache.order[0]
				cache.order = cache.order[1:]
				cache.bytes -= len(cache.bodies[oldest])
				delete(cache.bodies, oldest)
			}
			cache.bodies[key] = body
			cache.order = append(cache.order, key)
			cache.bytes += len(body)
		}
		delete(cache.flights, key)
		close(flight.done)
		cache.mu.Unlock()
		return body, err
	}
}
