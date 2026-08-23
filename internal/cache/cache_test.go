package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCacheEvictsAndTracksStats(t *testing.T) {
	cache := New(256 * 1024)
	value := Value{Data: make([]byte, 2048), ETag: `"etag"`, ContentType: "application/octet-stream"}
	for i := range 256 {
		cache.Add(Key{Source: fmt.Sprintf("tile-%d", i)}, value)
	}
	stats := cache.Stats()
	if stats.Bytes > 256*1024 {
		t.Fatalf("cache uses %d bytes, exceeds capacity", stats.Bytes)
	}
	if stats.Entries == 0 || stats.Entries >= 256 {
		t.Fatalf("unexpected entry count after eviction: %d", stats.Entries)
	}
	if _, ok := cache.Get(Key{Source: "tile-255"}); !ok {
		t.Fatal("most recently inserted entry was evicted")
	}
	if _, ok := cache.Get(Key{Source: "missing"}); ok {
		t.Fatal("missing cache entry reported as present")
	}
	stats = cache.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("unexpected hit/miss stats: %+v", stats)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	cache := New(4 << 20)
	var workers sync.WaitGroup
	for worker := range 32 {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := range 1000 {
				name := fmt.Sprintf("%d/%d", worker, i%64)
				key := Key{Source: name}
				cache.Add(key, Value{Data: []byte(name)})
				cache.Get(key)
			}
		}(worker)
	}
	workers.Wait()
	if stats := cache.Stats(); stats.Entries == 0 {
		t.Fatal("cache is empty after concurrent writes")
	}
}

func TestHashKeyDistributesCorrelatedCoordinates(t *testing.T) {
	const keyCount = 4096
	counts := make([]int, shardCount)
	for coordinate := range keyCount {
		key := NewTileKey("openmaptiles", 12, coordinate, coordinate, true)
		counts[hashKey(key)&(shardCount-1)]++
	}
	nonempty := 0
	maximum := 0
	for _, count := range counts {
		if count > 0 {
			nonempty++
		}
		maximum = max(maximum, count)
	}
	if nonempty < shardCount*3/4 || maximum > keyCount/shardCount*2 {
		t.Fatalf("poor shard distribution: nonempty=%d/%d maximum=%d counts=%v", nonempty, shardCount, maximum, counts)
	}
}

func BenchmarkCacheHitParallel(b *testing.B) {
	cache := New(64 << 20)
	key := NewTileKey("openmaptiles", 12, 2200, 1400, true)
	cache.Add(key, Value{Data: make([]byte, 32<<10), ETag: `"etag"`})
	b.ReportAllocs()
	b.SetBytes(32 << 10)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, ok := cache.Get(key); !ok {
				b.Fatal("cache miss")
			}
		}
	})
}

func BenchmarkCacheHitParallelSpread(b *testing.B) {
	const keyCount = 4096
	cache := New(64 << 20)
	keys := make([]Key, keyCount)
	value := Value{Data: make([]byte, 4<<10), ETag: `"etag"`}
	for i := range keys {
		keys[i] = NewTileKey("openmaptiles", 12, i, i, true)
		cache.Add(keys[i], value)
	}
	var workers atomic.Uint64
	b.ReportAllocs()
	b.SetBytes(4 << 10)
	b.RunParallel(func(pb *testing.PB) {
		index := workers.Add(1) * 64
		for pb.Next() {
			if _, ok := cache.Get(keys[index&(keyCount-1)]); !ok {
				b.Fatal("cache miss")
			}
			index++
		}
	})
}
