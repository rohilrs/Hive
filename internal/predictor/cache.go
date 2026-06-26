package predictor

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

// cacheEntry holds everything needed to reconstruct a *Result without
// re-running Haiku or the capsule fetcher. FullBundlePath is NOT cached
// because it depends on the per-call bundleDir; the cache-hit path
// rewrites prefetch.md to the new bundleDir and returns the new path.
type cacheEntry struct {
	Files            []string
	InlineCapsules   []capsule.Capsule
	InlineCandidates []anthropic.Candidate // parallel to InlineCapsules; needed for writeBundle on hit
	Overflow         []anthropic.Candidate
	HaikuLatency     time.Duration
	CandidateCount   int
	InlineCount      int
	OverflowCount    int
	Truncated        bool
}

// predictorCache is a thread-safe LRU cache keyed by sha256(task+":"+repoRoot).
//
// Capacity is fixed at construction. On Put-when-full the least-recently-used
// entry is evicted. On Get the entry is bumped to the front (most-recent).
type predictorCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	order    *list.List // front = newest, back = oldest
}

// cacheListEntry is what's stored in the list — pairs the key with the
// entry so eviction (which works off the list's back) can also remove
// the corresponding map entry.
type cacheListEntry struct {
	key   string
	entry *cacheEntry
}

// newPredictorCache returns an empty LRU with the given capacity.
// A capacity of 0 or less is treated as a no-op cache (Put silently
// drops and Get always misses), which keeps construction safe even if
// a caller passes an invalid bound.
func newPredictorCache(capacity int) *predictorCache {
	return &predictorCache{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Get returns the cached entry for key (bumping it to most-recent) or
// nil,false on miss.
func (c *predictorCache) Get(key string) (*cacheEntry, bool) {
	if c == nil || c.capacity <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheListEntry).entry, true
}

// Put inserts or overwrites the entry for key. If the cache is at
// capacity, the least-recently-used entry is evicted.
func (c *predictorCache) Put(key string, entry *cacheEntry) {
	if c == nil || c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		// Overwrite + bump to front.
		el.Value.(*cacheListEntry).entry = entry
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheListEntry{key: key, entry: entry})
	c.items[key] = el
	if c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheListEntry).key)
		}
	}
}

// cacheKey derives a stable key for the (task, repoRoot) pair. Same task
// in the same repo → same key; different repo with the same task gets
// a separate key because listRepoFiles output differs and a stale cached
// candidate list would point at the wrong tree.
func cacheKey(task, repoRoot string) string {
	h := sha256.New()
	h.Write([]byte(task))
	h.Write([]byte{':'})
	h.Write([]byte(repoRoot))
	return hex.EncodeToString(h.Sum(nil))
}
