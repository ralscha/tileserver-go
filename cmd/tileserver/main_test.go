package main

import (
	"strings"
	"testing"
)

func TestRunHelpSucceeds(t *testing.T) {
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("run --help: %v", err)
	}
}

func TestFindConfigPath(t *testing.T) {
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
		{name: "missing", args: []string{"--config"}, wantErr: "flag needs an argument"},
		{name: "next option", args: []string{"--config", "--listen", ":9090"}, wantErr: "flag needs an argument"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := findConfigPath(test.args)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("findConfigPath() error = %v, want an error containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("findConfigPath(): %v", err)
			}
			if got != test.want {
				t.Fatalf("findConfigPath() = %q, want %q", got, test.want)
			}
		})
	}
}
