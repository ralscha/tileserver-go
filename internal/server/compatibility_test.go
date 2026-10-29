package server

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ralscha/tileserver-go/internal/config"
	"github.com/ralscha/tileserver-go/internal/mbtiles"
)

func TestBasePathPreservesRoutingAndRedirects(t *testing.T) {
	tileServer := newTestServerWithConfig(t, []byte("tile"), func(cfg *config.Config) {
		cfg.BasePath = "/maps"
	})
	handler := tileServer.Handler()
	for _, test := range []struct {
		path     string
		status   int
		location string
	}{
		{"/maps", http.StatusOK, ""},
		{"/maps/", http.StatusOK, ""},
		{"/maps/%64ata/openmaptiles/1/1/0.pbf", http.StatusOK, ""},
		{"/maps/data%2Fopenmaptiles/1/1/0.pbf", http.StatusNotFound, ""},
		{"/maps/data//openmaptiles.json?keep=1", http.StatusTemporaryRedirect, "/maps/data/openmaptiles.json?keep=1"},
		{"/maps/./data/openmaptiles.json", http.StatusTemporaryRedirect, "/maps/data/openmaptiles.json"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := request(t, handler, http.MethodGet, test.path, nil)
			defer func() { _ = response.Body.Close() }()
			assertStatus(t, response, test.status)
			if got := response.Header.Get("Location"); got != test.location {
				t.Fatalf("Location = %q, want %q", got, test.location)
			}
		})
	}
}

func TestRepeatedAcceptEncodingFields(t *testing.T) {
	tileServer := newTestServer(t, []byte("tile"))
	handler := tileServer.Handler()
	for _, path := range []string{
		"/data/openmaptiles/1/1/0.pbf",
		"/data/openmaptiles/1/1/0.grid.json",
		wmtsTilePath(nil),
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Add("Accept-Encoding", "identity;q=0")
			req.Header.Add("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("split encoding fields: status=%d headers=%v", response.Code, response.Header())
			}
		})
	}
}

func TestNullUTFGridReturnsLoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null-grid.mbtiles")
	createMBTiles(t, path, []byte("tile"))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, updateErr := db.Exec(`UPDATE grids SET grid = 'null'`)
	closeErr := db.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("prepare null grid: update=%v close=%v", updateErr, closeErr)
	}
	store, err := mbtiles.Open(context.Background(), "null-grid", path, mbtiles.Options{MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var server Server
	if _, err := server.loadGrid(context.Background(), store, 1, 1, 0, false); err == nil || !strings.Contains(err.Error(), "object") {
		t.Fatalf("loadGrid error = %v, want invalid object error", err)
	}
}

func TestGzipRoundTripAndOutputOwnership(t *testing.T) {
	for range 8 {
		t.Run("round trip", func(t *testing.T) {
			t.Parallel()
			payload := bytes.Repeat([]byte(t.Name()), 1024)
			compressed, err := compressGzip(payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range [][]byte{nil, []byte("other tile"), bytes.Repeat([]byte("x"), 128<<10)} {
				encoded, err := compressGzip(next)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := expandGzip(encoded)
				if err != nil || !bytes.Equal(decoded, next) {
					t.Fatalf("gzip round trip failed: %v", err)
				}
			}
			decoded, err := expandGzip(compressed)
			if err != nil || !bytes.Equal(decoded, payload) {
				t.Fatalf("later compression modified retained output: %v", err)
			}
		})
	}
}

func BenchmarkCompressGzip(b *testing.B) {
	payload := bytes.Repeat([]byte("vector tile feature with properties "), 1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if _, err := compressGzip(payload); err != nil {
			b.Fatal(err)
		}
	}
}
