package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Config contains all server settings. Durations use Go duration strings in JSON.
type Config struct {
	Listen        string            `json:"listen"`
	PublicURL     string            `json:"public_url,omitempty"`
	BasePath      string            `json:"base_path,omitempty"`
	DataDir       string            `json:"data_dir"`
	AllowedHosts  []string          `json:"allowed_hosts"`
	TrustProxy    bool              `json:"trust_proxy"`
	CORS          CORS              `json:"cors"`
	Cache         Cache             `json:"cache"`
	Database      Database          `json:"database"`
	HTTP          HTTP              `json:"http"`
	Observability Observability     `json:"observability"`
	Sources       map[string]Source `json:"sources,omitempty"`
}

type CORS struct {
	Origins          []string `json:"origins"`
	AllowCredentials bool     `json:"allow_credentials"`
}

type Cache struct {
	SizeMB              int  `json:"size_mb"`
	TileMaxAge          int  `json:"tile_max_age_seconds"`
	MetadataMaxAge      int  `json:"metadata_max_age_seconds"`
	CompressVectorTiles bool `json:"compress_vector_tiles"`
}

type Database struct {
	MaxConnections int `json:"max_connections"`
	MmapSizeMB     int `json:"mmap_size_mb"`
	PageCacheMB    int `json:"page_cache_mb"`
}

type HTTP struct {
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
	MaxHeaderBytes    int      `json:"max_header_bytes"`
	MaxConcurrent     int      `json:"max_concurrent_requests"`
}

type Observability struct {
	Metrics   bool `json:"metrics"`
	AccessLog bool `json:"access_log"`
}

type Source struct {
	Path string `json:"path"`
}

type Duration time.Duration

func (d Duration) Value() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("duration must be a string such as \"15s\"")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func Default() Config {
	connections := min(max(runtime.GOMAXPROCS(0), 4), 32)
	return Config{
		Listen:       ":8080",
		DataDir:      "data",
		AllowedHosts: []string{"*"},
		CORS:         CORS{Origins: []string{"*"}},
		Cache: Cache{
			SizeMB:              256,
			TileMaxAge:          86400,
			MetadataMaxAge:      300,
			CompressVectorTiles: true,
		},
		Database: Database{
			MaxConnections: connections,
			MmapSizeMB:     256,
			PageCacheMB:    64,
		},
		HTTP: HTTP{
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadTimeout:       Duration(15 * time.Second),
			WriteTimeout:      Duration(30 * time.Second),
			IdleTimeout:       Duration(2 * time.Minute),
			ShutdownTimeout:   Duration(15 * time.Second),
			MaxHeaderBytes:    1 << 20,
			MaxConcurrent:     4096,
		},
		Observability: Observability{Metrics: true, AccessLog: true},
		Sources:       make(map[string]Source),
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	// The path is an operator-supplied CLI/configuration value, not request data.
	data, err := os.ReadFile(path) //nolint:gosec // Reading the explicitly selected config file is intended.
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Sources == nil {
		cfg.Sources = make(map[string]Source)
	}
	return cfg, nil
}

func (c *Config) Normalize(root string) error {
	if root == "" {
		root = "."
	}
	var err error
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(root, c.DataDir)
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.BasePath = normalizeBasePath(c.BasePath)
	if c.PublicURL != "" {
		u, parseErr := url.Parse(c.PublicURL)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("public_url must be an absolute http(s) URL without credentials, query, or fragment")
		}
		u.Path = strings.TrimRight(u.Path, "/") + c.BasePath
		c.PublicURL = strings.TrimRight(u.String(), "/")
	}
	for id, source := range c.Sources {
		if err := ValidateID(id); err != nil {
			return fmt.Errorf("source %q: %w", id, err)
		}
		if source.Path == "" {
			return fmt.Errorf("source %q has an empty path", id)
		}
		if !filepath.IsAbs(source.Path) {
			source.Path = filepath.Join(c.DataDir, source.Path)
		}
		c.Sources[id] = source
	}
	return c.Validate()
}

func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address cannot be empty")
	}
	if c.Cache.SizeMB < 0 || c.Cache.TileMaxAge < 0 || c.Cache.MetadataMaxAge < 0 {
		return errors.New("cache values cannot be negative")
	}
	if c.Database.MaxConnections < 1 || c.Database.MmapSizeMB < 0 || c.Database.PageCacheMB < 0 {
		return errors.New("invalid database settings")
	}
	if c.HTTP.MaxConcurrent < 1 || c.HTTP.MaxHeaderBytes < 1024 {
		return errors.New("invalid HTTP limits")
	}
	if c.HTTP.ReadHeaderTimeout.Value() <= 0 || c.HTTP.WriteTimeout.Value() <= 0 || c.HTTP.IdleTimeout.Value() <= 0 || c.HTTP.ShutdownTimeout.Value() <= 0 || c.HTTP.ReadTimeout.Value() < 0 {
		return errors.New("invalid HTTP timeouts")
	}
	if len(c.AllowedHosts) == 0 {
		return errors.New("allowed_hosts must contain at least one host or wildcard")
	}
	if len(c.CORS.Origins) == 0 {
		c.CORS.Origins = []string{"*"}
	}
	if c.CORS.AllowCredentials && slices.Contains(c.CORS.Origins, "*") {
		return errors.New("cors.allow_credentials cannot be combined with wildcard origin")
	}
	return nil
}

func normalizeBasePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return ""
	}
	return "/" + strings.Trim(value, "/")
}

func ValidateID(id string) error {
	if id == "" || len(id) > 128 {
		return errors.New("id must contain between 1 and 128 characters")
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return errors.New("id may contain only letters, digits, dots, dashes, and underscores")
	}
	return nil
}
