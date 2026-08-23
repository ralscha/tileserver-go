# MapLibre consumer example

This is a standalone browser client for tileserver-go. MapLibre GL JS is loaded from UNPKG and is not part of the server binary or Go package.

From the repository root, start tileserver-go with the included Liechtenstein archive and style:

```powershell
go run ./cmd/tileserver -- ./osm-2020-02-10-v3.11_europe_liechtenstein.mbtiles
```

In another terminal, serve this directory:

```powershell
python -m http.server 3000 --directory ./examples/maplibre
```

Open <http://localhost:3000>. The example defaults to `http://localhost:8080` and the included Liechtenstein style. The server URL, source ID, and example style can be changed with query parameters:

```text
http://localhost:3000/?server=http://localhost:8080&source=osm-2020-02-10-v3.11_europe_liechtenstein
http://localhost:3000/?style=minimal&source=openmaptiles
```

The style JSON files belong to this consumer example. tileserver-go only supplies the TileJSON and tile URLs.
