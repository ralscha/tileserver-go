package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

const shardCount = 64

type Value struct {
	Data               []byte
	ETag               string
	ContentType        string
	ContentEncoding    string
	NotFound           bool
	VaryAcceptEncoding bool
}

// Key identifies one cached tile or UTFGrid response. Coordinates are packed
// by NewTileKey and NewGridKey.
type Key struct {
	Source      string
	Coordinates uint64
	Variant     uint8
	Kind        uint8
}

func NewTileKey(source string, z, x, y int, gzip bool) Key {
	// Tile request validation limits z to 0..30 and x/y to 0..2^30-1.
	variant := uint8(z << 1) //nolint:gosec // Validated tile coordinates fit in the target type.
	if gzip {
		variant |= 1
	}
	return Key{
		Source:      source,
		Coordinates: uint64(uint32(x))<<32 | uint64(uint32(y)), //nolint:gosec // Validated tile coordinates are non-negative and at most 30 bits.
		Variant:     variant,
	}
}

func NewGridKey(source string, z, x, y int, gzip bool) Key {
	key := NewTileKey(source, z, x, y, gzip)
	key.Kind = 1
	return key
}

type Cache struct {
	shards [shardCount]shard
	hits   atomic.Uint64
	misses atomic.Uint64
}

type shard struct {
	mu       sync.Mutex
	items    map[Key]*list.Element
	lru      list.List
	used     int64
	capacity int64
}

type entry struct {
	key   Key
	value Value
	size  int64
}

type Stats struct {
	Hits    uint64
	Misses  uint64
	Entries int
	Bytes   int64
}

func New(capacityBytes int64) *Cache {
	c := &Cache{}
	perShard := capacityBytes / shardCount
	for i := range c.shards {
		c.shards[i].capacity = perShard
		c.shards[i].items = make(map[Key]*list.Element)
	}
	return c
}

func (c *Cache) Get(key Key) (Value, bool) {
	return c.get(key, true)
}

// Peek performs a cache lookup without changing hit/miss counters. It is used
// for the second lookup inside a coalesced cache-miss critical section.
func (c *Cache) Peek(key Key) (Value, bool) {
	return c.get(key, false)
}

func (c *Cache) get(key Key, count bool) (Value, bool) {
	s := c.shard(key)
	s.mu.Lock()
	element, ok := s.items[key]
	if ok {
		s.lru.MoveToFront(element)
		value := element.Value.(*entry).value
		s.mu.Unlock()
		if count {
			c.hits.Add(1)
		}
		return value, true
	}
	s.mu.Unlock()
	if count {
		c.misses.Add(1)
	}
	return Value{}, false
}

func (c *Cache) Add(key Key, value Value) {
	s := c.shard(key)
	if s.capacity <= 0 {
		return
	}
	size := int64(len(key.Source)+len(value.Data)+len(value.ETag)+len(value.ContentType)+len(value.ContentEncoding)) + 160
	if size > s.capacity {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.items[key]; ok {
		old := element.Value.(*entry)
		s.used -= old.size
		old.value = value
		old.size = size
		s.used += size
		s.lru.MoveToFront(element)
	} else {
		e := &entry{key: key, value: value, size: size}
		s.items[key] = s.lru.PushFront(e)
		s.used += size
	}
	for s.used > s.capacity {
		last := s.lru.Back()
		if last == nil {
			break
		}
		e := last.Value.(*entry)
		delete(s.items, e.key)
		s.used -= e.size
		s.lru.Remove(last)
	}
}

func (c *Cache) Stats() Stats {
	stats := Stats{Hits: c.hits.Load(), Misses: c.misses.Load()}
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		stats.Entries += len(s.items)
		stats.Bytes += s.used
		s.mu.Unlock()
	}
	return stats
}

func (c *Cache) shard(key Key) *shard {
	return &c.shards[hashKey(key)&(shardCount-1)]
}

func hashKey(key Key) uint64 {
	hash := uint64(14695981039346656037)
	for i := 0; i < len(key.Source); i++ {
		hash ^= uint64(key.Source[i])
		hash *= 1099511628211
	}
	hash ^= key.Coordinates
	hash ^= uint64(key.Variant) << 56
	hash ^= uint64(key.Kind) << 48
	// MurmurHash3's finalizer avalanches correlated XYZ coordinates before the
	// low bits select a shard.
	hash ^= hash >> 33
	hash *= 0xff51afd7ed558ccd
	hash ^= hash >> 33
	hash *= 0xc4ceb9fe1a85ec53
	return hash ^ (hash >> 33)
}
