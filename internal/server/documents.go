package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/ralscha/tileserver-go/internal/mbtiles"
)

func (s *Server) tileJSON(store *mbtiles.Store, baseURL string) map[string]any {
	meta := store.Metadata()
	doc := map[string]any{
		"tilejson": "3.0.0",
		"id":       store.ID(),
		"name":     meta.Name,
		"scheme":   "xyz",
		"format":   meta.Extension,
		"tiles": []string{
			baseURL + "/data/" + store.ID() + "/{z}/{x}/{y}." + meta.Extension,
		},
		"minzoom": meta.MinZoom,
		"maxzoom": meta.MaxZoom,
		"bounds":  []float64{meta.Bounds[0], meta.Bounds[1], meta.Bounds[2], meta.Bounds[3]},
	}
	if meta.HasCenter {
		doc["center"] = []float64{meta.Center[0], meta.Center[1], meta.Center[2]}
	}
	if meta.Description != "" {
		doc["description"] = meta.Description
	}
	if meta.Attribution != "" {
		doc["attribution"] = meta.Attribution
	}
	if meta.Type != "" {
		doc["type"] = meta.Type
	}
	if meta.Version != "" {
		doc["version"] = meta.Version
	}
	if meta.VectorLayers != nil {
		doc["vector_layers"] = meta.VectorLayers
	}
	if meta.TileStats != nil {
		doc["tilestats"] = meta.TileStats
	}
	if meta.HasUTFGrid {
		doc["grids"] = []string{baseURL + "/data/" + store.ID() + "/{z}/{x}/{y}.grid.json"}
	}
	return doc
}

func (s *Server) serveDataDocument(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	if !strings.HasSuffix(name, ".json") {
		s.writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	id := strings.TrimSuffix(name, ".json")
	if s.sources[id] == nil {
		s.writeError(w, http.StatusNotFound, "source not found")
		return
	}
	bundle := s.metadataForRequest(w, r)
	if bundle == nil {
		return
	}
	bundle.tileJSON[id].serve(w, r)
}

func (s *Server) serveDataIndex(w http.ResponseWriter, r *http.Request) {
	bundle := s.metadataForRequest(w, r)
	if bundle == nil {
		return
	}
	bundle.dataIndex.serve(w, r)
}

func (s *Server) serveCatalog(w http.ResponseWriter, r *http.Request) {
	bundle := s.metadataForRequest(w, r)
	if bundle == nil {
		return
	}
	bundle.catalog.serve(w, r)
}

func (s *Server) buildMetadataBundle(baseURL string) (*metadataBundle, error) {
	bundle := &metadataBundle{
		tileJSON:   make(map[string]staticResponse, len(s.sourceIDs)),
		sourceWMTS: make(map[string]staticResponse, len(s.sourceIDs)),
	}
	docs := make([]map[string]any, 0, len(s.sourceIDs))
	sources := make([]map[string]any, 0, len(s.sourceIDs))
	for _, id := range s.sourceIDs {
		store := s.sources[id]
		meta := store.Metadata()
		tileJSON := s.tileJSON(store, baseURL)
		docs = append(docs, tileJSON)
		response, err := newJSONResponse(tileJSON, store.ModTime(), s.metadataCacheControl)
		if err != nil {
			return nil, err
		}
		bundle.tileJSON[id] = response
		bundle.sourceWMTS[id] = s.buildWMTSCapabilities(baseURL, []string{id})
		sources = append(sources, map[string]any{
			"id": id, "name": meta.Name, "format": meta.Extension,
			"tilejson": baseURL + "/data/" + id + ".json",
		})
	}
	catalog := map[string]any{
		"name":    "tileserver-go",
		"sources": sources,
		"links": map[string]string{
			"health": baseURL + "/healthz",
			"wmts":   baseURL + "/wmts?SERVICE=WMTS&REQUEST=GetCapabilities",
		},
	}
	if s.cfg.Observability.Metrics {
		catalog["links"].(map[string]string)["metrics"] = baseURL + "/metrics"
	}
	latest := latestModTime(s.sources)
	var err error
	bundle.catalog, err = newJSONResponse(catalog, latest, s.metadataCacheControl)
	if err != nil {
		return nil, err
	}
	bundle.dataIndex, err = newJSONResponse(docs, latest, s.metadataCacheControl)
	if err != nil {
		return nil, err
	}
	bundle.wmts = s.buildWMTSCapabilities(baseURL, s.sourceIDs)
	return bundle, nil
}

func (s *Server) serveJSON(w http.ResponseWriter, r *http.Request, value any, modTime time.Time, maxAge int) {
	response, err := newJSONResponse(value, modTime, cacheControl(maxAge, false))
	if err != nil {
		s.logger.Error("encode JSON response", "error", err)
		s.writeError(w, http.StatusInternalServerError, "failed to encode response")
		return
	}
	response.serve(w, r)
}

func (s *Server) serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("OK\n"))
}

func (s *Server) serveHealthJSON(w http.ResponseWriter, r *http.Request) {
	s.healthJSON.serve(w, r)
}

func (s *Server) serveReadyJSON(w http.ResponseWriter, r *http.Request) {
	s.readyJSON.serve(w, r)
}

func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	type sourceStatus struct {
		ID              string `json:"id"`
		Format          string `json:"format"`
		OpenConnections int    `json:"open_connections"`
		InUse           int    `json:"in_use"`
		Idle            int    `json:"idle"`
	}
	sources := make([]sourceStatus, 0, len(s.sourceIDs))
	for _, id := range s.sourceIDs {
		store := s.sources[id]
		stats := store.DBStats()
		sources = append(sources, sourceStatus{ID: id, Format: store.Metadata().Extension, OpenConnections: stats.OpenConnections, InUse: stats.InUse, Idle: stats.Idle})
	}
	cacheStats := s.cache.Stats()
	s.serveJSON(w, r, map[string]any{
		"status":   "ok",
		"sources":  sources,
		"cache":    map[string]any{"entries": cacheStats.Entries, "bytes": cacheStats.Bytes, "hits": cacheStats.Hits, "misses": cacheStats.Misses},
		"requests": s.metrics.requests.Load(),
	}, time.Time{}, 0)
}
