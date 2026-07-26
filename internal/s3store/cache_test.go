package s3store

import "testing"

func fakeFS() *memFS { return &memFS{files: map[string][]byte{}, dirs: map[string][]string{".": {}}} }

func TestCacheHitAndMiss(t *testing.T) {
	c := newCache(1000)
	if _, ok := c.get("a", "v1"); ok {
		t.Error("empty cache reported a hit")
	}
	c.put("a", "v1", fakeFS(), 100)
	if _, ok := c.get("a", "v1"); !ok {
		t.Error("want hit after put")
	}
}

// A new version must not be served from the old entry — this is what replaces
// explicit invalidation on push.
func TestCacheVersionIsPartOfTheKey(t *testing.T) {
	c := newCache(1000)
	c.put("a", "v1", fakeFS(), 100)
	if _, ok := c.get("a", "v2"); ok {
		t.Error("v2 must miss while only v1 is cached")
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newCache(250)
	c.put("a", "v1", fakeFS(), 100)
	c.put("b", "v1", fakeFS(), 100)

	// Touch "a" so "b" becomes the least recently used.
	if _, ok := c.get("a", "v1"); !ok {
		t.Fatal("a should be cached")
	}
	c.put("c", "v1", fakeFS(), 100) // 300 > 250, forces an eviction

	if _, ok := c.get("b", "v1"); ok {
		t.Error("b was least recently used and should have been evicted")
	}
	for _, name := range []string{"a", "c"} {
		if _, ok := c.get(name, "v1"); !ok {
			t.Errorf("%s should still be cached", name)
		}
	}
}

// A site bigger than the whole budget must not be retained, otherwise every
// request for it would flush the cache.
func TestCacheSkipsOversizedEntries(t *testing.T) {
	c := newCache(100)
	c.put("small", "v1", fakeFS(), 50)
	c.put("huge", "v1", fakeFS(), 500)

	if _, ok := c.get("huge", "v1"); ok {
		t.Error("oversized entry should not be cached")
	}
	if _, ok := c.get("small", "v1"); !ok {
		t.Error("oversized put must not evict existing entries")
	}
}

func TestCacheDisabled(t *testing.T) {
	c := newCache(0)
	c.put("a", "v1", fakeFS(), 10)
	if _, ok := c.get("a", "v1"); ok {
		t.Error("cache with a zero budget should never hit")
	}
}

func TestCacheDropAllRemovesEveryVersion(t *testing.T) {
	c := newCache(1000)
	c.put("a", "v1", fakeFS(), 100)
	c.put("a", "v2", fakeFS(), 100)
	c.put("b", "v1", fakeFS(), 100)

	c.dropAll("a")

	for _, v := range []string{"v1", "v2"} {
		if _, ok := c.get("a", v); ok {
			t.Errorf("a/%s should be gone after dropAll", v)
		}
	}
	if _, ok := c.get("b", "v1"); !ok {
		t.Error("dropAll must not touch other sites")
	}
	if c.curBytes != 100 {
		t.Errorf("curBytes = %d, want 100", c.curBytes)
	}
}
