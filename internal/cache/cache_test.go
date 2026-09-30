package cache

import (
	"testing"
	"time"
)

func TestCache_GetPut(t *testing.T) {
	store := New[string](16, time.Minute)
	if _, ok := store.Get("missing"); ok {
		t.Fatal("empty cache reported a hit")
	}
	store.Put("a", "1")
	if value, ok := store.Get("a"); !ok || value != "1" {
		t.Fatalf("Get(a) = %q, %v; want 1, true", value, ok)
	}
	if got := store.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
}

func TestCache_TTLExpiresOnRead(t *testing.T) {
	store := New[string](16, time.Millisecond)
	store.Put("a", "1")
	time.Sleep(5 * time.Millisecond)
	if _, ok := store.Get("a"); ok {
		t.Fatal("expired entry still served")
	}
	if got := store.Size(); got != 0 {
		t.Fatalf("expired entry not evicted on read, Size = %d", got)
	}
}

func TestCache_DisabledWhenTTLNonPositive(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		store := New[string](16, ttl)
		store.Put("a", "1")
		if _, ok := store.Get("a"); ok {
			t.Fatalf("ttl=%v should disable the cache", ttl)
		}
		if got := store.Size(); got != 0 {
			t.Fatalf("ttl=%v cached %d entries", ttl, got)
		}
	}
}

func TestCache_CapacityOverflowClears(t *testing.T) {
	store := New[string](2, time.Minute)
	store.Put("a", "1")
	store.Put("b", "2")
	store.Put("c", "3")
	if _, ok := store.Get("a"); ok {
		t.Fatal("capacity overflow kept an evicted entry")
	}
	if _, ok := store.Get("c"); !ok {
		t.Fatal("newest entry lost after capacity overflow")
	}
	if got := store.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
}

func TestCache_EvictAndClear(t *testing.T) {
	store := New[string](16, time.Minute)
	store.Put("a", "1")
	store.Put("b", "2")
	store.Evict("a")
	store.Evict("a")
	if _, ok := store.Get("a"); ok {
		t.Fatal("evicted entry still served")
	}
	if got := store.Size(); got != 1 {
		t.Fatalf("Size after Evict = %d, want 1", got)
	}
	store.Clear()
	if got := store.Size(); got != 0 {
		t.Fatalf("Size after Clear = %d, want 0", got)
	}
	if _, ok := store.Get("b"); ok {
		t.Fatal("cleared entry still served")
	}
}
