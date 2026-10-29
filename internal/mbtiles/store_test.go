package mbtiles

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestOpenPathsWithURICharacters(t *testing.T) {
	names := []string{"map#2026.mbtiles", "map%2F2026.mbtiles", "Zürich map.mbtiles"}
	if runtime.GOOS != "windows" {
		names = append(names, "map?mode=rw.mbtiles")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			db, originalPath := createTestDatabase(t)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(filepath.Dir(originalPath), name)
			if err := os.Rename(originalPath, path); err != nil {
				t.Fatal(err)
			}
			store, err := Open(t.Context(), "test", path, Options{MaxConnections: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			data, err := store.Tile(t.Context(), 4, 1, 0)
			if err != nil || string(data) != "tile" {
				t.Fatalf("Tile = %q, %v; want tile, nil", data, err)
			}
			if _, err := store.db.ExecContext(t.Context(), `DELETE FROM tiles`); err == nil {
				t.Fatal("archive opened for writing")
			}
		})
	}
}

func TestOpenDerivesMissingZooms(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
		minZoom  int
		maxZoom  int
	}{
		{name: "both missing", minZoom: 1, maxZoom: 4},
		{name: "minimum supplied", metadata: `INSERT INTO metadata VALUES ('minzoom', '0')`, minZoom: 0, maxZoom: 4},
		{name: "maximum supplied", metadata: `INSERT INTO metadata VALUES ('maxzoom', '8')`, minZoom: 1, maxZoom: 8},
		{name: "empty archive", metadata: `DELETE FROM tiles`, minZoom: 0, maxZoom: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, path := createTestDatabase(t)
			if test.metadata != "" {
				if _, err := db.ExecContext(t.Context(), test.metadata); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(t.Context(), "test", path, Options{MaxConnections: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			meta := store.Metadata()
			if meta.MinZoom != test.minZoom || meta.MaxZoom != test.maxZoom {
				t.Fatalf("zoom range = %d..%d, want %d..%d", meta.MinZoom, meta.MaxZoom, test.minZoom, test.maxZoom)
			}
		})
	}
}

func BenchmarkLoadMetadataDerivedZooms(b *testing.B) {
	db, _ := createTestDatabase(b)
	if _, err := db.ExecContext(b.Context(), `
		WITH RECURSIVE sequence(n) AS (VALUES(0) UNION ALL SELECT n + 1 FROM sequence WHERE n < 99999)
		INSERT INTO tiles SELECT 14, n % 256, n / 256, 'tile' FROM sequence
	`); err != nil {
		b.Fatal(err)
	}
	store := &Store{id: "test", db: db}
	b.ReportAllocs()
	for b.Loop() {
		if err := store.loadMetadata(b.Context()); err != nil {
			b.Fatal(err)
		}
		if store.meta.MinZoom != 1 || store.meta.MaxZoom != 14 {
			b.Fatal("unexpected zoom range")
		}
	}
}

func createTestDatabase(t testing.TB) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.mbtiles")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`CREATE UNIQUE INDEX tile_index ON tiles (zoom_level, tile_column, tile_row)`,
		`INSERT INTO metadata VALUES ('name', 'Test'), ('format', 'png')`,
		`INSERT INTO tiles VALUES (1, 0, 0, 'tile'), (4, 1, 15, 'tile')`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

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
