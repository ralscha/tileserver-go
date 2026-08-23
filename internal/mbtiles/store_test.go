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

func TestCoordinateMetadataRejectsNonFiniteNumbers(t *testing.T) {
	for _, value := range []string{"NaN,0,1,1", "0,-Inf,1,1", "0,0,+Inf,1"} {
		if _, ok := parseNumbers4(value); ok {
			t.Errorf("parseNumbers4(%q) accepted a non-finite number", value)
		}
	}
	for _, value := range []string{"NaN,0,1", "0,-Inf,1", "0,0,+Inf"} {
		if _, ok := parseNumbers3(value); ok {
			t.Errorf("parseNumbers3(%q) accepted a non-finite number", value)
		}
	}
}

func TestValidateVectorLayers(t *testing.T) {
	valid := []any{
		map[string]any{
			"id":          "water",
			"fields":      map[string]any{"class": "Feature class"},
			"description": "Water polygons",
			"minzoom":     float64(0),
			"maxzoom":     float64(14),
			"custom":      true,
		},
	}
	if err := validateVectorLayers(valid, 0, 14); err != nil {
		t.Fatalf("valid vector layers rejected: %v", err)
	}

	for name, value := range map[string]any{
		"not an array":         "water",
		"entry not an object":  []any{"water"},
		"missing id":           []any{map[string]any{"fields": map[string]any{}}},
		"missing fields":       []any{map[string]any{"id": "water"}},
		"field not a string":   []any{map[string]any{"id": "water", "fields": map[string]any{"class": true}}},
		"description type":     []any{map[string]any{"id": "water", "fields": map[string]any{}, "description": true}},
		"fractional minzoom":   []any{map[string]any{"id": "water", "fields": map[string]any{}, "minzoom": 1.5}},
		"maxzoom out of range": []any{map[string]any{"id": "water", "fields": map[string]any{}, "maxzoom": float64(15)}},
		"reversed zooms":       []any{map[string]any{"id": "water", "fields": map[string]any{}, "minzoom": float64(10), "maxzoom": float64(5)}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateVectorLayers(value, 0, 14); err == nil {
				t.Fatal("invalid vector layers accepted")
			}
		})
	}
}
