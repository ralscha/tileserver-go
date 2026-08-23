// Command releasesmoke verifies the host archive produced by GoReleaser.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const smokeTile = "tileserver-go-release-smoke"

type artifact struct {
	Path   string `json:"path"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Type   string `json:"type"`
}

func main() {
	distDir := flag.String("dist", "dist", "GoReleaser output directory")
	flag.Parse()
	if err := run(*distDir); err != nil {
		fmt.Fprintln(os.Stderr, "release smoke test:", err)
		os.Exit(1)
	}
}

func run(distDir string) error {
	archivePath, err := hostArchive(distDir)
	if err != nil {
		return err
	}
	extractDir, err := os.MkdirTemp("", "tileserver-go-release-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(extractDir) //nolint:errcheck // Best-effort cleanup of a temporary test directory.

	if err := extractArchive(archivePath, extractDir); err != nil {
		return fmt.Errorf("extract %s: %w", filepath.Base(archivePath), err)
	}
	for _, name := range []string{"LICENSE", "README.md", "SECURITY.md", "config.example.json"} {
		if _, err := os.Stat(filepath.Join(extractDir, name)); err != nil {
			return fmt.Errorf("archive is missing %s: %w", name, err)
		}
	}
	binaryName := "tileserver"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(extractDir, binaryName)
	versionOutput, err := exec.Command(binaryPath, "--version").CombinedOutput() //nolint:gosec // The executable comes from the locally built release archive.
	if err != nil {
		return fmt.Errorf("run packaged binary --version: %w: %s", err, versionOutput)
	}
	if !bytes.Contains(versionOutput, []byte("tileserver ")) {
		return fmt.Errorf("unexpected --version output: %s", versionOutput)
	}

	fixturePath := filepath.Join(extractDir, "smoke.mbtiles")
	if err := createFixture(fixturePath); err != nil {
		return err
	}
	if err := exerciseServer(binaryPath, fixturePath); err != nil {
		return err
	}
	fmt.Printf("release smoke test passed: %s (%s/%s)\n", filepath.Base(archivePath), runtime.GOOS, runtime.GOARCH)
	return nil
}

func hostArchive(distDir string) (string, error) {
	absDist, err := filepath.Abs(distDir)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(absDist, "artifacts.json")) //nolint:gosec // The operator-selected release output is intentional.
	if err != nil {
		return "", fmt.Errorf("read GoReleaser artifacts: %w", err)
	}
	var artifacts []artifact
	if err := json.Unmarshal(data, &artifacts); err != nil {
		return "", fmt.Errorf("decode GoReleaser artifacts: %w", err)
	}
	for _, candidate := range artifacts {
		if candidate.Type != "Archive" || candidate.GOOS != runtime.GOOS || candidate.GOARCH != runtime.GOARCH {
			continue
		}
		path := filepath.FromSlash(candidate.Path)
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(absDist), path)
		}
		return path, nil
	}
	return "", fmt.Errorf("no release archive for %s/%s in %s", runtime.GOOS, runtime.GOARCH, absDist)
}

func extractArchive(archivePath, destination string) error {
	switch {
	case strings.HasSuffix(archivePath, ".zip"):
		return extractZIP(archivePath, destination)
	case strings.HasSuffix(archivePath, ".tar.gz"):
		return extractTarGz(archivePath, destination)
	default:
		return fmt.Errorf("unsupported archive format: %s", archivePath)
	}
}

func extractZIP(archivePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close() //nolint:errcheck // No useful recovery is possible for an archive read close error.
	for _, file := range reader.File {
		target, err := safeArchivePath(destination, file.Name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		if err := writeExtractedFile(target, file.Mode(), source); err != nil {
			_ = source.Close()
			return err
		}
		if err := source.Close(); err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(archivePath, destination string) error {
	file, err := os.Open(archivePath) //nolint:gosec // The path comes from GoReleaser's local artifact manifest.
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // No useful recovery is possible for an archive read close error.
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close() //nolint:errcheck // No useful recovery is possible for an archive read close error.
	tape := tar.NewReader(gzipReader)
	for {
		header, err := tape.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeArchivePath(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			if err := writeExtractedFile(target, os.FileMode(header.Mode&0o777), tape); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", header.Name)
		}
	}
}

func safeArchivePath(destination, name string) (string, error) {
	root, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return "", err
	}
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry escapes destination: %q", name)
	}
	return target, nil
}

func writeExtractedFile(path string, mode os.FileMode, source io.Reader) error {
	target, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()) //nolint:gosec // Archive paths are confined by safeArchivePath.
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	return errors.Join(copyErr, closeErr)
}

func createFixture(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("create smoke fixture: %w", err)
	}
	defer db.Close() //nolint:errcheck // The explicit statements surface useful fixture errors.
	for _, statement := range []string{
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`CREATE UNIQUE INDEX tile_index ON tiles (zoom_level, tile_column, tile_row)`,
		`INSERT INTO metadata(name, value) VALUES ('name', 'Release smoke'), ('format', 'png'), ('minzoom', '0'), ('maxzoom', '0')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("create smoke fixture: %w", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO tiles(zoom_level, tile_column, tile_row, tile_data) VALUES (0, 0, 0, ?)`, []byte(smokeTile)); err != nil {
		return fmt.Errorf("insert smoke tile: %w", err)
	}
	return db.Close()
}

func exerciseServer(binaryPath, fixturePath string) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return err
	}

	var logs bytes.Buffer
	command := exec.Command(binaryPath, //nolint:gosec // The executable comes from the locally built release archive.
		"--listen="+address,
		"--allowed-hosts=127.0.0.1",
		"--access-log=false",
		"--metrics=false",
		fixturePath,
	)
	command.Stdout = &logs
	command.Stderr = &logs
	if err := command.Start(); err != nil {
		return fmt.Errorf("start packaged server: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	stop := func(force bool) error {
		if force || runtime.GOOS == "windows" {
			return command.Process.Kill()
		}
		return command.Process.Signal(os.Interrupt)
	}

	baseURL := "http://" + address
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, requestErr := client.Get(baseURL + "/healthz")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case processErr := <-wait:
			return fmt.Errorf("packaged server exited during startup: %w\n%s", processErr, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			_ = stop(true)
			<-wait
			return fmt.Errorf("packaged server did not become healthy\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, check := range []struct {
		path        string
		contentType string
		body        string
	}{
		{path: "/readyz", contentType: "application/json", body: `"status":"ready"`},
		{path: "/data/smoke.json", contentType: "application/json", body: `"tilejson":"3.0.0"`},
		{path: "/data/smoke/0/0/0.png", contentType: "image/png", body: smokeTile},
	} {
		response, err := client.Get(baseURL + check.path)
		if err != nil {
			_ = stop(true)
			<-wait
			return fmt.Errorf("GET %s: %w\n%s", check.path, err, logs.String())
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), check.contentType) || !bytes.Contains(body, []byte(check.body)) {
			_ = stop(true)
			<-wait
			if responseErr := errors.Join(readErr, closeErr); responseErr != nil {
				return fmt.Errorf("GET %s failed: status=%d content-type=%q body=%q: %w\n%s", check.path, response.StatusCode, response.Header.Get("Content-Type"), body, responseErr, logs.String())
			}
			return fmt.Errorf("GET %s failed: status=%d content-type=%q body=%q\n%s", check.path, response.StatusCode, response.Header.Get("Content-Type"), body, logs.String())
		}
	}

	if err := stop(false); err != nil {
		_ = stop(true)
		<-wait
		return fmt.Errorf("stop packaged server: %w\n%s", err, logs.String())
	}
	select {
	case err := <-wait:
		if runtime.GOOS != "windows" && err != nil {
			return fmt.Errorf("packaged server did not shut down cleanly: %w\n%s", err, logs.String())
		}
	case <-time.After(15 * time.Second):
		_ = stop(true)
		<-wait
		return fmt.Errorf("packaged server did not stop in time\n%s", logs.String())
	}
	return nil
}
