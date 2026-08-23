package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ralscha/tileserver-go/internal/cache"
	"github.com/ralscha/tileserver-go/internal/mbtiles"
)

const maxExpandedTileSize = 64 << 20

func (s *Server) serveTilePath(w http.ResponseWriter, r *http.Request) {
	s.serveTile(w, r, r.PathValue("id"), r.PathValue("z"), r.PathValue("x"), r.PathValue("tile"))
}

func (s *Server) serveTile(w http.ResponseWriter, r *http.Request, id, zText, xText, tileText string) {
	metricsEnabled := s.cfg.Observability.Metrics
	if metricsEnabled {
		s.metrics.tileRequests.Add(1)
	}
	store := s.sources[id]
	if store == nil {
		if metricsEnabled {
			s.metrics.tileNotFound.Add(1)
		}
		s.writeError(w, http.StatusNotFound, "source not found")
		return
	}
	z, errZ := strconv.Atoi(zText)
	x, errX := strconv.Atoi(xText)
	yText, extension, hasExtension := strings.Cut(tileText, ".")
	y, errY := strconv.Atoi(yText)
	meta := store.Metadata()
	if errZ == nil && errX == nil && errY == nil && hasExtension && extension == "grid.json" {
		s.serveGrid(w, r, store, z, x, y, tileText)
		return
	}
	if errZ != nil || errX != nil || errY != nil || !hasExtension || !extensionAllowed(extension, meta.Extension) || z < meta.MinZoom || z > meta.MaxZoom || !validXYZ(z, x, y) {
		if metricsEnabled {
			s.metrics.tileNotFound.Add(1)
		}
		s.writeError(w, http.StatusNotFound, "tile not found")
		return
	}
	// Raster MBTiles have only an identity representation. Restricting content
	// encoding negotiation to vector tiles prevents byte-identical raster data
	// from occupying both the gzip and identity cache keys.
	wantsGzip := meta.Extension == "pbf" && acceptsGzipEncoding(r.Header.Get("Accept-Encoding"))
	key := cache.NewTileKey(id, z, x, y, wantsGzip)
	value, loadErr := s.cachedResponse(r, key, func(ctx context.Context) (cache.Value, error) {
		return s.loadTile(ctx, store, z, x, y, wantsGzip)
	})
	if loadErr != nil {
		if r.Context().Err() != nil {
			return
		}
		if metricsEnabled {
			s.metrics.databaseErrors.Add(1)
		}
		s.logger.Error("load tile", "source", id, "z", z, "x", x, "y", y, "error", loadErr)
		s.writeError(w, http.StatusInternalServerError, "failed to load tile")
		return
	}
	if value.NotFound {
		if metricsEnabled {
			s.metrics.tileNotFound.Add(1)
		}
		s.writeError(w, http.StatusNotFound, "tile not found")
		return
	}
	serveCachedResponse(w, r, tileText, store.ModTime(), s.tileCacheControl, value)
}

func (s *Server) serveGrid(w http.ResponseWriter, r *http.Request, store *mbtiles.Store, z, x, y int, name string) {
	metricsEnabled := s.cfg.Observability.Metrics
	meta := store.Metadata()
	if !meta.HasUTFGrid || z < meta.MinZoom || z > meta.MaxZoom || !validXYZ(z, x, y) {
		if metricsEnabled {
			s.metrics.tileNotFound.Add(1)
		}
		s.writeError(w, http.StatusNotFound, "grid not found")
		return
	}
	wantsGzip := acceptsGzipEncoding(r.Header.Get("Accept-Encoding"))
	key := cache.NewGridKey(store.ID(), z, x, y, wantsGzip)
	value, loadErr := s.cachedResponse(r, key, func(ctx context.Context) (cache.Value, error) {
		return s.loadGrid(ctx, store, z, x, y, wantsGzip)
	})
	if loadErr != nil {
		if r.Context().Err() != nil {
			return
		}
		if metricsEnabled {
			s.metrics.databaseErrors.Add(1)
		}
		s.logger.Error("load UTFGrid", "source", store.ID(), "z", z, "x", x, "y", y, "error", loadErr)
		s.writeError(w, http.StatusInternalServerError, "failed to load grid")
		return
	}
	if value.NotFound {
		if metricsEnabled {
			s.metrics.tileNotFound.Add(1)
		}
		s.writeError(w, http.StatusNotFound, "grid not found")
		return
	}
	serveCachedResponse(w, r, name, store.ModTime(), s.tileCacheControl, value)
}

func (s *Server) cachedResponse(r *http.Request, key cache.Key, load func(context.Context) (cache.Value, error)) (cache.Value, error) {
	if value, ok := s.cache.Get(key); ok {
		return value, nil
	}
	value, loadErr, shared := s.flight.DoContext(r.Context(), key, func() (cache.Value, error) {
		if existing, exists := s.cache.Peek(key); exists {
			return existing, nil
		}
		loadContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.HTTP.WriteTimeout.Value())
		defer cancel()
		loaded, err := load(loadContext)
		if errors.Is(err, mbtiles.ErrTileNotFound) {
			loaded = cache.Value{NotFound: true}
			err = nil
		}
		if err == nil {
			s.cache.Add(key, loaded)
		}
		return loaded, err
	})
	if shared && s.cfg.Observability.Metrics {
		s.metrics.coalesced.Add(1)
	}
	return value, loadErr
}

func serveCachedResponse(w http.ResponseWriter, r *http.Request, name string, modTime time.Time, cacheControlValue string, value cache.Value) {
	w.Header().Set("Content-Type", value.ContentType)
	if value.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", value.ContentEncoding)
	}
	if value.VaryAcceptEncoding {
		w.Header().Add("Vary", "Accept-Encoding")
	}
	w.Header().Set("Cache-Control", cacheControlValue)
	w.Header().Set("ETag", value.ETag)
	http.ServeContent(w, r, name, modTime, bytes.NewReader(value.Data))
}

func (s *Server) loadGrid(ctx context.Context, store *mbtiles.Store, z, x, y int, wantsGzip bool) (cache.Value, error) {
	data, gridData, err := store.Grid(ctx, z, x, y)
	if err != nil {
		return cache.Value{}, err
	}
	wasGzip := isGzip(data)
	varyAcceptEncoding := wasGzip || gridData != nil
	if gridData != nil {
		if wasGzip {
			data, err = expandGzip(data)
			if err != nil {
				return cache.Value{}, fmt.Errorf("decompress grid: %w", err)
			}
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(data, &document); err != nil {
			return cache.Value{}, fmt.Errorf("decode grid: %w", err)
		}
		encodedData, marshalErr := json.Marshal(gridData)
		if marshalErr != nil {
			return cache.Value{}, fmt.Errorf("encode grid data: %w", marshalErr)
		}
		document["data"] = encodedData
		data, err = json.Marshal(document)
		if err != nil {
			return cache.Value{}, fmt.Errorf("encode grid: %w", err)
		}
		wasGzip = false
	}
	encoding := ""
	if wasGzip {
		if wantsGzip {
			encoding = "gzip"
		} else {
			data, err = expandGzip(data)
			if err != nil {
				return cache.Value{}, fmt.Errorf("decompress grid: %w", err)
			}
		}
	} else if gridData != nil && wantsGzip {
		data, err = compressGzip(data)
		if err != nil {
			return cache.Value{}, fmt.Errorf("compress grid: %w", err)
		}
		encoding = "gzip"
	}
	variant := "identity"
	if encoding != "" {
		variant = encoding
	}
	etag := fmt.Sprintf(`"%x-%x-%08x-%s"`, store.ModTime().UnixNano(), len(data), crc32.ChecksumIEEE(data), variant)
	return cache.Value{
		Data:               data,
		ETag:               etag,
		ContentType:        "application/json; charset=utf-8",
		ContentEncoding:    encoding,
		VaryAcceptEncoding: varyAcceptEncoding,
	}, nil
}

func (s *Server) loadTile(ctx context.Context, store *mbtiles.Store, z, x, y int, wantsGzip bool) (cache.Value, error) {
	data, err := store.Tile(ctx, z, x, y)
	if err != nil {
		return cache.Value{}, err
	}
	meta := store.Metadata()
	storedGzip := isGzip(data)
	encoding := ""
	switch {
	case storedGzip && wantsGzip:
		encoding = "gzip"
	case storedGzip:
		data, err = expandGzip(data)
		if err != nil {
			return cache.Value{}, fmt.Errorf("decompress tile: %w", err)
		}
	case wantsGzip && meta.Extension == "pbf" && s.cfg.Cache.CompressVectorTiles:
		data, err = compressGzip(data)
		if err != nil {
			return cache.Value{}, fmt.Errorf("compress tile: %w", err)
		}
		encoding = "gzip"
	}
	variant := "identity"
	if encoding != "" {
		variant = encoding
	}
	etag := fmt.Sprintf(`"%x-%x-%08x-%s"`, store.ModTime().UnixNano(), len(data), crc32.ChecksumIEEE(data), variant)
	return cache.Value{
		Data:               data,
		ETag:               etag,
		ContentType:        meta.ContentType,
		ContentEncoding:    encoding,
		VaryAcceptEncoding: meta.Extension == "pbf",
	}, nil
}

func extensionAllowed(requested, actual string) bool {
	requested = strings.ToLower(requested)
	actual = strings.ToLower(actual)
	if actual == "pbf" {
		return requested == "pbf" || requested == "mvt" || requested == "vector.pbf"
	}
	if actual == "jpg" {
		return requested == "jpg" || requested == "jpeg"
	}
	return requested == actual
}

func validXYZ(z, x, y int) bool {
	if z < 0 || z > 30 || x < 0 || y < 0 {
		return false
	}
	width := int64(1) << z
	return int64(x) < width && int64(y) < width
}

func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func expandGzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	expanded, readErr := io.ReadAll(io.LimitReader(reader, maxExpandedTileSize+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(expanded) > maxExpandedTileSize {
		return nil, errors.New("expanded tile exceeds safety limit")
	}
	return expanded, nil
}

func compressGzip(data []byte) ([]byte, error) {
	var output bytes.Buffer
	output.Grow(len(data) / 2)
	writer, err := gzip.NewWriterLevel(&output, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func acceptsGzipEncoding(header string) bool {
	best := -1.0
	wildcard := -1.0
	for part := range strings.SplitSeq(header, ",") {
		name, parameters, hasParameters := strings.Cut(part, ";")
		name = strings.TrimSpace(name)
		quality := 1.0
		for hasParameters {
			var parameter string
			parameter, parameters, hasParameters = strings.Cut(parameters, ";")
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && parsed >= 0 && parsed <= 1 {
					quality = parsed
				} else {
					quality = 0
				}
			}
		}
		switch {
		case strings.EqualFold(name, "gzip"):
			best = quality
		case name == "*":
			wildcard = quality
		}
	}
	if best >= 0 {
		return best > 0
	}
	return wildcard > 0
}

func cacheControl(maxAge int, immutable bool) string {
	if maxAge <= 0 {
		return "public, max-age=0, must-revalidate"
	}
	value := "public, max-age=" + strconv.Itoa(maxAge)
	if immutable {
		value += ", immutable"
	}
	return value
}

func latestModTime(stores map[string]*mbtiles.Store) time.Time {
	var latest time.Time
	for _, store := range stores {
		if store.ModTime().After(latest) {
			latest = store.ModTime()
		}
	}
	return latest
}
