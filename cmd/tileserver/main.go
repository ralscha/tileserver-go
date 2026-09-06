package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/ralscha/tileserver-go/internal/config"
	"github.com/ralscha/tileserver-go/internal/server"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tileserver:", err)
		os.Exit(1)
	}
}

func run(args []string) (resultErr error) {
	configPath, err := findConfigPath(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	allowedHosts := strings.Join(cfg.AllowedHosts, ",")
	corsOrigins := strings.Join(cfg.CORS.Origins, ",")
	readHeaderTimeout := cfg.HTTP.ReadHeaderTimeout.Value()
	readTimeout := cfg.HTTP.ReadTimeout.Value()
	writeTimeout := cfg.HTTP.WriteTimeout.Value()
	idleTimeout := cfg.HTTP.IdleTimeout.Value()
	shutdownTimeout := cfg.HTTP.ShutdownTimeout.Value()
	showVersion := false
	logFormat := "text"

	flags := flag.NewFlagSet("tileserver", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&configPath, "config", configPath, "JSON configuration file")
	flags.StringVar(&cfg.Listen, "listen", cfg.Listen, "listen address")
	flags.StringVar(&cfg.PublicURL, "public-url", cfg.PublicURL, "canonical public URL")
	flags.StringVar(&cfg.BasePath, "base-path", cfg.BasePath, "URL path prefix")
	flags.StringVar(&cfg.DataDir, "data", cfg.DataDir, "MBTiles directory")
	flags.StringVar(&allowedHosts, "allowed-hosts", allowedHosts, "comma-separated allowed Host names")
	flags.BoolVar(&cfg.TrustProxy, "trust-proxy", cfg.TrustProxy, "trust X-Forwarded-Host and X-Forwarded-Proto")
	flags.StringVar(&corsOrigins, "cors-origins", corsOrigins, "comma-separated allowed CORS origins")
	flags.BoolVar(&cfg.CORS.AllowCredentials, "cors-credentials", cfg.CORS.AllowCredentials, "allow credentials for configured CORS origins")
	flags.IntVar(&cfg.Cache.SizeMB, "cache-size", cfg.Cache.SizeMB, "in-memory tile cache size in MiB")
	flags.IntVar(&cfg.Cache.TileMaxAge, "tile-max-age", cfg.Cache.TileMaxAge, "tile browser/CDN cache lifetime in seconds")
	flags.IntVar(&cfg.Cache.MetadataMaxAge, "metadata-max-age", cfg.Cache.MetadataMaxAge, "metadata browser/CDN cache lifetime in seconds")
	flags.BoolVar(&cfg.Cache.CompressVectorTiles, "compress-vector-tiles", cfg.Cache.CompressVectorTiles, "gzip uncompressed vector tiles when accepted")
	flags.IntVar(&cfg.Database.MaxConnections, "db-connections", cfg.Database.MaxConnections, "SQLite connections per source")
	flags.IntVar(&cfg.Database.MmapSizeMB, "mmap-size", cfg.Database.MmapSizeMB, "SQLite mmap size per source in MiB")
	flags.IntVar(&cfg.Database.PageCacheMB, "page-cache-size", cfg.Database.PageCacheMB, "SQLite page-cache budget per source in MiB")
	flags.IntVar(&cfg.HTTP.MaxConcurrent, "max-concurrent", cfg.HTTP.MaxConcurrent, "maximum concurrent HTTP requests")
	flags.IntVar(&cfg.HTTP.MaxHeaderBytes, "max-header-bytes", cfg.HTTP.MaxHeaderBytes, "maximum HTTP request-header size in bytes")
	flags.DurationVar(&readHeaderTimeout, "read-header-timeout", readHeaderTimeout, "HTTP read-header timeout")
	flags.DurationVar(&readTimeout, "read-timeout", readTimeout, "HTTP read timeout")
	flags.DurationVar(&writeTimeout, "write-timeout", writeTimeout, "HTTP write timeout")
	flags.DurationVar(&idleTimeout, "idle-timeout", idleTimeout, "HTTP keep-alive idle timeout")
	flags.DurationVar(&shutdownTimeout, "shutdown-timeout", shutdownTimeout, "graceful shutdown timeout")
	flags.BoolVar(&cfg.Observability.Metrics, "metrics", cfg.Observability.Metrics, "serve Prometheus metrics")
	flags.BoolVar(&cfg.Observability.AccessLog, "access-log", cfg.Observability.AccessLog, "log HTTP requests")
	flags.StringVar(&logFormat, "log-format", logFormat, "log format: text or json")
	flags.BoolVar(&showVersion, "version", false, "print version and exit")
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage: tileserver [options] [source.mbtiles ...]\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if showVersion {
		fmt.Printf("tileserver %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
		return nil
	}
	if logFormat != "text" && logFormat != "json" {
		return errors.New("log-format must be text or json")
	}
	cfg.AllowedHosts = splitList(allowedHosts)
	cfg.CORS.Origins = splitList(corsOrigins)
	cfg.HTTP.ReadHeaderTimeout = config.Duration(readHeaderTimeout)
	cfg.HTTP.ReadTimeout = config.Duration(readTimeout)
	cfg.HTTP.WriteTimeout = config.Duration(writeTimeout)
	cfg.HTTP.IdleTimeout = config.Duration(idleTimeout)
	cfg.HTTP.ShutdownTimeout = config.Duration(shutdownTimeout)

	root := "."
	if configPath != "" {
		absoluteConfig, absErr := filepath.Abs(configPath)
		if absErr != nil {
			return absErr
		}
		root = filepath.Dir(absoluteConfig)
	}
	if err := cfg.Normalize(root); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	for _, sourcePath := range flags.Args() {
		absolute, absErr := filepath.Abs(sourcePath)
		if absErr != nil {
			return absErr
		}
		id := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
		if err := config.ValidateID(id); err != nil {
			return fmt.Errorf("source filename %q does not produce a valid id: %w", sourcePath, err)
		}
		cfg.Sources[id] = config.Source{Path: absolute}
	}

	var logHandler slog.Handler
	if logFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		logHandler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	logger := slog.New(logHandler).With("service", "tileserver-go", "version", version)
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lifetimeContext, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()

	tileServer, err := server.New(lifetimeContext, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, tileServer.Close())
	}()
	httpServer := &http.Server{
		Handler:           tileServer.Handler(),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout.Value(),
		ReadTimeout:       cfg.HTTP.ReadTimeout.Value(),
		WriteTimeout:      cfg.HTTP.WriteTimeout.Value(),
		IdleTimeout:       cfg.HTTP.IdleTimeout.Value(),
		MaxHeaderBytes:    cfg.HTTP.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logHandler, slog.LevelError),
		BaseContext:       func(net.Listener) context.Context { return lifetimeContext },
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	logger.Info("server ready", "address", listener.Addr().String(), "public_url", cfg.PublicURL, "base_path", cfg.BasePath)
	serveError := make(chan error, 1)
	go func() {
		serveError <- httpServer.Serve(listener)
	}()
	select {
	case <-signalContext.Done():
		logger.Info("shutting down")
	case err := <-serveError:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout.Value())
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		_ = httpServer.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveError; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func findConfigPath(args []string) (string, error) {
	var path string
	for index, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--config" || arg == "-config" {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
				return "", errors.New("flag needs an argument: -config")
			}
			path = args[index+1]
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--config="); ok {
			path = value
			continue
		}
		if value, ok := strings.CutPrefix(arg, "-config="); ok {
			path = value
		}
	}
	return path, nil
}

func splitList(value string) []string {
	result := make([]string, 0)
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
