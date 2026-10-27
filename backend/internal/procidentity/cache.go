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
	// TTL is how long a successful identification is reused; NegativeTTL how long a refusal is.
	TTL, NegativeTTL time.Duration
	// MaxEntries caps the cache (a flood of distinct ports cannot grow it).
	MaxEntries int
	Now        func() time.Time
}

type cacheKey struct{ client, server int }

type cacheEntry struct {
	res     Result
	err     error
	expires time.Time
	started time.Time // start time of res.PID when cached, to notice a recycled pid
}

// Cached reuses an identification for the same connection for a short time. A positive entry is
// only served while its caller is still the same process (the kernel's start time for that pid is
// unchanged), so a port reused by another process is never served the previous answer.
type Cached struct {
	inner Identifier
	procs ProcTable
	cfg   CacheConfig

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

// NewCached wraps inner.
func NewCached(inner Identifier, procs ProcTable, cfg CacheConfig) *Cached {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 512
	}
	return &Cached{inner: inner, procs: procs, cfg: cfg, entries: map[cacheKey]cacheEntry{}}
}

// Len is the number of cached entries.
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
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		stillSame := e.err != nil
		if !stillSame {
			info, err := c.procs.Info(e.res.PID)
			stillSame = err == nil && info.Start.Equal(e.started)
		}
		if stillSame {
			c.mu.Unlock()
			return e.res, e.err
		}
		delete(c.entries, key)
	}
	c.mu.Unlock()

	res, err := c.inner.Identify(ctx, clientPort, serverPort)
	entry := cacheEntry{res: res, err: err}
	cacheable := true
	if err == nil {
		if info, infoErr := c.procs.Info(res.PID); infoErr == nil {
			entry.started = info.Start
			entry.expires = c.cfg.Now().Add(c.cfg.TTL)
		} else {
			cacheable = false // identified, but its process cannot be re-checked: not cached
		}
	} else {
		entry.expires = c.cfg.Now().Add(c.cfg.NegativeTTL)
	}
	if !cacheable {
		return res, err
	}
	c.mu.Lock()
	if len(c.entries) >= c.cfg.MaxEntries {
		for k, e := range c.entries { // drop expired first, else an arbitrary entry
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
	c.entries[key] = entry
	c.mu.Unlock()
	return res, err
}
