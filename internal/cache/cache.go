package cache

import (
	"sync"
	"time"
)

// Cache 有界、按过期时间惰性淘汰的并发缓存。
// 条目过期即在读取时清除，避免死条目长期占满容量；容量用尽时整体重建，
// 固定代价低于逐条淘汰的实现复杂度。ttl 为零或负值时缓存失效，Put 不写入。
type Cache[V any] struct {
	mu       sync.Mutex
	entries  map[string]entry[V]
	ttl      time.Duration
	capacity int
}

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

func New[V any](capacity int, ttl time.Duration) *Cache[V] {
	return &Cache[V]{entries: map[string]entry[V]{}, ttl: ttl, capacity: capacity}
}

// Get 命中未过期条目即返回；过期条目顺手删除，容量不被死条目占满。
func (c *Cache[V]) Get(key string) (V, bool) {
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

func (c *Cache[V]) Put(key string, value V) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity > 0 && len(c.entries) >= c.capacity {
		clear(c.entries)
	}
	c.entries[key] = entry[V]{value: value, expiresAt: time.Now().Add(c.ttl)}
}

// Evict 精确删除单个条目，缓存值被判定失效时调用以触发立即重建。
func (c *Cache[V]) Evict(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Clear 清空全部条目，配置变更后调用以丢弃派生状态。
func (c *Cache[V]) Clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}

func (c *Cache[V]) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
