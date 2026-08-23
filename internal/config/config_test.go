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
