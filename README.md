# tileserver-go

[![Test](https://github.com/ralscha/tileserver-go/actions/workflows/test.yml/badge.svg)](https://github.com/ralscha/tileserver-go/actions/workflows/test.yml)
[![Release](https://github.com/ralscha/tileserver-go/actions/workflows/release.yml/badge.svg)](https://github.com/ralscha/tileserver-go/actions/workflows/release.yml)

A fast, dependency-light HTTP tile server for OpenMapTiles and other MBTiles 1.3 archives. It serves vector tiles, pre-rendered raster tiles, TileJSON, and WMTS from one statically deployable Go binary. Rendering, styles, fonts, sprites, and browser UI concerns are intentionally left to clients.

## What it supports

- OpenMapTiles `.mbtiles` archives and the commonly used MBTiles 1.3 metadata fields
- Mapbox Vector Tiles (`.pbf`, `.mvt`, and `.vector.pbf` aliases)
- Pre-rendered PNG, JPEG, WebP, AVIF, and custom MIME-type MBTiles
- Correct XYZ HTTP coordinates over MBTiles' internally stored TMS rows
- TileJSON 3.0 with bounds, center, zooms, attribution, vector layers, and tilestats
- WMTS 1.0 GetCapabilities and GetTile through RESTful templates or KVP
- UTFGrid payload delivery when an archive contains a `grids` table
- HTTP `GET`, `HEAD`, byte ranges, ETags, `If-None-Match`, `If-Modified-Since`, gzip negotiation, CORS, and CDN-friendly cache headers
- Sharded, byte-bounded in-memory LRU cache for tile and UTFGrid representations, including negative lookups, with duplicate-miss request coalescing
- Pre-serialized TileJSON, catalog, health, and WMTS documents with a bounded cache for dynamic public base URLs
- Read-only SQLite pools with immutable files, mmap, prepared queries, and bounded concurrency
- Health/readiness endpoints, runtime status, Prometheus metrics, structured access logs, panic recovery, and graceful shutdown
- Canonical public URLs, reverse-proxy support, URL base paths, and allowed-host enforcement

The server directly serves raster tiles already present in MBTiles. Vector rendering belongs to consumers such as MapLibre, Mapbox, OpenLayers, browser applications, or mobile SDKs.

## Installation

### Pre-built binaries

Download the latest archive for Linux, macOS, or Windows from the
[GitHub Releases](https://github.com/ralscha/tileserver-go/releases) page. Builds are available for
amd64 and arm64. Each release includes a `checksums.txt` file for SHA-256 verification.

### Go install

Requires Go 1.27 or newer:

```sh
go install github.com/ralscha/tileserver-go/cmd/tileserver@latest
```

### Build from source

```sh
git clone https://github.com/ralscha/tileserver-go.git
cd tileserver-go
task build
```

## Quick start

1. Download an OpenMapTiles `.mbtiles` archive and place it in `data/`.
2. Start the server:

```sh
go run ./cmd/tileserver
```

3. Request the JSON catalog at <http://localhost:8080>.

Every `.mbtiles` file in `data/` is discovered at startup. The filename without its extension becomes the source ID. For example, `data/openmaptiles.mbtiles` provides:

```text
http://localhost:8080/data/openmaptiles.json
http://localhost:8080/data/openmaptiles/{z}/{x}/{y}.pbf
```

The server exits with an error when no valid sources are found. Sources are immutable for the
lifetime of the process; restart the server after adding or replacing an archive.

You can also pass files directly:

```sh
go run ./cmd/tileserver -- ./zurich_switzerland.mbtiles
```

OpenMapTiles data and schema have attribution requirements. Keep the archive's attribution metadata intact and display it in your map. See the [OpenMapTiles documentation](https://openmaptiles.org/docs/) and [OpenStreetMap copyright page](https://www.openstreetmap.org/copyright).

## Client examples

Client-specific code lives under [`examples`](examples), outside the Go server. The MapLibre
example includes its own style documents and loads MapLibre GL JS from UNPKG, keeping the server
client-agnostic and the repository small.

### MapLibre Liechtenstein demo

The repository includes a styled demo for
`osm-2020-02-10-v3.11_europe_liechtenstein.mbtiles`. From the repository root, run:

```powershell
go run ./cmd/tileserver -- ./osm-2020-02-10-v3.11_europe_liechtenstein.mbtiles
```

In another terminal, serve the standalone consumer:

```powershell
go run ./cmd/exampleserver
```

Then open <http://localhost:3000>. tileserver-go serves only the vector tiles and TileJSON. The
consumer owns the style and loads [MapLibre GL JS 6.5.0](https://github.com/maplibre/maplibre-gl-js)
from UNPKG. Its text labels use the public MapLibre demo glyph endpoint, so the example requires
internet access unless those dependencies are replaced with local files.

## HTTP endpoints

| Endpoint | Purpose |
| --- | --- |
| `/` | Source JSON catalog |
| `/index.json` | Machine-readable catalog |
| `/data.json` | All source TileJSON documents |
| `/data/{id}.json` | TileJSON 3.0 |
| `/data/{id}/{z}/{x}/{y}.{format}` | Vector or raster tile |
| `/tiles/{id}/{z}/{x}/{y}.{format}` | Tile endpoint alias |
| `/data/{id}/{z}/{x}/{y}.grid.json` | UTFGrid payload, if present |
| `/wmts?SERVICE=WMTS&REQUEST=GetCapabilities` | WMTS capabilities |
| `/data/{id}/wmts.xml` | Single-source WMTS capabilities |
| `/health`, `/healthz` | Process liveness |
| `/readyz` | Readiness after all tile sources have opened |
| `/status.json` | Runtime source/cache/database status |
| `/metrics` | Prometheus exposition |

WMTS KVP `GetTile` requests follow the WMTS 1.0 parameter contract and require `SERVICE=WMTS`,
`REQUEST=GetTile`, `VERSION=1.0.0`, `LAYER`, `STYLE=default`, `FORMAT`,
`TILEMATRIXSET=WebMercatorQuad`, `TILEMATRIX`, `TILEROW`, and `TILECOL`. Invalid KVP requests
return an OWS XML exception report.

## Configuration

Run `tileserver --help` for flags. JSON configuration is useful for production and multiple explicitly named sources:

```sh
go run ./cmd/tileserver --config config.example.json
```

`data_dir` is relative to the configuration file's directory. Relative `sources.*.path` values are
resolved beneath `data_dir`; absolute source paths are used unchanged. Command-line values override
the loaded configuration. A complete example is in [`config.example.json`](config.example.json).

For a public deployment, set both a canonical URL and allowed hosts:

```json
{
  "public_url": "https://maps.example.com",
  "allowed_hosts": ["maps.example.com"]
}
```

Only enable `trust_proxy` when the server is behind a trusted reverse proxy that overwrites `X-Forwarded-Host` and `X-Forwarded-Proto`.

## Docker

```sh
docker build -t tileserver-go .
docker run --rm -p 8080:8080 \
  -v "$PWD/data:/data:ro" \
  tileserver-go
```

Or adapt the included [`docker-compose.yml`](docker-compose.yml).

## Performance tuning

- Keep MBTiles on local SSD/NVMe storage. SQLite mmap and the operating-system page cache do most of the work after warm-up.
- Size `cache.size_mb` for frequently requested tile representations. Vector-tile gzip and identity responses are cached independently; raster requests share one identity entry.
- `database.max_connections` is per source. For SSD storage, begin near the available CPU count; very high values can add contention without increasing throughput.
- `database.page_cache_mb` is the total SQLite page-cache budget per source and is divided across that source's connection pool. `database.mmap_size_mb` is applied to each connection; mapped file pages are managed by the operating system.
- Put a CDN or caching reverse proxy in front of public installations. Tiles have immutable cache headers by default.
- Set `public_url` in production so metadata documents are serialized once during startup. Dynamic host-derived documents use a bounded base-URL cache.
- MBTiles files are opened with SQLite's immutable mode. Restart the server after replacing an archive; do not modify a served archive in place.
- Disable access logs for maximum throughput. Disabling metrics also removes request instrumentation and its response-writer allocation entirely.

Run benchmarks locally on representative hardware. The HTTP benchmarks use a counting discard
writer so response-body buffering does not distort allocation measurements:

```sh
task bench
```

## Verification

Install [Task](https://taskfile.dev/) to use the repository's cross-platform development commands:

```sh
task                 # list available tasks
task format          # modernize and format Go code
task test            # run all tests
task test:cover      # run tests and print coverage
task verify          # run tests, vet, and build a static binary
task lint            # run golangci-lint through Docker
task vuln            # scan reachable Go code for known vulnerabilities
task release:check   # validate the GoReleaser configuration
task release:snapshot # build local release archives without publishing
task release:smoke   # build and exercise the packaged host archive
task release:verify  # run every pre-release check
```

Tests construct real MBTiles databases and exercise the HTTP compatibility surface, compression variants, caching validators, TMS conversion, security boundaries, and WMTS.

## Continuous integration and releases

GitHub Actions checks formatting and module consistency, runs the race-enabled test suite on Linux
and the regular test suite on macOS and Windows, runs `go vet`, golangci-lint, and govulncheck,
builds a static binary and container image, and checks the binary metadata for every branch push
and pull request.

Pushing a semantic version tag such as `v1.0.0` or `v1.0.0-rc.1` first runs the complete verification
workflow. GoReleaser then builds all archives, and the host archive is unpacked and exercised with a
generated MBTiles database before the publishing build can create the GitHub release. Releases attach
Linux, macOS, and Windows archives for amd64 and arm64, plus SHA-256 checksums. Release binaries report
the tag, commit, and build time through `tileserver --version`.

```sh
task release-tag TAG=v1.0.0
```

The release workflow uses the repository's built-in `GITHUB_TOKEN`; no separate release secret is
required. Repository workflow permissions must allow GitHub Actions to write repository contents.

## Format references

- [MBTiles 1.3 specification](https://github.com/mapbox/mbtiles-spec/blob/master/1.3/spec.md)
- [TileJSON 3.0 specification](https://github.com/mapbox/tilejson-spec/blob/master/3.0.0/README.md)
- [OpenMapTiles vector schema](https://openmaptiles.org/schema/)
- [MapLibre Style Specification](https://maplibre.org/maplibre-style-spec/)
