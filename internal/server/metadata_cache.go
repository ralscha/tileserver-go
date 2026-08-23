package server

import (
	"bytes"
	"container/list"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"sync"
	"time"
)

const metadataBaseURLCacheEntries = 8

type staticResponse struct {
	data         []byte
	contentType  string
	cacheControl string
	etag         string
	name         string
	modTime      time.Time
}

func newStaticResponse(name, contentType string, data []byte, modTime time.Time, cacheControlValue string) staticResponse {
	return staticResponse{
		data:         data,
		contentType:  contentType,
		cacheControl: cacheControlValue,
		etag:         fmt.Sprintf(`"%x-%x-%08x"`, modTime.UnixNano(), len(data), crc32.ChecksumIEEE(data)),
		name:         name,
		modTime:      modTime,
	}
}

func newJSONResponse(value any, modTime time.Time, cacheControlValue string) (staticResponse, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return staticResponse{}, err
	}
	data = append(data, '\n')
	return newStaticResponse("document.json", "application/json; charset=utf-8", data, modTime, cacheControlValue), nil
}

func (response staticResponse) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", response.contentType)
	w.Header().Set("Cache-Control", response.cacheControl)
	w.Header().Set("ETag", response.etag)
	http.ServeContent(w, r, response.name, response.modTime, bytes.NewReader(response.data))
}

type metadataBundle struct {
	catalog    staticResponse
	dataIndex  staticResponse
	tileJSON   map[string]staticResponse
	wmts       staticResponse
	sourceWMTS map[string]staticResponse
}

type metadataCacheEntry struct {
	baseURL string
	bundle  *metadataBundle
}

type metadataResponseCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
}

func (c *metadataResponseCache) get(baseURL string, build func() (*metadataBundle, error)) (*metadataBundle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[baseURL]; element != nil {
		c.lru.MoveToFront(element)
		return element.Value.(*metadataCacheEntry).bundle, nil
	}
	bundle, err := build()
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element, metadataBaseURLCacheEntries)
	}
	entry := &metadataCacheEntry{baseURL: baseURL, bundle: bundle}
	c.entries[baseURL] = c.lru.PushFront(entry)
	if c.lru.Len() > metadataBaseURLCacheEntries {
		oldest := c.lru.Back()
		delete(c.entries, oldest.Value.(*metadataCacheEntry).baseURL)
		c.lru.Remove(oldest)
	}
	return bundle, nil
}

func (s *Server) metadataForBaseURL(baseURL string) (*metadataBundle, error) {
	return s.metadataResponses.get(baseURL, func() (*metadataBundle, error) {
		return s.buildMetadataBundle(baseURL)
	})
}

func (s *Server) metadataForRequest(w http.ResponseWriter, r *http.Request) *metadataBundle {
	bundle, err := s.metadataForBaseURL(s.externalBaseURL(r))
	if err != nil {
		s.logger.Error("encode metadata response", "error", err)
		s.writeError(w, http.StatusInternalServerError, "failed to encode response")
		return nil
	}
	return bundle
}
