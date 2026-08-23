package server

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) serveWMTS(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	request := strings.ToLower(queryValue(query, "REQUEST"))
	if request == "" || request == "getcapabilities" {
		bundle := s.metadataForRequest(w, r)
		if bundle != nil {
			bundle.wmts.serve(w, r)
		}
		return
	}
	if request != "gettile" {
		s.writeOWSException(w, http.StatusBadRequest, "OperationNotSupported", "REQUEST", "unsupported WMTS request")
		return
	}
	id := queryValue(query, "LAYER")
	store := s.sources[id]
	if store == nil {
		s.writeOWSException(w, http.StatusNotFound, "InvalidParameterValue", "LAYER", "source not found")
		return
	}
	matrix := queryValue(query, "TILEMATRIX")
	if index := strings.LastIndexByte(matrix, ':'); index >= 0 {
		matrix = matrix[index+1:]
	}
	tile := queryValue(query, "TILEROW") + "." + store.Metadata().Extension
	s.serveTile(w, r, id, matrix, queryValue(query, "TILECOL"), tile)
}

func queryValue(values map[string][]string, name string) string {
	for key, entries := range values {
		if strings.EqualFold(key, name) && len(entries) > 0 {
			return entries[0]
		}
	}
	return ""
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
		body.WriteString(`</Format><TileMatrixSetLink><TileMatrixSet>WebMercatorQuad</TileMatrixSet></TileMatrixSetLink><ResourceURL format="`)
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
	body.WriteString(`" locator="`)
	writeXMLText(&body, locator)
	body.WriteString(`"><ows:ExceptionText>`)
	writeXMLText(&body, message)
	body.WriteString(`</ows:ExceptionText></ows:Exception></ows:ExceptionReport>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
