package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":8080","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an unknown configuration field to fail")
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	for name, contents := range map[string]string{
		"second document": `{} {}`,
		"invalid suffix":  `{} trailing`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected trailing configuration data to fail")
			}
		})
	}
}

func TestLoadRejectsNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(" \nnull\n "), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected null configuration to fail")
	}
}

func TestLoadPreservesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":9090","cache":{"size_mb":12}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9090" || cfg.Cache.SizeMB != 12 || cfg.Cache.TileMaxAge != Default().Cache.TileMaxAge {
		t.Fatalf("loaded config lost defaults or overrides: %+v", cfg)
	}
}

func TestNormalizeRejectsInvalidPublicURL(t *testing.T) {
	for _, publicURL := range []string{"https://:443", "https://maps.example?", "https://maps.example#", "https://maps.example?key=value", "https://maps.example#fragment"} {
		cfg := Default()
		cfg.PublicURL = publicURL
		if err := cfg.Normalize(t.TempDir()); err == nil {
			t.Errorf("Normalize() accepted public URL %q", publicURL)
		}
	}
}

func TestNormalizePublicURLAndPaths(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	cfg.BasePath = "/maps/"
	cfg.PublicURL = "https://maps.example.test/edge/"
	cfg.Sources["named"] = Source{Path: "named.mbtiles"}
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://maps.example.test/edge/maps" {
		t.Fatalf("unexpected public URL: %q", cfg.PublicURL)
	}
	if cfg.BasePath != "/maps" {
		t.Fatalf("unexpected base path: %q", cfg.BasePath)
	}
	if cfg.DataDir != filepath.Join(root, "data") {
		t.Fatalf("unexpected data directory: %q", cfg.DataDir)
	}
	if cfg.Sources["named"].Path != filepath.Join(root, "data", "named.mbtiles") {
		t.Fatalf("unexpected source path: %q", cfg.Sources["named"].Path)
	}
}

func TestNormalizeRejectsUnsafeBasePaths(t *testing.T) {
	for _, basePath := range []string{"/maps//v1", "/maps/../v1", "/./maps"} {
		cfg := Default()
		cfg.BasePath = basePath
		if err := cfg.Normalize(t.TempDir()); err == nil {
			t.Errorf("Normalize() accepted base path %q", basePath)
		}
	}
}

func TestValidateID(t *testing.T) {
	for _, valid := range []string{"openmaptiles", "zurich-2026", "raster.v1", "a_b"} {
		if err := ValidateID(valid); err != nil {
			t.Errorf("ValidateID(%q): %v", valid, err)
		}
	}
	for _, invalid := range []string{"", ".", "..", "../map", "has space", "map/tiles"} {
		if err := ValidateID(invalid); err == nil {
			t.Errorf("ValidateID(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestValidateNormalizesPolicyLists(t *testing.T) {
	cfg := Default()
	cfg.AllowedHosts = []string{" maps.example.test ", "maps.example.test"}
	cfg.CORS.Origins = []string{" https://client.example ", "https://client.example"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedHosts) != 1 || cfg.AllowedHosts[0] != "maps.example.test" {
		t.Fatalf("unexpected allowed hosts: %#v", cfg.AllowedHosts)
	}
	if len(cfg.CORS.Origins) != 1 || cfg.CORS.Origins[0] != "https://client.example" {
		t.Fatalf("unexpected CORS origins: %#v", cfg.CORS.Origins)
	}
}

func TestValidateRejectsInvalidPolicies(t *testing.T) {
	for name, configure := range map[string]func(*Config){
		"blank allowed hosts":        func(cfg *Config) { cfg.AllowedHosts = []string{" "} },
		"malformed wildcard":         func(cfg *Config) { cfg.AllowedHosts = []string{"maps.*.example"} },
		"wildcard with port":         func(cfg *Config) { cfg.AllowedHosts = []string{"*.example.test:8443"} },
		"origin with path":           func(cfg *Config) { cfg.CORS.Origins = []string{"https://client.example/path"} },
		"origin without host":        func(cfg *Config) { cfg.CORS.Origins = []string{"https://:443"} },
		"origin with empty query":    func(cfg *Config) { cfg.CORS.Origins = []string{"https://client.example?"} },
		"origin with empty fragment": func(cfg *Config) { cfg.CORS.Origins = []string{"https://client.example#"} },
		"wildcard credentials": func(cfg *Config) {
			cfg.CORS.Origins = []string{" * "}
			cfg.CORS.AllowCredentials = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			configure(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected policy validation to fail")
			}
		})
	}
}
