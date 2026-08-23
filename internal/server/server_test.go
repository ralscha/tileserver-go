package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tilecache "github.com/ralscha/tileserver-go/internal/cache"
	"github.com/ralscha/tileserver-go/internal/config"
	_ "modernc.org/sqlite"
)

func TestOpenMapTilesHTTPCompatibility(t *testing.T) {
	tilePayload := []byte{0x1a, 0x03, 'm', 'v', 't'}
	server := newTestServer(t, tilePayload)
	handler := server.Handler()

	t.Run("TileJSON", func(t *testing.T) {
		response := request(t, handler, http.MethodGet, "/data/openmaptiles.json", nil)
		assertStatus(t, response, http.StatusOK)
		var doc map[string]any
		decodeJSON(t, response.Body, &doc)
		if doc["tilejson"] != "3.0.0" || doc["format"] != "pbf" || doc["scheme"] != "xyz" {
			t.Fatalf("unexpected TileJSON: %#v", doc)
		}
		tiles := doc["tiles"].([]any)
		if !strings.Contains(tiles[0].(string), "/data/openmaptiles/{z}/{x}/{y}.pbf") {
			t.Fatalf("unexpected tile template: %v", tiles)
		}
		if _, ok := doc["vector_layers"]; !ok {
			t.Fatal("vector_layers missing from TileJSON")
		}
	})

	t.Run("gzip and identity tile variants", func(t *testing.T) {
		response := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "gzip"})
		assertStatus(t, response, http.StatusOK)
		if response.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("expected gzip content encoding, got %q", response.Header.Get("Content-Encoding"))
		}
		compressed := readAll(t, response.Body)
		reader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			t.Fatal(err)
		}
		if expanded := readAll(t, reader); !bytes.Equal(expanded, tilePayload) {
			t.Fatalf("unexpected expanded tile: %x", expanded)
		}

		identity := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.mvt", map[string]string{"Accept-Encoding": "identity"})
		assertStatus(t, identity, http.StatusOK)
		if identity.Header.Get("Content-Encoding") != "" {
			t.Fatalf("identity response is encoded: %q", identity.Header.Get("Content-Encoding"))
		}
		if body := readAll(t, identity.Body); !bytes.Equal(body, tilePayload) {
			t.Fatalf("unexpected tile: %x", body)
		}
	})

	t.Run("pre-rendered raster tile", func(t *testing.T) {
		response := request(t, handler, http.MethodGet, "/data/raster/0/0/0.png", nil)
		assertStatus(t, response, http.StatusOK)
		if contentType := response.Header.Get("Content-Type"); contentType != "image/png" {
			t.Fatalf("unexpected raster content type: %q", contentType)
		}
		if body := readAll(t, response.Body); !bytes.Equal(body, []byte("PNG-test")) {
			t.Fatalf("unexpected raster data: %q", body)
		}
	})

	t.Run("HTTP validators HEAD range and CORS", func(t *testing.T) {
		first := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "identity", "Origin": "https://client.example"})
		assertStatus(t, first, http.StatusOK)
		etag := first.Header.Get("ETag")
		if etag == "" || first.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("missing caching/CORS headers: %#v", first.Header)
		}
		conditional := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "identity", "If-None-Match": etag})
		assertStatus(t, conditional, http.StatusNotModified)
		head := request(t, handler, http.MethodHead, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "identity"})
		assertStatus(t, head, http.StatusOK)
		if body := readAll(t, head.Body); len(body) != 0 {
			t.Fatalf("HEAD returned %d body bytes", len(body))
		}
		ranged := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "identity", "Range": "bytes=0-1"})
		assertStatus(t, ranged, http.StatusPartialContent)
		if body := readAll(t, ranged.Body); !bytes.Equal(body, tilePayload[:2]) {
			t.Fatalf("unexpected range: %x", body)
		}
	})

	t.Run("WMTS REST and KVP", func(t *testing.T) {
		capabilities := request(t, handler, http.MethodGet, "/wmts?SERVICE=WMTS&REQUEST=GetCapabilities", nil)
		assertStatus(t, capabilities, http.StatusOK)
		if body := string(readAll(t, capabilities.Body)); !strings.Contains(body, "openmaptiles") || !strings.Contains(body, "WebMercatorQuad") {
			t.Fatalf("unexpected capabilities: %s", body)
		}
		kvp := request(t, handler, http.MethodGet, "/wmts?service=WMTS&request=GetTile&layer=openmaptiles&tilematrix=1&tilecol=1&tilerow=0", map[string]string{"Accept-Encoding": "identity"})
		assertStatus(t, kvp, http.StatusOK)
		if body := readAll(t, kvp.Body); !bytes.Equal(body, tilePayload) {
			t.Fatalf("unexpected KVP tile: %x", body)
		}
	})

	t.Run("UTFGrid with joined feature data", func(t *testing.T) {
		response := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.grid.json", map[string]string{"Accept-Encoding": "identity"})
		assertStatus(t, response, http.StatusOK)
		var grid map[string]any
		decodeJSON(t, response.Body, &grid)
		feature := grid["data"].(map[string]any)["water"].(map[string]any)
		if feature["name"] != "Lake" {
			t.Fatalf("unexpected UTFGrid data: %#v", grid)
		}
	})

	catalog := request(t, handler, http.MethodGet, "/", nil)
	assertStatus(t, catalog, http.StatusOK)
	var catalogDocument map[string]any
	decodeJSON(t, catalog.Body, &catalogDocument)
	if catalogDocument["name"] != "tileserver-go" || len(catalogDocument["sources"].([]any)) != 2 {
		t.Fatalf("unexpected root catalog: %#v", catalogDocument)
	}
	if _, containsStyles := catalogDocument["styles"]; containsStyles {
		t.Fatalf("tile catalog contains client-specific styles: %#v", catalogDocument)
	}

	for _, path := range []string{"/health", "/healthz", "/readyz", "/status.json", "/metrics", "/data.json", "/index.json"} {
		response := request(t, handler, http.MethodGet, path, nil)
		assertStatus(t, response, http.StatusOK)
	}
	ready := request(t, handler, http.MethodGet, "/readyz", nil)
	if ready.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("readiness Cache-Control = %q, want no-store", ready.Header.Get("Cache-Control"))
	}
	var readiness map[string]any
	decodeJSON(t, ready.Body, &readiness)
	if readiness["status"] != "ready" || readiness["sources"] != float64(2) {
		t.Fatalf("unexpected readiness response: %#v", readiness)
	}
	for _, path := range []string{"/styles.json", "/styles/basic/style.json", "/fonts.json", "/files/overlay.geojson", "/_assets/viewer.js"} {
		response := request(t, handler, http.MethodGet, path, nil)
		assertStatus(t, response, http.StatusNotFound)
	}
	missing := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/1.pbf", nil)
	assertStatus(t, missing, http.StatusNotFound)
}

func TestNewRejectsEmptySourceSet(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	_, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "no MBTiles sources found") {
		t.Fatalf("New() error = %v, want an empty-source error", err)
	}
}

func TestHostRestrictionAndBasePath(t *testing.T) {
	tileServer := newTestServerWithConfig(t, []byte("tile"), func(cfg *config.Config) {
		cfg.AllowedHosts = []string{"maps.example.test"}
		cfg.BasePath = "/maps"
	})
	handler := tileServer.Handler()

	good := httptest.NewRequest(http.MethodGet, "http://maps.example.test/maps/health", nil)
	good.Host = "maps.example.test"
	goodResponse := httptest.NewRecorder()
	handler.ServeHTTP(goodResponse, good)
	if goodResponse.Code != http.StatusOK {
		t.Fatalf("allowed host/base path returned %d", goodResponse.Code)
	}

	catalog := httptest.NewRequest(http.MethodGet, "http://maps.example.test/maps/", nil)
	catalogResponse := httptest.NewRecorder()
	handler.ServeHTTP(catalogResponse, catalog)
	if catalogResponse.Code != http.StatusOK || !strings.Contains(catalogResponse.Body.String(), `http://maps.example.test/maps/data/openmaptiles.json`) {
		t.Fatalf("catalog did not preserve the base path: status=%d body=%s", catalogResponse.Code, catalogResponse.Body.String())
	}

	bad := httptest.NewRequest(http.MethodGet, "http://evil.example/maps/health", nil)
	bad.Host = "evil.example"
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusMisdirectedRequest {
		t.Fatalf("disallowed host returned %d", badResponse.Code)
	}
}

func TestRasterTileUsesOneCacheEntryForAllAcceptEncodings(t *testing.T) {
	tileServer := newTestServer(t, []byte("tile"))
	handler := tileServer.Handler()

	compressed := request(t, handler, http.MethodGet, "/data/raster/0/0/0.png", map[string]string{"Accept-Encoding": "gzip"})
	assertStatus(t, compressed, http.StatusOK)
	if encoding := compressed.Header.Get("Content-Encoding"); encoding != "" {
		t.Fatalf("raster response has unexpected content encoding %q", encoding)
	}
	_ = readAll(t, compressed.Body)
	afterFirst := tileServer.cache.Stats()

	identity := request(t, handler, http.MethodGet, "/data/raster/0/0/0.png", map[string]string{"Accept-Encoding": "identity"})
	assertStatus(t, identity, http.StatusOK)
	_ = readAll(t, identity.Body)
	afterSecond := tileServer.cache.Stats()

	if afterFirst.Entries != 1 || afterSecond.Entries != 1 {
		t.Fatalf("raster variants used multiple cache entries: first=%+v second=%+v", afterFirst, afterSecond)
	}
	if afterSecond.Hits != afterFirst.Hits+1 {
		t.Fatalf("identity request did not reuse the raster cache entry: first=%+v second=%+v", afterFirst, afterSecond)
	}
}

func TestMissingTileAndGridResponsesAreCached(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "tile", path: "/data/openmaptiles/1/0/0.pbf"},
		{name: "grid", path: "/data/openmaptiles/1/0/0.grid.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tileServer := newTestServer(t, []byte("tile"))
			handler := tileServer.Handler()

			first := request(t, handler, http.MethodGet, test.path, map[string]string{"Accept-Encoding": "identity"})
			assertStatus(t, first, http.StatusNotFound)
			_ = readAll(t, first.Body)
			afterFirst := tileServer.cache.Stats()

			second := request(t, handler, http.MethodGet, test.path, map[string]string{"Accept-Encoding": "identity"})
			assertStatus(t, second, http.StatusNotFound)
			_ = readAll(t, second.Body)
			afterSecond := tileServer.cache.Stats()

			if afterFirst.Entries != 1 || afterSecond.Entries != 1 {
				t.Fatalf("negative response was not retained: first=%+v second=%+v", afterFirst, afterSecond)
			}
			if afterSecond.Hits != afterFirst.Hits+1 {
				t.Fatalf("second negative response missed the cache: first=%+v second=%+v", afterFirst, afterSecond)
			}
		})
	}
}

func TestUTFGridRepresentationsAreCached(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			tileServer := newTestServer(t, []byte("tile"))
			handler := tileServer.Handler()
			headers := map[string]string{"Accept-Encoding": encoding}

			first := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.grid.json", headers)
			assertStatus(t, first, http.StatusOK)
			if vary := first.Header.Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
				t.Fatalf("UTFGrid response does not vary by encoding: %q", vary)
			}
			if got := first.Header.Get("Content-Encoding"); (encoding == "gzip") != (got == "gzip") {
				t.Fatalf("content encoding = %q for %q request", got, encoding)
			}
			etag := first.Header.Get("ETag")
			_ = readAll(t, first.Body)
			afterFirst := tileServer.cache.Stats()

			second := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.grid.json", headers)
			assertStatus(t, second, http.StatusOK)
			_ = readAll(t, second.Body)
			afterSecond := tileServer.cache.Stats()

			if etag == "" || second.Header.Get("ETag") != etag {
				t.Fatalf("cached UTFGrid ETag changed: first=%q second=%q", etag, second.Header.Get("ETag"))
			}
			if afterFirst.Entries != 1 || afterSecond.Entries != 1 || afterSecond.Hits != afterFirst.Hits+1 {
				t.Fatalf("UTFGrid response was not cached: first=%+v second=%+v", afterFirst, afterSecond)
			}
		})
	}
}

func TestMetricsCanBeDisabled(t *testing.T) {
	tileServer := newTestServer(t, []byte("tile"))
	tileServer.cfg.Observability.Metrics = false
	handler := tileServer.Handler()

	response := request(t, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "identity"})
	assertStatus(t, response, http.StatusOK)
	_ = readAll(t, response.Body)
	metricsResponse := request(t, handler, http.MethodGet, "/metrics", nil)
	assertStatus(t, metricsResponse, http.StatusNotFound)

	if tileServer.metrics.requests.Load() != 0 || tileServer.metrics.tileRequests.Load() != 0 || tileServer.metrics.responses2xx.Load() != 0 {
		t.Fatal("metrics were collected while observability.metrics was disabled")
	}
}

func TestObservabilityUsesOneResponseWriter(t *testing.T) {
	tileServer := newTestServer(t, []byte("tile"))
	tileServer.cfg.Observability.Metrics = true
	tileServer.cfg.Observability.AccessLog = true
	observedDepth := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for {
			observed, ok := w.(*observedWriter)
			if !ok {
				break
			}
			observedDepth++
			w = observed.ResponseWriter
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := tileServer.observabilityMiddleware(inner)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.test/health", nil))

	if observedDepth != 1 {
		t.Fatalf("observability installed %d response-writer wrappers, want 1", observedDepth)
	}
	if tileServer.metrics.requests.Load() != 1 || tileServer.metrics.responses2xx.Load() != 1 {
		t.Fatal("combined observability wrapper did not record metrics")
	}
}

func TestMetadataResponsesArePrecomputedAndBounded(t *testing.T) {
	t.Run("fixed public URL is precomputed", func(t *testing.T) {
		tileServer := newTestServerWithConfig(t, []byte("tile"), func(cfg *config.Config) {
			cfg.PublicURL = "https://maps.example.test"
			cfg.Observability.Metrics = false
		})
		tileServer.metadataResponses.mu.Lock()
		entries := len(tileServer.metadataResponses.entries)
		tileServer.metadataResponses.mu.Unlock()
		if entries != 1 {
			t.Fatalf("metadata cache contains %d entries after startup, want 1", entries)
		}
		firstBundle, err := tileServer.metadataForBaseURL("https://maps.example.test")
		if err != nil {
			t.Fatal(err)
		}
		secondBundle, err := tileServer.metadataForBaseURL("https://maps.example.test")
		if err != nil {
			t.Fatal(err)
		}
		if firstBundle != secondBundle {
			t.Fatal("fixed metadata bundle was rebuilt")
		}

		response := request(t, tileServer.Handler(), http.MethodGet, "/data/openmaptiles.json", nil)
		assertStatus(t, response, http.StatusOK)
		if body := string(readAll(t, response.Body)); !strings.Contains(body, "https://maps.example.test/data/openmaptiles/") {
			t.Fatalf("precomputed TileJSON has wrong base URL: %s", body)
		}
	})

	t.Run("dynamic host cache is bounded", func(t *testing.T) {
		tileServer := newTestServerWithConfig(t, []byte("tile"), func(cfg *config.Config) {
			cfg.Observability.Metrics = false
		})
		handler := tileServer.Handler()
		for i := range metadataBaseURLCacheEntries + 4 {
			req := httptest.NewRequest(http.MethodGet, "http://example.test/data/openmaptiles.json", nil)
			req.Host = fmt.Sprintf("host-%d.example.test", i)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("dynamic metadata request %d returned %d", i, recorder.Code)
			}
		}
		tileServer.metadataResponses.mu.Lock()
		entries := len(tileServer.metadataResponses.entries)
		tileServer.metadataResponses.mu.Unlock()
		if entries != metadataBaseURLCacheEntries {
			t.Fatalf("dynamic metadata cache contains %d entries, want %d", entries, metadataBaseURLCacheEntries)
		}
	})
}

func TestRequestPoliciesAreNormalized(t *testing.T) {
	hosts := newHostPolicy([]string{" Maps.Example.Test:8443 ", "*.Tiles.Example.Test"})
	for _, host := range []string{"maps.example.test:8443", "MAPS.EXAMPLE.TEST:8443", "a.tiles.example.test"} {
		if !hosts.allows(host) {
			t.Errorf("normalized host policy rejected %q", host)
		}
	}
	for _, host := range []string{"tiles.example.test", "evil.example.test", ""} {
		if hosts.allows(host) {
			t.Errorf("normalized host policy accepted %q", host)
		}
	}

	cors := newCORSPolicy([]string{" HTTPS://Client.Example.Test "})
	if !cors.allows("https://client.example.test") {
		t.Fatal("normalized CORS policy rejected configured origin")
	}
	if cors.allows("https://evil.example.test") {
		t.Fatal("normalized CORS policy accepted unconfigured origin")
	}
	if wildcard := newCORSPolicy([]string{"*"}); !wildcard.allowAll || !wildcard.allows("https://any.example.test") {
		t.Fatal("wildcard CORS policy does not allow arbitrary origins")
	}
}

func TestMetadataCacheConcurrentAccess(t *testing.T) {
	tileServer := newTestServerWithConfig(t, []byte("tile"), func(cfg *config.Config) {
		cfg.Observability.Metrics = false
	})
	handler := tileServer.Handler()
	var workers sync.WaitGroup
	errors := make(chan error, 32)
	for worker := range 32 {
		workers.Go(func() {
			for requestIndex := range 16 {
				req := httptest.NewRequest(http.MethodGet, "http://example.test/data/openmaptiles.json", nil)
				req.Host = fmt.Sprintf("host-%d.example.test", (worker+requestIndex)%12)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusOK {
					errors <- fmt.Errorf("metadata status %d", recorder.Code)
					return
				}
			}
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	tileServer.metadataResponses.mu.Lock()
	entries := len(tileServer.metadataResponses.entries)
	tileServer.metadataResponses.mu.Unlock()
	if entries > metadataBaseURLCacheEntries {
		t.Fatalf("dynamic metadata cache exceeded its bound: %d", entries)
	}
}

func TestProtocolHelpers(t *testing.T) {
	for _, test := range []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"br, gzip;q=0.5", true},
		{"gzip;q=0", false},
		{"*;q=1", true},
		{"identity", false},
		{"GZIP; level=1; q = 0.25", true},
		{"gzip; q = 0", false},
		{"gzip;foo=bar;q=invalid", false},
	} {
		if got := acceptsGzipEncoding(test.header); got != test.want {
			t.Errorf("acceptsGzipEncoding(%q) = %v, want %v", test.header, got, test.want)
		}
	}
	var accepted bool
	if allocations := testing.AllocsPerRun(1000, func() {
		accepted = acceptsGzipEncoding("br, gzip;q=0.5, deflate")
	}); allocations != 0 {
		t.Fatalf("acceptsGzipEncoding allocated %.2f times per call", allocations)
	}
	if !accepted {
		t.Fatal("allocation test did not negotiate gzip")
	}
}
func TestOptionsRequiresCORSPreflightHeaders(t *testing.T) {
	tileServer := newTestServer(t, []byte("tile"))
	handler := tileServer.Handler()

	plain := request(t, handler, http.MethodOptions, "/health", nil)
	if plain.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("plain OPTIONS status = %d, want %d", plain.StatusCode, http.StatusMethodNotAllowed)
	}
	preflight := request(t, handler, http.MethodOptions, "/health", map[string]string{
		"Origin":                        "https://client.example",
		"Access-Control-Request-Method": http.MethodGet,
	})
	assertStatus(t, preflight, http.StatusNoContent)
}

func BenchmarkCachedTileHTTP(b *testing.B) {
	benchmarkCachedTileHTTP(b, true)
}

func BenchmarkCachedTileHTTPMetricsDisabled(b *testing.B) {
	benchmarkCachedTileHTTP(b, false)
}

func benchmarkCachedTileHTTP(b *testing.B, metricsEnabled bool) {
	tileServer := newTestServer(b, bytes.Repeat([]byte{0x42}, 32<<10))
	tileServer.cfg.Observability.Metrics = metricsEnabled
	handler := tileServer.Handler()
	warm := request(b, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "identity"})
	assertStatus(b, warm, http.StatusOK)
	_ = readAll(b, warm.Body)
	benchmarkHTTPHandler(b, handler, "/data/openmaptiles/1/1/0.pbf", "identity", http.StatusOK, 32<<10)
}

func BenchmarkCachedTileHTTPParallel(b *testing.B) {
	tileServer := newTestServerWithConfig(b, bytes.Repeat([]byte{0x42}, 32<<10), func(cfg *config.Config) {
		cfg.Observability.Metrics = false
	})
	handler := tileServer.Handler()
	warm := request(b, handler, http.MethodGet, "/data/openmaptiles/1/1/0.pbf", map[string]string{"Accept-Encoding": "gzip"})
	assertStatus(b, warm, http.StatusOK)
	_ = readAll(b, warm.Body)
	b.ReportAllocs()
	b.SetBytes(32 << 10)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			serveBenchmarkRequest(b, handler, "/data/openmaptiles/1/1/0.pbf", "gzip", http.StatusOK)
		}
	})
}

func BenchmarkUncachedTileHTTP(b *testing.B) {
	for _, encoding := range []string{"gzip", "identity"} {
		b.Run(encoding, func(b *testing.B) {
			tileServer := newTestServerWithConfig(b, bytes.Repeat([]byte{0x42}, 32<<10), func(cfg *config.Config) {
				cfg.Observability.Metrics = false
			})
			tileServer.cache = tilecache.New(0)
			handler := tileServer.Handler()
			benchmarkHTTPHandler(b, handler, "/data/openmaptiles/1/1/0.pbf", encoding, http.StatusOK, 32<<10)
		})
	}
}

func BenchmarkCachedNegativeTileHTTP(b *testing.B) {
	tileServer := newTestServerWithConfig(b, []byte("tile"), func(cfg *config.Config) {
		cfg.Observability.Metrics = false
	})
	handler := tileServer.Handler()
	warm := request(b, handler, http.MethodGet, "/data/openmaptiles/1/0/0.pbf", map[string]string{"Accept-Encoding": "identity"})
	assertStatus(b, warm, http.StatusNotFound)
	_ = readAll(b, warm.Body)
	benchmarkHTTPHandler(b, handler, "/data/openmaptiles/1/0/0.pbf", "identity", http.StatusNotFound, 0)
}

func BenchmarkCachedUTFGridHTTP(b *testing.B) {
	tileServer := newTestServerWithConfig(b, []byte("tile"), func(cfg *config.Config) {
		cfg.Observability.Metrics = false
	})
	handler := tileServer.Handler()
	warm := request(b, handler, http.MethodGet, "/data/openmaptiles/1/1/0.grid.json", map[string]string{"Accept-Encoding": "gzip"})
	assertStatus(b, warm, http.StatusOK)
	_ = readAll(b, warm.Body)
	benchmarkHTTPHandler(b, handler, "/data/openmaptiles/1/1/0.grid.json", "gzip", http.StatusOK, 0)
}

func BenchmarkCachedMetadataHTTP(b *testing.B) {
	tileServer := newTestServerWithConfig(b, []byte("tile"), func(cfg *config.Config) {
		cfg.PublicURL = "https://maps.example.test"
		cfg.Observability.Metrics = false
	})
	handler := tileServer.Handler()
	for name, path := range map[string]string{
		"catalog":  "/",
		"tilejson": "/data/openmaptiles.json",
		"wmts":     "/wmts?SERVICE=WMTS&REQUEST=GetCapabilities",
	} {
		b.Run(name, func(b *testing.B) {
			benchmarkHTTPHandler(b, handler, path, "", http.StatusOK, 0)
		})
	}
}

func benchmarkHTTPHandler(b *testing.B, handler http.Handler, path, encoding string, wantStatus, responseBytes int) {
	b.Helper()
	b.ReportAllocs()
	if responseBytes > 0 {
		b.SetBytes(int64(responseBytes))
	}
	b.ResetTimer()
	for b.Loop() {
		serveBenchmarkRequest(b, handler, path, encoding, wantStatus)
	}
}

func serveBenchmarkRequest(b *testing.B, handler http.Handler, path, encoding string, wantStatus int) {
	b.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.test"+path, nil)
	if encoding != "" {
		req.Header.Set("Accept-Encoding", encoding)
	}
	writer := &discardResponseWriter{header: make(http.Header)}
	handler.ServeHTTP(writer, req)
	if writer.status != wantStatus {
		b.Fatalf("status %d, want %d", writer.status, wantStatus)
	}
}

type discardResponseWriter struct {
	header http.Header
	status int
	bytes  int64
}

func (w *discardResponseWriter) Header() http.Header { return w.header }

func (w *discardResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *discardResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.bytes += int64(len(data))
	return len(data), nil
}

func (w *discardResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(io.Discard, reader)
	w.bytes += n
	return n, err
}

type testingTB interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
	TempDir() string
}

func newTestServer(t testingTB, tilePayload []byte) *Server {
	return newTestServerWithConfig(t, tilePayload, nil)
}

func newTestServerWithConfig(t testingTB, tilePayload []byte, configure func(*config.Config)) *Server {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	createMBTiles(t, filepath.Join(root, "data", "openmaptiles.mbtiles"), tilePayload)
	createRasterMBTiles(t, filepath.Join(root, "data", "raster.mbtiles"))

	cfg := config.Default()
	cfg.Observability.AccessLog = false
	cfg.Cache.SizeMB = 8
	cfg.Database.MaxConnections = 4
	if configure != nil {
		configure(&cfg)
	}
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tileServer, err := New(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tileServer.Close() })
	return tileServer
}

func createRasterMBTiles(t testingTB, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for _, statement := range []string{
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`CREATE UNIQUE INDEX tile_index ON tiles (zoom_level, tile_column, tile_row)`,
		`INSERT INTO metadata(name, value) VALUES ('name', 'Raster test'), ('format', 'png'), ('minzoom', '0'), ('maxzoom', '0')`,
		`INSERT INTO tiles(zoom_level, tile_column, tile_row, tile_data) VALUES (0, 0, 0, 'PNG-test')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func createMBTiles(t testingTB, path string, tilePayload []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	statements := []string{
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`CREATE UNIQUE INDEX tile_index ON tiles (zoom_level, tile_column, tile_row)`,
		`CREATE TABLE grids (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, grid BLOB)`,
		`CREATE TABLE grid_data (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, key_name TEXT, key_json TEXT)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	metadata := map[string]string{
		"name": "OpenMapTiles test", "format": "pbf", "minzoom": "0", "maxzoom": "14",
		"bounds": "8.4,47.3,8.7,47.5", "center": "8.55,47.4,10", "attribution": "© OpenMapTiles © OpenStreetMap contributors",
		"json": `{"vector_layers":[{"id":"water","fields":{"class":"String"},"minzoom":0,"maxzoom":14}]}`,
	}
	for name, value := range metadata {
		if _, err := db.Exec(`INSERT INTO metadata(name, value) VALUES (?, ?)`, name, value); err != nil {
			t.Fatal(err)
		}
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(tilePayload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// XYZ y=0 at z=1 is stored as TMS row 1.
	if _, err := db.Exec(`INSERT INTO tiles(zoom_level, tile_column, tile_row, tile_data) VALUES (1, 1, 1, ?)`, compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	var compressedGrid bytes.Buffer
	gridWriter := gzip.NewWriter(&compressedGrid)
	if _, err := gridWriter.Write([]byte(`{"grid":[" !"],"keys":["","water"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := gridWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO grids(zoom_level, tile_column, tile_row, grid) VALUES (1, 1, 1, ?)`, compressedGrid.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO grid_data(zoom_level, tile_column, tile_row, key_name, key_json) VALUES (1, 1, 1, 'water', '{"name":"Lake"}')`); err != nil {
		t.Fatal(err)
	}
}

func request(t testingTB, handler http.Handler, method, target string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, "http://example.test"+target, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder.Result()
}

func assertStatus(t testingTB, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status %d, want %d; body=%s", response.StatusCode, want, body)
	}
}

func decodeJSON(t testingTB, reader io.Reader, target any) {
	t.Helper()
	if err := json.NewDecoder(reader).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func readAll(t testingTB, reader io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
