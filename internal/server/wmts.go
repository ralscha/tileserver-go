package server

import (
	"bytes"
	"encoding/xml"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ralscha/tileserver-go/internal/mbtiles"
)

const (
	wmtsService   = "WMTS"
	wmtsVersion   = "1.0.0"
	wmtsStyle     = "default"
	wmtsMatrixSet = "WebMercatorQuad"
)

type wmtsMatrixLimits struct {
	minRow int
	maxRow int
	minCol int
	maxCol int
}

func (s *Server) serveWMTS(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	service := queryValue(query, "SERVICE")
	if service == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "SERVICE", "SERVICE is required")
		return
	}
	if !strings.EqualFold(service, wmtsService) {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "SERVICE", "SERVICE must be WMTS")
		return
	}
	requestValue := queryValue(query, "REQUEST")
	if requestValue == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "REQUEST", "REQUEST is required")
		return
	}
	request := strings.ToLower(requestValue)
	if request == "getcapabilities" {
		if version := queryValue(query, "VERSION"); version != "" && version != wmtsVersion {
			s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "VERSION", "VERSION must be 1.0.0")
			return
		}
		bundle := s.metadataForRequest(w, r)
		if bundle != nil {
			bundle.wmts.serve(w, r)
		}
		return
	}
	if request != "gettile" {
		s.writeOWSException(w, http.StatusNotImplemented, "OperationNotSupported", requestValue, "unsupported WMTS request")
		return
	}
	version := queryValue(query, "VERSION")
	if version == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "VERSION", "VERSION is required")
		return
	}
	if version != wmtsVersion {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "VERSION", "VERSION must be 1.0.0")
		return
	}
	id := queryValue(query, "LAYER")
	if id == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "LAYER", "LAYER is required")
		return
	}
	store := s.sources[id]
	if store == nil {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "LAYER", "source not found")
		return
	}
	style := queryValue(query, "STYLE")
	if style == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "STYLE", "STYLE is required")
		return
	}
	if style != wmtsStyle {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "STYLE", "STYLE must be default")
		return
	}
	format := queryValue(query, "FORMAT")
	if format == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "FORMAT", "FORMAT is required")
		return
	}
	if !strings.EqualFold(format, store.Metadata().ContentType) {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "FORMAT", "FORMAT is not supported for this layer")
		return
	}
	matrixSet := queryValue(query, "TILEMATRIXSET")
	if matrixSet == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "TILEMATRIXSET", "TILEMATRIXSET is required")
		return
	}
	if matrixSet != wmtsMatrixSet {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "TILEMATRIXSET", "TILEMATRIXSET must be WebMercatorQuad")
		return
	}
	matrix := queryValue(query, "TILEMATRIX")
	if matrix == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "TILEMATRIX", "TILEMATRIX is required")
		return
	}
	z, err := strconv.Atoi(matrix)
	if err != nil || z < 0 || z > 30 {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "TILEMATRIX", "TILEMATRIX must be an integer between 0 and 30")
		return
	}
	meta := store.Metadata()
	if z < meta.MinZoom || z > meta.MaxZoom {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "TILEMATRIX", "TILEMATRIX is outside the layer zoom range")
		return
	}
	rowText := queryValue(query, "TILEROW")
	if rowText == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "TILEROW", "TILEROW is required")
		return
	}
	row, err := strconv.Atoi(rowText)
	if err != nil || row < 0 {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "TILEROW", "TILEROW must be a non-negative integer")
		return
	}
	columnText := queryValue(query, "TILECOL")
	if columnText == "" {
		s.writeOWSException(w, http.StatusBadRequest, "MissingParameterValue", "TILECOL", "TILECOL is required")
		return
	}
	column, err := strconv.Atoi(columnText)
	if err != nil || column < 0 {
		s.writeOWSException(w, http.StatusBadRequest, "InvalidParameterValue", "TILECOL", "TILECOL must be a non-negative integer")
		return
	}
	width := int64(1) << z
	if int64(row) >= width {
		s.writeOWSException(w, http.StatusBadRequest, "TileOutOfRange", "TILEROW", "TILEROW is outside the tile matrix")
		return
	}
	if int64(column) >= width {
		s.writeOWSException(w, http.StatusBadRequest, "TileOutOfRange", "TILECOL", "TILECOL is outside the tile matrix")
		return
	}
	limits := webMercatorTileLimits(meta.Bounds, z)
	if row < limits.minRow || row > limits.maxRow {
		s.writeOWSException(w, http.StatusBadRequest, "TileOutOfRange", "TILEROW", "TILEROW is outside the layer limits")
		return
	}
	if column < limits.minCol || column > limits.maxCol {
		s.writeOWSException(w, http.StatusBadRequest, "TileOutOfRange", "TILECOL", "TILECOL is outside the layer limits")
		return
	}
	s.serveWMTSTile(w, r, store, z, column, row)
}

func (s *Server) serveWMTSTile(w http.ResponseWriter, r *http.Request, store *mbtiles.Store, z, x, y int) {
	if s.cfg.Observability.Metrics {
		s.metrics.tileRequests.Add(1)
	}
	value, loadErr := s.cachedTile(r, store, z, x, y)
	if loadErr != nil {
		if r.Context().Err() != nil {
			return
		}
		if s.cfg.Observability.Metrics {
			s.metrics.databaseErrors.Add(1)
		}
		s.logger.Error("load WMTS tile", "source", store.ID(), "z", z, "x", x, "y", y, "error", loadErr)
		s.writeOWSException(w, http.StatusInternalServerError, "NoApplicableCode", "", "failed to load tile")
		return
	}
	if value.NotFound {
		if s.cfg.Observability.Metrics {
			s.metrics.tileNotFound.Add(1)
		}
		s.writeOWSException(w, http.StatusInternalServerError, "NoApplicableCode", "", "tile is not available")
		return
	}
	name := strconv.Itoa(y) + "." + store.Metadata().Extension
	serveCachedResponse(w, r, name, store.ModTime(), s.tileCacheControl, value)
}

func queryValue(values map[string][]string, name string) string {
	for key, entries := range values {
		if strings.EqualFold(key, name) && len(entries) > 0 {
			return entries[0]
		}
	}
	return ""
}

func webMercatorTileLimits(bounds [4]float64, z int) wmtsMatrixLimits {
	size := math.Ldexp(1, z)
	west := (bounds[0] + 180) / 360 * size
	east := (bounds[2] + 180) / 360 * size
	north := webMercatorTileRow(bounds[3], size)
	south := webMercatorTileRow(bounds[1], size)
	minCol, maxCol := tileIndexRange(west, east, int(size))
	minRow, maxRow := tileIndexRange(north, south, int(size))
	return wmtsMatrixLimits{minRow: minRow, maxRow: maxRow, minCol: minCol, maxCol: maxCol}
}

func webMercatorTileRow(latitude, size float64) float64 {
	const mercatorLatitudeLimit = 85.0511287798066
	latitude = min(max(latitude, -mercatorLatitudeLimit), mercatorLatitudeLimit)
	radians := latitude * math.Pi / 180
	return (1 - math.Asinh(math.Tan(radians))/math.Pi) / 2 * size
}

func tileIndexRange(start, end float64, size int) (int, int) {
	minimum := min(max(int(math.Floor(start)), 0), size-1)
	maximum := max(min(max(int(math.Ceil(end))-1, 0), size-1), minimum)
	return minimum, maximum
}

func (s *Server) serveSourceWMTS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.sources[id] == nil {
		s.writeError(w, http.StatusNotFound, "source not found")
		return
	}
	bundle := s.metadataForRequest(w, r)
	if bundle != nil {
		bundle.sourceWMTS[id].serve(w, r)
	}
}

func (s *Server) buildWMTSCapabilities(baseURL string, ids []string) staticResponse {
	var body bytes.Buffer
	body.Grow(32 * 1024)
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	body.WriteString(`<Capabilities xmlns="http://www.opengis.net/wmts/1.0" xmlns:ows="http://www.opengis.net/ows/1.1" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://www.opengis.net/wmts/1.0 http://schemas.opengis.net/wmts/1.0/wmtsGetCapabilities_response.xsd" version="1.0.0">`)
	body.WriteString(`<ows:ServiceIdentification><ows:Title>tileserver-go</ows:Title><ows:ServiceType>OGC WMTS</ows:ServiceType><ows:ServiceTypeVersion>1.0.0</ows:ServiceTypeVersion></ows:ServiceIdentification>`)
	body.WriteString(`<ows:OperationsMetadata><ows:Operation name="GetCapabilities"><ows:DCP><ows:HTTP><ows:Get xlink:href="`)
	writeXMLText(&body, baseURL+"/wmts?")
	body.WriteString(`"><ows:Constraint name="GetEncoding"><ows:AllowedValues><ows:Value>KVP</ows:Value></ows:AllowedValues></ows:Constraint></ows:Get></ows:HTTP></ows:DCP></ows:Operation>`)
	body.WriteString(`<ows:Operation name="GetTile"><ows:DCP><ows:HTTP><ows:Get xlink:href="`)
	writeXMLText(&body, baseURL+"/wmts?")
	body.WriteString(`"><ows:Constraint name="GetEncoding"><ows:AllowedValues><ows:Value>KVP</ows:Value></ows:AllowedValues></ows:Constraint></ows:Get></ows:HTTP></ows:DCP></ows:Operation></ows:OperationsMetadata><Contents>`)
	maxZoom := 0
	var latest time.Time
	for _, id := range ids {
		store := s.sources[id]
		if store == nil {
			continue
		}
		meta := store.Metadata()
		if meta.MaxZoom > maxZoom {
			maxZoom = meta.MaxZoom
		}
		if store.ModTime().After(latest) {
			latest = store.ModTime()
		}
		body.WriteString(`<Layer><ows:Title>`)
		writeXMLText(&body, meta.Name)
		body.WriteString(`</ows:Title><ows:Identifier>`)
		writeXMLText(&body, id)
		body.WriteString(`</ows:Identifier><ows:WGS84BoundingBox><ows:LowerCorner>`)
		body.WriteString(strconv.FormatFloat(meta.Bounds[0], 'f', -1, 64))
		body.WriteByte(' ')
		body.WriteString(strconv.FormatFloat(meta.Bounds[1], 'f', -1, 64))
		body.WriteString(`</ows:LowerCorner><ows:UpperCorner>`)
		body.WriteString(strconv.FormatFloat(meta.Bounds[2], 'f', -1, 64))
		body.WriteByte(' ')
		body.WriteString(strconv.FormatFloat(meta.Bounds[3], 'f', -1, 64))
		body.WriteString(`</ows:UpperCorner></ows:WGS84BoundingBox><Style isDefault="true"><ows:Identifier>default</ows:Identifier></Style><Format>`)
		writeXMLText(&body, meta.ContentType)
		body.WriteString(`</Format><TileMatrixSetLink><TileMatrixSet>WebMercatorQuad</TileMatrixSet><TileMatrixSetLimits>`)
		for z := meta.MinZoom; z <= meta.MaxZoom; z++ {
			limits := webMercatorTileLimits(meta.Bounds, z)
			body.WriteString(`<TileMatrixLimits><TileMatrix>`)
			body.WriteString(strconv.Itoa(z))
			body.WriteString(`</TileMatrix><MinTileRow>`)
			body.WriteString(strconv.Itoa(limits.minRow))
			body.WriteString(`</MinTileRow><MaxTileRow>`)
			body.WriteString(strconv.Itoa(limits.maxRow))
			body.WriteString(`</MaxTileRow><MinTileCol>`)
			body.WriteString(strconv.Itoa(limits.minCol))
			body.WriteString(`</MinTileCol><MaxTileCol>`)
			body.WriteString(strconv.Itoa(limits.maxCol))
			body.WriteString(`</MaxTileCol></TileMatrixLimits>`)
		}
		body.WriteString(`</TileMatrixSetLimits></TileMatrixSetLink><ResourceURL format="`)
		writeXMLText(&body, meta.ContentType)
		body.WriteString(`" resourceType="tile" template="`)
		writeXMLText(&body, baseURL+"/data/"+id+"/{TileMatrix}/{TileCol}/{TileRow}."+meta.Extension)
		body.WriteString(`"/></Layer>`)
	}
	body.WriteString(`<TileMatrixSet><ows:Title>Google Maps Compatible for the World</ows:Title><ows:Identifier>WebMercatorQuad</ows:Identifier><ows:SupportedCRS>urn:ogc:def:crs:EPSG::3857</ows:SupportedCRS><WellKnownScaleSet>urn:ogc:def:wkss:OGC:1.0:GoogleMapsCompatible</WellKnownScaleSet>`)
	for z := 0; z <= maxZoom; z++ {
		body.WriteString(`<TileMatrix><ows:Identifier>`)
		body.WriteString(strconv.Itoa(z))
		body.WriteString(`</ows:Identifier><ScaleDenominator>`)
		body.WriteString(strconv.FormatFloat(559082264.0287178/float64(uint64(1)<<z), 'f', 12, 64))
		body.WriteString(`</ScaleDenominator><TopLeftCorner>-20037508.342789244 20037508.342789244</TopLeftCorner><TileWidth>256</TileWidth><TileHeight>256</TileHeight><MatrixWidth>`)
		body.WriteString(strconv.FormatUint(uint64(1)<<z, 10))
		body.WriteString(`</MatrixWidth><MatrixHeight>`)
		body.WriteString(strconv.FormatUint(uint64(1)<<z, 10))
		body.WriteString(`</MatrixHeight></TileMatrix>`)
	}
	body.WriteString(`</TileMatrixSet></Contents></Capabilities>`)
	return newStaticResponse("wmts.xml", "application/xml; charset=utf-8", body.Bytes(), latest, s.metadataCacheControl)
}

func writeXMLText(builder *bytes.Buffer, value string) {
	_ = xml.EscapeText(builder, []byte(value))
}

func (s *Server) writeOWSException(w http.ResponseWriter, status int, code, locator, message string) {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ows:ExceptionReport xmlns:ows="http://www.opengis.net/ows/1.1" version="1.0.0"><ows:Exception exceptionCode="`)
	writeXMLText(&body, code)
	body.WriteByte('"')
	if locator != "" {
		body.WriteString(` locator="`)
		writeXMLText(&body, locator)
		body.WriteByte('"')
	}
	body.WriteString(`><ows:ExceptionText>`)
	writeXMLText(&body, message)
	body.WriteString(`</ows:ExceptionText></ows:Exception></ows:ExceptionReport>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
