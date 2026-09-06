package server

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic while serving request", "panic", recovered, "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
				// Once a wrapped handler has written headers, net/http cannot replace
				// the status code; writeError can only terminate the response body.
				s.writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) concurrencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness must remain responsive when tile work has exhausted the
		// request budget. Readiness remains limited so overloaded instances can
		// be removed from service by an orchestrator.
		if s.isLivenessPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.limit <- struct{}{}:
			defer func() { <-s.limit }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			s.writeError(w, http.StatusServiceUnavailable, "server is busy")
		}
	})
}

func (s *Server) isLivenessPath(path string) bool {
	return path == s.cfg.BasePath+"/health" || path == s.cfg.BasePath+"/healthz"
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
		origin := r.Header.Get("Origin")
		if s.cors.allowAll {
			// A wildcard CORS policy is a static response policy. Emit it even for
			// non-CORS requests so an intermediary cannot cache a response without
			// the header and later reuse it for a CORS request.
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Last-Modified, Content-Length, Content-Encoding, Accept-Ranges")
		} else {
			// The response varies by Origin even when the current request has no
			// Origin or has one that is not allowed.
			w.Header().Add("Vary", "Origin")
			if origin != "" && s.originAllowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				if s.cfg.CORS.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				w.Header().Set("Access-Control-Expose-Headers", "ETag, Last-Modified, Content-Length, Content-Encoding, Accept-Ranges")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) preflightMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		requestedMethod := r.Header.Get("Access-Control-Request-Method")
		isPreflight := r.Method == http.MethodOptions && origin != "" && requestedMethod != ""
		if isPreflight {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			if !s.originAllowed(origin) {
				s.writeError(w, http.StatusForbidden, "CORS origin is not allowed")
				return
			}
			if requestedMethod != http.MethodGet && requestedMethod != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD, OPTIONS")
				s.writeError(w, http.StatusMethodNotAllowed, "CORS method is not allowed")
				return
			}
			if !corsRequestHeadersAllowed(r.Header.Get("Access-Control-Request-Headers")) {
				s.writeError(w, http.StatusBadRequest, "CORS request header is not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Range, If-None-Match, If-Modified-Since")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func corsRequestHeadersAllowed(value string) bool {
	for entry := range strings.SplitSeq(value, ",") {
		switch strings.ToLower(strings.TrimSpace(entry)) {
		case "", "accept", "accept-encoding", "range", "if-none-match", "if-modified-since":
		default:
			return false
		}
	}
	return true
}

func (s *Server) originAllowed(origin string) bool {
	return s.cors.allows(origin)
}

func (s *Server) hostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := s.requestHost(r)
		if !s.hostAllowed(host) {
			s.writeError(w, http.StatusMisdirectedRequest, "host is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(hostport string) bool {
	return s.hosts.allows(hostport)
}

func normalizeHostname(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	var host string
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	} else {
		host = strings.Trim(hostport, "[]")
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (s *Server) requestHost(r *http.Request) string {
	if s.cfg.TrustProxy {
		if forwarded := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
			return forwarded
		}
	}
	return r.Host
}

func (s *Server) externalBaseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if s.cfg.TrustProxy {
		if forwarded := strings.ToLower(firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))); forwarded == "http" || forwarded == "https" {
			scheme = forwarded
		}
	}
	u := url.URL{Scheme: scheme, Host: s.requestHost(r), Path: s.cfg.BasePath}
	return strings.TrimRight(u.String(), "/")
}

func firstHeaderValue(value string) string {
	if before, _, ok := strings.Cut(value, ","); ok {
		value = before
	}
	return strings.TrimSpace(value)
}

func (s *Server) basePathMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == s.cfg.BasePath {
			clone := r.Clone(r.Context())
			clone.URL.Path = "/"
			next.ServeHTTP(w, clone)
			return
		}
		prefix := s.cfg.BasePath + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			s.writeError(w, http.StatusNotFound, "endpoint not found")
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = strings.TrimPrefix(r.URL.Path, s.cfg.BasePath)
		clone.URL.RawPath = ""
		next.ServeHTTP(w, clone)
	})
}

func (s *Server) observabilityMiddleware(next http.Handler) http.Handler {
	collectMetrics := s.cfg.Observability.Metrics
	logAccess := s.cfg.Observability.AccessLog
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		if collectMetrics {
			s.metrics.requests.Add(1)
			s.metrics.inFlight.Add(1)
			defer s.metrics.inFlight.Add(-1)
		}
		observed := &observedWriter{ResponseWriter: w}
		next.ServeHTTP(observed, r)
		status := observed.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(started)
		if collectMetrics {
			s.metrics.record(status, observed.bytes, duration)
		}
		if !logAccess {
			return
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}
		s.logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"status", status,
			"bytes", observed.bytes,
			"duration", duration,
			"remote", remoteIP(r.RemoteAddr),
		)
	})
}

func remoteIP(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message, "status": status})
}
