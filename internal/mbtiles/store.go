package mbtiles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrTileNotFound = errors.New("tile not found")

type Options struct {
	MaxConnections int
	MmapSizeMB     int
	// PageCacheMB is the total SQLite page-cache budget for this source. It is
	// divided across the source's connection pool.
	PageCacheMB int
}

type Store struct {
	id           string
	path         string
	db           *sql.DB
	tileStmt     *sql.Stmt
	gridStmt     *sql.Stmt
	gridDataStmt *sql.Stmt
	meta         Metadata
	modTime      time.Time
	fileSize     int64
}

type Metadata struct {
	Name         string
	Description  string
	Attribution  string
	Type         string
	Version      string
	Format       string
	ContentType  string
	Extension    string
	MinZoom      int
	MaxZoom      int
	Bounds       [4]float64
	Center       [3]float64
	HasBounds    bool
	HasCenter    bool
	VectorLayers any
	TileStats    any
	Raw          map[string]string
	HasUTFGrid   bool
}

func Open(ctx context.Context, id, path string, options Options) (*Store, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat MBTiles: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("MBTiles path is not a regular file")
	}
	if options.MaxConnections < 1 {
		options.MaxConnections = 1
	}

	dsn := sqliteDSN(absPath, options)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(options.MaxConnections)
	db.SetMaxIdleConns(options.MaxConnections)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping SQLite: %w", err)
	}

	store := &Store{id: id, path: absPath, db: db, modTime: info.ModTime().UTC(), fileSize: info.Size()}
	if err := store.loadMetadata(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	store.tileStmt, err = db.PrepareContext(ctx, `SELECT tile_data FROM tiles WHERE zoom_level = ? AND tile_column = ? AND tile_row = ? LIMIT 1`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare tile query (is this a valid MBTiles file?): %w", err)
	}
	store.meta.HasUTFGrid, err = store.hasObject(ctx, "grids")
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if store.meta.HasUTFGrid {
		store.gridStmt, err = db.PrepareContext(ctx, `SELECT grid FROM grids WHERE zoom_level = ? AND tile_column = ? AND tile_row = ? LIMIT 1`)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("prepare UTFGrid query: %w", err)
		}
		hasGridData, schemaErr := store.hasObject(ctx, "grid_data")
		if schemaErr != nil {
			_ = store.Close()
			return nil, schemaErr
		}
		if hasGridData {
			store.gridDataStmt, err = db.PrepareContext(ctx, `SELECT key_name, key_json FROM grid_data WHERE zoom_level = ? AND tile_column = ? AND tile_row = ?`)
			if err != nil {
				_ = store.Close()
				return nil, fmt.Errorf("prepare UTFGrid data query: %w", err)
			}
		}
	}
	return store, nil
}

func sqliteDSN(path string, options Options) string {
	// SQLite expects a drive-letter URI such as file:C:/maps/world.mbtiles on Windows;
	// url.URL.Path would incorrectly produce file:///C:/... for this driver.
	u := &url.URL{Scheme: "file", Opaque: filepath.ToSlash(path)}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "temp_store(MEMORY)")
	if options.MmapSizeMB > 0 {
		query.Add("_pragma", fmt.Sprintf("mmap_size(%d)", int64(options.MmapSizeMB)<<20))
	}
	if options.PageCacheMB > 0 {
		// cache_size is connection-local. Divide the source budget across every
		// possible pooled connection so increasing MaxConnections does not also
		// multiply the configured memory budget. A negative value is kibibytes.
		query.Add("_pragma", fmt.Sprintf("cache_size(-%d)", pageCacheKiBPerConnection(options.PageCacheMB, options.MaxConnections)))
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func pageCacheKiBPerConnection(totalMB, maxConnections int) int64 {
	if totalMB <= 0 {
		return 0
	}
	if maxConnections < 1 {
		maxConnections = 1
	}
	totalKiB := int64(totalMB) * 1024
	// SQLite needs at least one cache page. One KiB is the smallest useful
	// advisory when a deliberately tiny budget is spread over a large pool.
	return max(1, totalKiB/int64(maxConnections))
}

func (s *Store) ID() string           { return s.id }
func (s *Store) Path() string         { return s.path }
func (s *Store) Metadata() Metadata   { return s.meta }
func (s *Store) ModTime() time.Time   { return s.modTime }
func (s *Store) FileSize() int64      { return s.fileSize }
func (s *Store) DBStats() sql.DBStats { return s.db.Stats() }

func (s *Store) Close() error {
	if s.gridDataStmt != nil {
		_ = s.gridDataStmt.Close()
	}
	if s.gridStmt != nil {
		_ = s.gridStmt.Close()
	}
	if s.tileStmt != nil {
		_ = s.tileStmt.Close()
	}
	return s.db.Close()
}

func (s *Store) Tile(ctx context.Context, z, x, y int) ([]byte, error) {
	row, ok := tmsRow(z, y)
	if !ok || x < 0 || int64(x) >= int64(1)<<z {
		return nil, ErrTileNotFound
	}
	var data []byte
	if err := s.tileStmt.QueryRowContext(ctx, z, x, row).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTileNotFound
		}
		return nil, fmt.Errorf("read tile: %w", err)
	}
	return data, nil
}

func (s *Store) Grid(ctx context.Context, z, x, y int) ([]byte, map[string]json.RawMessage, error) {
	if s.gridStmt == nil {
		return nil, nil, ErrTileNotFound
	}
	row, ok := tmsRow(z, y)
	if !ok || x < 0 || int64(x) >= int64(1)<<z {
		return nil, nil, ErrTileNotFound
	}
	var data []byte
	if err := s.gridStmt.QueryRowContext(ctx, z, x, row).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrTileNotFound
		}
		return nil, nil, fmt.Errorf("read grid: %w", err)
	}
	if s.gridDataStmt == nil {
		return data, nil, nil
	}
	rows, err := s.gridDataStmt.QueryContext(ctx, z, x, row)
	if err != nil {
		return nil, nil, fmt.Errorf("read grid data: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values := make(map[string]json.RawMessage)
	for rows.Next() {
		var key, encoded string
		if err := rows.Scan(&key, &encoded); err != nil {
			return nil, nil, fmt.Errorf("scan grid data: %w", err)
		}
		value := json.RawMessage(encoded)
		if !json.Valid(value) {
			return nil, nil, fmt.Errorf("grid data for key %q is invalid JSON", key)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate grid data: %w", err)
	}
	return data, values, nil
}

func tmsRow(z, y int) (int64, bool) {
	if z < 0 || z > 30 || y < 0 {
		return 0, false
	}
	width := int64(1) << z
	if int64(y) >= width {
		return 0, false
	}
	return width - 1 - int64(y), true
}

func (s *Store) loadMetadata(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name, value FROM metadata`)
	if err != nil {
		return fmt.Errorf("read MBTiles metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	raw := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return fmt.Errorf("scan MBTiles metadata: %w", err)
		}
		raw[strings.ToLower(strings.TrimSpace(name))] = value
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate MBTiles metadata: %w", err)
	}
	format := strings.ToLower(strings.TrimSpace(raw["format"]))
	if format == "" {
		return errors.New("invalid MBTiles: required format metadata is missing")
	}
	contentType, extension := formatInfo(format)
	meta := Metadata{
		Name:        raw["name"],
		Description: raw["description"],
		Attribution: raw["attribution"],
		Type:        raw["type"],
		Version:     raw["version"],
		Format:      format,
		ContentType: contentType,
		Extension:   extension,
		Raw:         raw,
	}
	if meta.Name == "" {
		meta.Name = s.id
	}
	minZoom, minOK := parseZoom(raw["minzoom"])
	maxZoom, maxOK := parseZoom(raw["maxzoom"])
	if !minOK || !maxOK {
		var foundMin, foundMax sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT min(zoom_level), max(zoom_level) FROM tiles`).Scan(&foundMin, &foundMax); err != nil {
			return fmt.Errorf("derive zoom range: %w", err)
		}
		if !minOK && foundMin.Valid {
			minZoom = int(foundMin.Int64)
		}
		if !maxOK && foundMax.Valid {
			maxZoom = int(foundMax.Int64)
		}
	}
	if minZoom < 0 || maxZoom < minZoom || maxZoom > 30 {
		return fmt.Errorf("invalid zoom range %d..%d", minZoom, maxZoom)
	}
	meta.MinZoom, meta.MaxZoom = minZoom, maxZoom
	meta.Bounds, meta.HasBounds = parseNumbers4(raw["bounds"])
	if !meta.HasBounds {
		meta.Bounds = [4]float64{-180, -85.0511287798066, 180, 85.0511287798066}
	}
	meta.Center, meta.HasCenter = parseNumbers3(raw["center"])
	if meta.HasCenter && (meta.Center[0] < meta.Bounds[0] || meta.Center[0] > meta.Bounds[2] || meta.Center[1] < meta.Bounds[1] || meta.Center[1] > meta.Bounds[3] || meta.Center[2] < float64(meta.MinZoom) || meta.Center[2] > float64(meta.MaxZoom)) {
		meta.HasCenter = false
	}
	if metadataJSON := raw["json"]; metadataJSON != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(metadataJSON), &extra); err != nil {
			return fmt.Errorf("decode MBTiles json metadata: %w", err)
		}
		meta.VectorLayers = extra["vector_layers"]
		meta.TileStats = extra["tilestats"]
	}
	if extension == "pbf" && meta.VectorLayers == nil {
		return errors.New("invalid vector MBTiles: json metadata with vector_layers is required")
	}
	s.meta = meta
	return nil
}

func (s *Store) hasObject(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE name = ? AND type IN ('table', 'view') LIMIT 1`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect MBTiles schema: %w", err)
	}
	return true, nil
}

func parseZoom(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	return n, err == nil
}

func parseNumbers4(value string) ([4]float64, bool) {
	var result [4]float64
	parts := strings.Split(value, ",")
	if len(parts) != len(result) {
		return result, false
	}
	for i := range result {
		n, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
		if err != nil {
			return [4]float64{}, false
		}
		result[i] = n
	}
	if result[0] < -180 || result[0] > 180 || result[2] < -180 || result[2] > 180 || result[1] < -90 || result[1] > 90 || result[3] < -90 || result[3] > 90 || result[0] > result[2] || result[1] > result[3] {
		return [4]float64{}, false
	}
	return result, true
}

func parseNumbers3(value string) ([3]float64, bool) {
	var result [3]float64
	parts := strings.Split(value, ",")
	if len(parts) != len(result) {
		return result, false
	}
	for i := range result {
		n, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
		if err != nil {
			return [3]float64{}, false
		}
		result[i] = n
	}
	return result, true
}

func formatInfo(format string) (contentType, extension string) {
	switch format {
	case "pbf", "mvt", "application/vnd.mapbox-vector-tile", "application/x-protobuf":
		return "application/vnd.mapbox-vector-tile", "pbf"
	case "png", "image/png":
		return "image/png", "png"
	case "jpg", "jpeg", "image/jpeg":
		return "image/jpeg", "jpg"
	case "webp", "image/webp":
		return "image/webp", "webp"
	case "avif", "image/avif":
		return "image/avif", "avif"
	default:
		if strings.Contains(format, "/") {
			return format, extensionForMIME(format)
		}
		return "application/octet-stream", format
	}
}

func extensionForMIME(value string) string {
	value = strings.TrimPrefix(value, "application/")
	value = strings.TrimPrefix(value, "image/")
	value = strings.TrimPrefix(value, "vnd.")
	value = strings.ReplaceAll(value, "+", ".")
	if value == "octet-stream" || strings.ContainsAny(value, `/\\`) {
		return "bin"
	}
	return value
}
