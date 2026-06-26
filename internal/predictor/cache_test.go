package predictor

import (
	"fmt"
	"sync"
	"testing"
)

func TestCacheGetMissingReturnsFalse(t *testing.T) {
	c := newPredictorCache(10)
	if _, ok := c.Get("nope"); ok {
		t.Fatalf("Get on empty cache returned ok=true")
	}
}

func TestCachePutGetRoundtrip(t *testing.T) {
	c := newPredictorCache(10)
	entry := &cacheEntry{Files: []string{"a.go"}, CandidateCount: 1}
	c.Put("k1", entry)
	got, ok := c.Get("k1")
	if !ok {
		t.Fatalf("Get after Put returned ok=false")
	}
	if got != entry {
		t.Fatalf("Get returned different pointer than Put stored")
	}
	if len(got.Files) != 1 || got.Files[0] != "a.go" {
		t.Errorf("Files=%v want [a.go]", got.Files)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newPredictorCache(3)
	c.Put("k1", &cacheEntry{Files: []string{"f1"}})
	c.Put("k2", &cacheEntry{Files: []string{"f2"}})
	c.Put("k3", &cacheEntry{Files: []string{"f3"}})
	// All three present.
	for _, k := range []string{"k1", "k2", "k3"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("expected %s present", k)
		}
	}
	// After the gets above, recency order is: k3 (most recent), k2, k1 (least).
	// Wait — get bumps to front, so the recency order after the loop is k3,k2,k1
	// (k1 was got last, so it's now most recent). Let's reset to make assertions clean.
	c = newPredictorCache(3)
	c.Put("k1", &cacheEntry{Files: []string{"f1"}})
	c.Put("k2", &cacheEntry{Files: []string{"f2"}})
	c.Put("k3", &cacheEntry{Files: []string{"f3"}})
	// At capacity. Insert k4 → k1 (oldest) should be evicted.
	c.Put("k4", &cacheEntry{Files: []string{"f4"}})
	if _, ok := c.Get("k1"); ok {
		t.Errorf("k1 should have been evicted as LRU")
	}
	for _, k := range []string{"k2", "k3", "k4"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s should still be present", k)
		}
	}
}

func TestCacheMoveToFrontOnHit(t *testing.T) {
	c := newPredictorCache(3)
	c.Put("k1", &cacheEntry{Files: []string{"f1"}})
	c.Put("k2", &cacheEntry{Files: []string{"f2"}})
	c.Put("k3", &cacheEntry{Files: []string{"f3"}})
	// Touch k1 — it should become most-recently-used, so k2 is now the LRU.
	if _, ok := c.Get("k1"); !ok {
		t.Fatalf("k1 missing before bump")
	}
	// Insert k4: evicts k2 (oldest), not k1.
	c.Put("k4", &cacheEntry{Files: []string{"f4"}})
	if _, ok := c.Get("k2"); ok {
		t.Errorf("k2 should have been evicted after k1 was bumped")
	}
	for _, k := range []string{"k1", "k3", "k4"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s should still be present", k)
		}
	}
}

func TestCachePutOverwritesExistingKey(t *testing.T) {
	c := newPredictorCache(3)
	c.Put("k1", &cacheEntry{Files: []string{"old"}})
	c.Put("k1", &cacheEntry{Files: []string{"new"}})
	got, ok := c.Get("k1")
	if !ok {
		t.Fatalf("k1 missing after overwrite")
	}
	if got.Files[0] != "new" {
		t.Errorf("expected overwrite to update value; got Files=%v", got.Files)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := newPredictorCache(100)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := fmt.Sprintf("g%d-k%d", g, j%20)
				c.Put(key, &cacheEntry{Files: []string{key}})
				_, _ = c.Get(key)
			}
		}(i)
	}
	wg.Wait()
}
