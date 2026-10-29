package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ralscha/tileserver-go/internal/config"
)

func TestRunHelpSucceeds(t *testing.T) {
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("run --help: %v", err)
	}
}

func TestHelpAndVersionDoNotLoadConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	for _, option := range []string{"--help", "--version"} {
		if err := run([]string{"--config", missing, option}); err != nil {
			t.Fatalf("run %s with missing config: %v", option, err)
		}
	}
}

func TestParseFlagsConfigPath(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{name: "unset", args: []string{"--listen", ":9090"}},
		{name: "long separate", args: []string{"--config", "custom.json"}, want: "custom.json"},
		{name: "short separate", args: []string{"-config", "custom.json"}, want: "custom.json"},
		{name: "long equals", args: []string{"--config=custom.json"}, want: "custom.json"},
		{name: "short equals", args: []string{"-config=custom.json"}, want: "custom.json"},
		{name: "last occurrence wins", args: []string{"--config", "first.json", "--config=second.json"}, want: "second.json"},
		{name: "argument delimiter", args: []string{"--", "--config=source.mbtiles"}},
		{name: "positional argument stops flags", args: []string{"source.mbtiles", "--config=ignored.json"}},
		{name: "flag value is not a flag", args: []string{"--data", "--config=directory"}},
		{name: "boolean before config", args: []string{"--metrics", "--config=custom.json"}, want: "custom.json"},
		{name: "string before config", args: []string{"--data", "data", "--config=custom.json"}, want: "custom.json"},
		{name: "missing", args: []string{"--config"}, wantErr: "flag needs an argument"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, options, err := parseFlags(test.args, config.Default())
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseFlags() error = %v, want an error containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(): %v", err)
			}
			if options.configPath != test.want {
				t.Fatalf("config path = %q, want %q", options.configPath, test.want)
			}
		})
	}
}

func TestParseFlagsOverridesLoadedConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":9000"
	cfg.Cache.SizeMB = 42
	cfg.HTTP.ReadTimeout = config.Duration(20 * time.Second)
	cfg.CORS.Origins = []string{"https://client.example"}
	cfg.Observability.Metrics = true
	got, _, err := parseFlags([]string{"--listen=:9090", "--metrics=false", "--read-timeout=10s"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Listen != ":9090" || got.Observability.Metrics || got.HTTP.ReadTimeout.Value() != 10*time.Second {
		t.Fatalf("CLI overrides were not applied: %+v", got)
	}
	if got.Cache.SizeMB != 42 || len(got.CORS.Origins) != 1 || got.CORS.Origins[0] != "https://client.example" {
		t.Fatalf("loaded defaults were not preserved: %+v", got)
	}
}
