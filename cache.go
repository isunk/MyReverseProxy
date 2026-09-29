package main

import (
	"sync"
	"time"
)

// expiringCache 有界、按过期时间惰性淘汰的并发缓存。
// 条目过期即在读取时清除，避免死条目长期占满容量；容量用尽时整体重建，
// 固定代价低于逐条淘汰的实现复杂度。ttl 为零或负值时缓存失效，put 不写入。
type expiringCache[V any] struct {
	mu       sync.Mutex
	entries  map[string]expiringEntry[V]
	ttl      time.Duration
	capacity int
}

type expiringEntry[V any] struct {
	value     V
	expiresAt time.Time
}

func newExpiringCache[V any](capacity int, ttl time.Duration) *expiringCache[V] {
	return &expiringCache[V]{entries: map[string]expiringEntry[V]{}, ttl: ttl, capacity: capacity}
}

// get 命中未过期条目即返回；过期条目顺手删除，容量不被死条目占满。
func (c *expiringCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !time.Now().Before(entry.expiresAt) {
		delete(c.entries, key)
		var miss V
		return miss, false
	}
	return entry.value, true
}

func (c *expiringCache[V]) put(key string, value V) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity > 0 && len(c.entries) >= c.capacity {
		c.entries = map[string]expiringEntry[V]{}
	}
	c.entries[key] = expiringEntry[V]{value: value, expiresAt: time.Now().Add(c.ttl)}
}

func (c *expiringCache[V]) evict(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *expiringCache[V]) clear() {
	c.mu.Lock()
	c.entries = map[string]expiringEntry[V]{}
	c.mu.Unlock()
}

func (c *expiringCache[V]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
