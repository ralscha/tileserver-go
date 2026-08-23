package mbtiles

import (
	"net/url"
	"slices"
	"testing"
)

func TestSQLiteDSNDividesPageCacheBudgetAcrossConnections(t *testing.T) {
	dsn := sqliteDSN("tiles.mbtiles", Options{
		MaxConnections: 8,
		MmapSizeMB:     256,
		PageCacheMB:    64,
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pragmas := parsed.Query()["_pragma"]
	if !slices.Contains(pragmas, "cache_size(-8192)") {
		t.Fatalf("SQLite pragmas = %q, want an 8 MiB per-connection cache", pragmas)
	}
}

func TestPageCacheKiBPerConnection(t *testing.T) {
	tests := []struct {
		name           string
		totalMB        int
		maxConnections int
		want           int64
	}{
		{name: "disabled", totalMB: 0, maxConnections: 8, want: 0},
		{name: "single connection", totalMB: 64, maxConnections: 1, want: 64 * 1024},
		{name: "pool budget", totalMB: 64, maxConnections: 32, want: 2 * 1024},
		{name: "minimum advisory", totalMB: 1, maxConnections: 2048, want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pageCacheKiBPerConnection(test.totalMB, test.maxConnections); got != test.want {
				t.Fatalf("pageCacheKiBPerConnection(%d, %d) = %d, want %d", test.totalMB, test.maxConnections, got, test.want)
			}
		})
	}
}
