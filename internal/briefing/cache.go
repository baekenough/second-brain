package briefing

import "sync"

// defaultCacheCapacity is used when NewCache is given a non-positive capacity.
// A cache that never hits would silently double LLM spend, so a misconfigured
// value degrades to a working default rather than to a no-op.
const defaultCacheCapacity = 8

// Cache is a tiny in-process, capacity-bounded briefing cache keyed by
// Input.CacheKey.
//
// It is intentionally a map plus an insertion-order slice rather than a real
// LRU: the capacity is single digits and there is one user, so the eviction
// policy barely matters, while an extra data structure would be code to
// maintain for no gain. Entries are lost on restart, which costs exactly one
// extra LLM call after a deploy.
type Cache struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]Result
	order    []string // insertion order; order[0] is the next eviction victim
}

// NewCache returns a Cache holding at most capacity briefings.
func NewCache(capacity int) *Cache {
	if capacity <= 0 {
		capacity = defaultCacheCapacity
	}
	return &Cache{
		capacity: capacity,
		entries:  make(map[string]Result, capacity),
		order:    make([]string, 0, capacity),
	}
}

// Get returns the cached briefing for key, if present.
func (c *Cache) Get(key string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[key]
	return r, ok
}

// Put stores r under key, evicting the oldest entry when full. Re-putting an
// existing key overwrites in place and does not consume a second slot.
func (c *Cache) Put(key string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; exists {
		c.entries[key] = r
		return
	}

	if len(c.order) >= c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[key] = r
	c.order = append(c.order, key)
}
