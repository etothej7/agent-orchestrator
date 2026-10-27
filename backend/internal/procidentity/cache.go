package procidentity

import (
	"context"
	"sync"
	"time"
)

// Identifier is what Resolver provides and Cached wraps.
type Identifier interface {
	Identify(ctx context.Context, clientPort, serverPort int) (Result, error)
}

// CacheConfig bounds a Cached identifier.
type CacheConfig struct {
	// NegativeTTL is how long a refusal is reused.
	NegativeTTL time.Duration
	// MaxEntries caps the cache (a flood of distinct ports cannot grow it).
	MaxEntries int
	Now        func() time.Time
}

type cacheKey struct{ client, server int }

// Cached remembers REFUSALS for a moment so a flood of unidentifiable calls costs one lookup per
// connection, not one per request. It never remembers a successful identification: a closed
// connection's ports can be taken over by another process within moments, and a remembered answer
// would hand that process the previous owner's session. The lookup is cheap (about 0.3 ms), so
// every positive answer is recomputed from the live socket and process tables. A cached refusal
// can only make a caller less identified, never more.
type Cached struct {
	inner Identifier
	cfg   CacheConfig

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

type cacheEntry struct {
	err     error
	expires time.Time
}

// NewCached wraps inner.
func NewCached(inner Identifier, cfg CacheConfig) *Cached {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 512
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = time.Second
	}
	return &Cached{inner: inner, cfg: cfg, entries: map[cacheKey]cacheEntry{}}
}

// Len is the number of cached refusals.
func (c *Cached) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Identify implements Identifier.
func (c *Cached) Identify(ctx context.Context, clientPort, serverPort int) (Result, error) {
	key := cacheKey{clientPort, serverPort}
	now := c.cfg.Now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if now.Before(e.expires) {
			c.mu.Unlock()
			return Result{}, e.err
		}
		delete(c.entries, key)
	}
	c.mu.Unlock()

	res, err := c.inner.Identify(ctx, clientPort, serverPort)
	if err == nil {
		return res, nil
	}
	c.mu.Lock()
	if len(c.entries) >= c.cfg.MaxEntries {
		for k, e := range c.entries { // drop expired first, else arbitrary entries
			if !c.cfg.Now().Before(e.expires) {
				delete(c.entries, k)
			}
		}
		for k := range c.entries {
			if len(c.entries) < c.cfg.MaxEntries {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{err: err, expires: c.cfg.Now().Add(c.cfg.NegativeTTL)}
	c.mu.Unlock()
	return Result{}, err
}
