package maker

import (
	"context"
	"strings"
	"sync"
)

// A read phase is deliberately short lived and owned by one client. It is
// carried by context so it cannot accidentally survive a workflow boundary.
type readPhaseKey struct{ owner *Client }
type readPhase struct {
	owner      *Client
	mu         sync.Mutex
	generation uint64
	pages      map[string][]byte
	bytes      int
}

const readPhaseMaxEntries = 32
const readPhaseMaxBytes = 16 * 1024 * 1024

// BeginReadPhase enables bounded page reuse within one read-only operation.
// Nested phases for the same client retain the enclosing operation's cache.
func (c *Client) BeginReadPhase(ctx context.Context) context.Context {
	if c.phase(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, readPhaseKey{owner: c}, &readPhase{owner: c, pages: make(map[string][]byte)})
}

func (c *Client) phase(ctx context.Context) *readPhase {
	p, _ := ctx.Value(readPhaseKey{owner: c}).(*readPhase)
	if p != nil && p.owner == c {
		return p
	}
	return nil
}

func cacheablePage(path string) bool {
	// Accept only exact unescaped route shapes; query strings and fragments
	// cannot become cache keys, even if a caller supplies them accidentally.
	if strings.ContainsAny(path, "?#%") || !strings.HasPrefix(path, "/") {
		return false
	}
	s := strings.Split(path[1:], "/")
	if len(s) < 2 || safeID(s[0]) != nil {
		return false
	}
	if len(s) == 2 {
		return s[1] == "products"
	}
	if len(s) == 3 && s[1] == "create" {
		return s[2] == "plugins"
	}
	if len(s) == 3 && s[1] == "products" {
		return safeID(s[2]) == nil
	}
	return len(s) == 4 && s[1] == "products" && safeID(s[2]) == nil && s[3] == "versions"
}

func (c *Client) readPhaseGet(ctx context.Context, path string, fetch func() ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := c.phase(ctx)
	if p == nil || !cacheablePage(path) {
		return fetch()
	}
	p.mu.Lock()
	if b, ok := p.pages[path]; ok {
		out := append([]byte(nil), b...)
		p.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.recordCacheHit("page")
		return out, nil
	}
	generation := p.generation
	p.mu.Unlock()
	b, err := fetch()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.generation == generation && len(p.pages) < readPhaseMaxEntries && len(b) <= readPhaseMaxBytes-p.bytes {
		if _, exists := p.pages[path]; !exists {
			p.pages[path] = append([]byte(nil), b...)
			p.bytes += len(b)
		}
	}
	p.mu.Unlock()
	return b, nil
}

// invalidateReadPhase also prevents earlier in-flight reads from repopulating
// the cache after a mutation, including a mutation with an uncertain outcome.
func (c *Client) invalidateReadPhase(ctx context.Context) {
	p := c.phase(ctx)
	if p == nil {
		return
	}
	p.mu.Lock()
	p.generation++
	p.pages = make(map[string][]byte)
	p.bytes = 0
	p.mu.Unlock()
}
