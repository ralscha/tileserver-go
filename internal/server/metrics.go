package server

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

type metrics struct {
	requests       atomic.Uint64
	responses2xx   atomic.Uint64
	responses3xx   atomic.Uint64
	responses4xx   atomic.Uint64
	responses5xx   atomic.Uint64
	responseBytes  atomic.Uint64
	tileRequests   atomic.Uint64
	tileNotFound   atomic.Uint64
	databaseErrors atomic.Uint64
	coalesced      atomic.Uint64
	durationUS     atomic.Uint64
	inFlight       atomic.Int64
}

type observedWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *observedWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (w *observedWriter) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(reader)
		w.bytes += n
		return n, err
	}
	n, err := io.Copy(struct{ io.Writer }{w.ResponseWriter}, reader)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach optional interfaces implemented by
// the underlying writer without duplicating Flusher, Hijacker, and Pusher here.
func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (m *metrics) record(status int, bytes int64, duration time.Duration) {
	switch status / 100 {
	case 2:
		m.responses2xx.Add(1)
	case 3:
		m.responses3xx.Add(1)
	case 4:
		m.responses4xx.Add(1)
	case 5:
		m.responses5xx.Add(1)
	}
	m.responseBytes.Add(uint64(max(0, bytes)))
	m.durationUS.Add(uint64(max(0, duration.Microseconds())))
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	stats := s.cache.Stats()
	requests := s.metrics.requests.Load()
	durationSeconds := float64(s.metrics.durationUS.Load()) / 1_000_000
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	values := []struct {
		name  string
		help  string
		type_ string
		value string
	}{
		{"tileserver_http_requests_total", "Total HTTP requests.", "counter", strconv.FormatUint(requests, 10)},
		{"tileserver_http_responses_2xx_total", "HTTP responses with a 2xx status.", "counter", strconv.FormatUint(s.metrics.responses2xx.Load(), 10)},
		{"tileserver_http_responses_3xx_total", "HTTP responses with a 3xx status.", "counter", strconv.FormatUint(s.metrics.responses3xx.Load(), 10)},
		{"tileserver_http_responses_4xx_total", "HTTP responses with a 4xx status.", "counter", strconv.FormatUint(s.metrics.responses4xx.Load(), 10)},
		{"tileserver_http_responses_5xx_total", "HTTP responses with a 5xx status.", "counter", strconv.FormatUint(s.metrics.responses5xx.Load(), 10)},
		{"tileserver_http_response_bytes_total", "Response body bytes written.", "counter", strconv.FormatUint(s.metrics.responseBytes.Load(), 10)},
		{"tileserver_http_request_duration_seconds_total", "Cumulative HTTP request duration.", "counter", strconv.FormatFloat(durationSeconds, 'f', 6, 64)},
		{"tileserver_http_requests_in_flight", "Currently executing HTTP requests.", "gauge", strconv.FormatInt(s.metrics.inFlight.Load(), 10)},
		{"tileserver_tile_requests_total", "Total tile requests.", "counter", strconv.FormatUint(s.metrics.tileRequests.Load(), 10)},
		{"tileserver_tile_not_found_total", "Tile requests that did not find data.", "counter", strconv.FormatUint(s.metrics.tileNotFound.Load(), 10)},
		{"tileserver_database_errors_total", "Tile database errors.", "counter", strconv.FormatUint(s.metrics.databaseErrors.Load(), 10)},
		{"tileserver_coalesced_requests_total", "Tile loads shared with an in-flight request.", "counter", strconv.FormatUint(s.metrics.coalesced.Load(), 10)},
		{"tileserver_cache_hits_total", "In-memory response cache hits.", "counter", strconv.FormatUint(stats.Hits, 10)},
		{"tileserver_cache_misses_total", "In-memory response cache misses.", "counter", strconv.FormatUint(stats.Misses, 10)},
		{"tileserver_cache_entries", "Current in-memory response cache entries.", "gauge", strconv.Itoa(stats.Entries)},
		{"tileserver_cache_bytes", "Approximate in-memory response cache bytes.", "gauge", strconv.FormatInt(stats.Bytes, 10)},
		{"tileserver_sources", "Open tile sources.", "gauge", strconv.Itoa(len(s.sources))},
	}
	for _, value := range values {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", value.name, value.help, value.name, value.type_, value.name, value.value)
	}
}
