package s3store

import (
	"container/list"
	"sync"
)

// cache holds expanded sites in memory, bounded by a total byte budget and
// evicted least-recently-used first.
//
// Entries are versioned: a site's cache key carries the object version its
// bytes came from, so a push that writes a new version makes the old entry
// unreachable rather than stale. Nothing has to invalidate explicitly.
//
// A site larger than the whole budget is never cached — it is fetched and
// served, but not retained, so one oversized site cannot evict everything
// else on every request.
type cache struct {
	mu       sync.Mutex
	maxBytes int64
	curBytes int64
	ll       *list.List // front = most recently used
	items    map[cacheKey]*list.Element
}

type cacheKey struct {
	name    string
	version string
}

type cacheEntry struct {
	key   cacheKey
	fsys  *memFS
	bytes int64
}

// newCache returns a cache bounded to maxBytes. A non-positive budget disables
// caching entirely.
func newCache(maxBytes int64) *cache {
	return &cache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[cacheKey]*list.Element),
	}
}

func (c *cache) get(name, version string) (*memFS, bool) {
	if c.maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[cacheKey{name, version}]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheEntry).fsys, true
}

func (c *cache) put(name, version string, fsys *memFS, size int64) {
	// Skip caching when disabled, or when this site alone would blow the
	// budget — retaining it would evict every other entry for no gain.
	if c.maxBytes <= 0 || size > c.maxBytes {
		return
	}
	key := cacheKey{name, version}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: key, fsys: fsys, bytes: size})
	c.items[key] = el
	c.curBytes += size
	for c.curBytes > c.maxBytes {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.evict(oldest)
	}
}

// dropAll removes every cached version of a site, used when it is deleted.
func (c *cache) dropAll(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, el := range c.items {
		if key.name == name {
			c.evict(el)
		}
	}
}

// evict removes one element. The caller must hold c.mu.
func (c *cache) evict(el *list.Element) {
	entry := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.items, entry.key)
	c.curBytes -= entry.bytes
}
