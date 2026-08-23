package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ralscha/tileserver-go/internal/cache"
	"github.com/ralscha/tileserver-go/internal/config"
	"github.com/ralscha/tileserver-go/internal/flight"
	"github.com/ralscha/tileserver-go/internal/mbtiles"
)

type Server struct {
	cfg                  config.Config
	logger               *slog.Logger
	sources              map[string]*mbtiles.Store
	sourceIDs            []string
	cache                *cache.Cache
	flight               flight.Group[cache.Key, cache.Value]
	metrics              metrics
	limit                chan struct{}
	closeOnce            sync.Once
	hosts                hostPolicy
	cors                 corsPolicy
	tileCacheControl     string
	metadataCacheControl string
	healthJSON           staticResponse
	readyJSON            staticResponse
	metadataResponses    metadataResponseCache
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	paths, err := discoverSources(cfg)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no MBTiles sources found in %q", cfg.DataDir)
	}
	s := &Server{
		cfg:                  cfg,
		logger:               logger,
		sources:              make(map[string]*mbtiles.Store, len(paths)),
		cache:                cache.New(int64(cfg.Cache.SizeMB) << 20),
		limit:                make(chan struct{}, cfg.HTTP.MaxConcurrent),
		hosts:                newHostPolicy(cfg.AllowedHosts),
		cors:                 newCORSPolicy(cfg.CORS.Origins),
		tileCacheControl:     cacheControl(cfg.Cache.TileMaxAge, true),
		metadataCacheControl: cacheControl(cfg.Cache.MetadataMaxAge, false),
	}
	ids := make([]string, 0, len(paths))
	for id := range paths {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		store, openErr := mbtiles.Open(ctx, id, paths[id], mbtiles.Options{
			MaxConnections: cfg.Database.MaxConnections,
			MmapSizeMB:     cfg.Database.MmapSizeMB,
			PageCacheMB:    cfg.Database.PageCacheMB,
		})
		if openErr != nil {
			_ = s.Close()
			return nil, fmt.Errorf("open source %q: %w", id, openErr)
		}
		s.sources[id] = store
		s.sourceIDs = append(s.sourceIDs, id)
	}
	s.healthJSON, err = newJSONResponse(map[string]any{"status": "ok"}, time.Time{}, "no-store")
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("encode health response: %w", err)
	}
	s.readyJSON, err = newJSONResponse(map[string]any{"status": "ready", "sources": len(s.sources)}, time.Time{}, "no-store")
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("encode readiness response: %w", err)
	}
	if cfg.PublicURL != "" {
		if _, err := s.metadataForBaseURL(cfg.PublicURL); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("encode metadata responses: %w", err)
		}
	}
	return s, nil
}

func discoverSources(cfg config.Config) (map[string]string, error) {
	paths := make(map[string]string, len(cfg.Sources))
	for id, entry := range cfg.Sources {
		paths[id] = entry.Path
	}
	entries, err := os.ReadDir(cfg.DataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("scan data directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".mbtiles") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if _, exists := paths[id]; exists {
			continue
		}
		if config.ValidateID(id) != nil {
			continue
		}
		paths[id] = filepath.Join(cfg.DataDir, entry.Name())
	}
	return paths, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.serveCatalog)
	mux.HandleFunc("GET /health", s.serveHealth)
	mux.HandleFunc("GET /healthz", s.serveHealthJSON)
	mux.HandleFunc("GET /readyz", s.serveReadyJSON)
	if s.cfg.Observability.Metrics {
		mux.HandleFunc("GET /metrics", s.serveMetrics)
	}
	mux.HandleFunc("GET /status.json", s.serveStatus)
	mux.HandleFunc("GET /data.json", s.serveDataIndex)
	mux.HandleFunc("GET /index.json", s.serveCatalog)
	mux.HandleFunc("GET /data/{id}", s.serveDataDocument)
	mux.HandleFunc("GET /data/{id}/wmts.xml", s.serveSourceWMTS)
	mux.HandleFunc("GET /data/{id}/{z}/{x}/{tile}", s.serveTilePath)
	mux.HandleFunc("GET /tiles/{id}/{z}/{x}/{tile}", s.serveTilePath)
	mux.HandleFunc("GET /wmts", s.serveWMTS)
	mux.HandleFunc("GET /{path...}", func(w http.ResponseWriter, _ *http.Request) {
		s.writeError(w, http.StatusNotFound, "endpoint not found")
	})

	var handler http.Handler = mux
	handler = s.preflightMiddleware(handler)
	if s.cfg.BasePath != "" {
		handler = s.basePathMiddleware(handler)
	}
	handler = s.recoverMiddleware(handler)
	handler = s.concurrencyMiddleware(handler)
	handler = s.hostMiddleware(handler)
	if s.cfg.Observability.Metrics || s.cfg.Observability.AccessLog {
		handler = s.observabilityMiddleware(handler)
	}
	handler = s.securityMiddleware(handler)
	return handler
}

func (s *Server) Close() error {
	var result error
	s.closeOnce.Do(func() {
		for _, store := range s.sources {
			if err := store.Close(); err != nil {
				result = errors.Join(result, err)
			}
		}
	})
	return result
}
