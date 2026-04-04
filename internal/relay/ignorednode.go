package relay

import (
	"sync"
	"time"
)

const ignoredDuration = 3 * time.Minute

// IgnoredNodeCollection tracks hosts that should be temporarily skipped.
// Entries automatically expire after ignoredDuration.
type IgnoredNodeCollection struct {
	mu        sync.Mutex
	entries   map[string]time.Time
	threshold time.Duration
}

func NewIgnoredNodeCollection() *IgnoredNodeCollection {
	return &IgnoredNodeCollection{
		entries:   make(map[string]time.Time),
		threshold: ignoredDuration,
	}
}

// Add marks an address as ignored starting from now.
func (c *IgnoredNodeCollection) Add(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[addr] = time.Now()
}

// Contains returns true if addr is still within the ignore window.
func (c *IgnoredNodeCollection) Contains(addr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	t, ok := c.entries[addr]
	if !ok {
		return false
	}
	if time.Since(t) > c.threshold {
		delete(c.entries, addr)
		return false
	}
	return true
}
