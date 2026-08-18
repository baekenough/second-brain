package briefing

import (
	"sync"
	"testing"
)

func dummyResult(text string) Result {
	return Result{Sentences: []Sentence{{Text: text, DocumentIDs: []string{"11111111-1111-1111-1111-111111111111"}}}}
}

// TestCacheEvictsOldest pins the bounded-memory property: a long-running server
// must not accumulate one cached briefing per distinct open-action set forever.
func TestCacheEvictsOldest(t *testing.T) {
	c := NewCache(2)
	c.Put("k1", dummyResult("one"))
	c.Put("k2", dummyResult("two"))
	c.Put("k3", dummyResult("three"))

	if _, ok := c.Get("k1"); ok {
		t.Fatal("k1 survived past capacity 2; the oldest entry must be evicted")
	}
	if _, ok := c.Get("k2"); !ok {
		t.Fatal("k2 was evicted too early")
	}
	if _, ok := c.Get("k3"); !ok {
		t.Fatal("k3 missing right after Put")
	}
}

// TestCacheGetMissAndHit pins the basic contract, including that a miss returns
// the zero Result rather than a partially populated one.
func TestCacheGetMissAndHit(t *testing.T) {
	c := NewCache(4)
	if got, ok := c.Get("absent"); ok || len(got.Sentences) != 0 {
		t.Fatalf("miss returned ok=%v result=%+v", ok, got)
	}
	c.Put("present", dummyResult("hello"))
	got, ok := c.Get("present")
	if !ok || len(got.Sentences) != 1 || got.Sentences[0].Text != "hello" {
		t.Fatalf("hit returned ok=%v result=%+v", ok, got)
	}
}

// TestCacheOverwriteKeepsOneEntry pins that re-Putting a known key replaces the
// value instead of appending a second entry that would push a live key out.
func TestCacheOverwriteKeepsOneEntry(t *testing.T) {
	c := NewCache(2)
	c.Put("k1", dummyResult("first"))
	c.Put("k1", dummyResult("second"))
	c.Put("k2", dummyResult("two"))

	got, ok := c.Get("k1")
	if !ok {
		t.Fatal("k1 evicted after being overwritten; overwrite must not consume a second slot")
	}
	if got.Sentences[0].Text != "second" {
		t.Fatalf("k1 = %q, want the overwritten value", got.Sentences[0].Text)
	}
}

// TestCacheZeroCapacity pins that a misconfigured capacity degrades to a usable
// default rather than to a cache that never hits (which would silently double
// LLM spend).
func TestCacheZeroCapacity(t *testing.T) {
	c := NewCache(0)
	c.Put("k1", dummyResult("one"))
	if _, ok := c.Get("k1"); !ok {
		t.Fatal("NewCache(0) produced a cache that never hits")
	}
}

// TestCacheIsConcurrencySafe pins the mutex. Two browser tabs hitting the
// briefing route at once is the ordinary case, not an exotic one.
func TestCacheIsConcurrencySafe(t *testing.T) {
	c := NewCache(8)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%8))
			c.Put(key, dummyResult(key))
			c.Get(key)
		}(i)
	}
	wg.Wait()
}
