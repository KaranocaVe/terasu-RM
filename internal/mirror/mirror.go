package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Mirror struct {
	routes           []*route
	routesByUpstream []*route
	transport        http.RoundTripper
	publicBase       *publicBase
	accessLog        bool
	maxInflight      chan struct{}
	maxInflightWait  time.Duration
	metrics          *metrics
	metricsHandler   http.Handler
	logger           *structuredLogger
}

type publicBase struct {
	Scheme string
	Host   string
}

type ctxKey int

const (
	ctxPublicBaseKey ctxKey = iota
	ctxRouteKey
)

func New(cfg RuntimeConfig, transport http.RoundTripper) (*Mirror, error) {
	if transport == nil {
		return nil, errors.New("transport must not be nil")
	}
	routes, err := buildRoutes(cfg)
	if err != nil {
		return nil, err
	}
	m := &Mirror{
		routes:    routes,
		transport: transport,
		accessLog: cfg.AccessLog,
	}
	if cfg.PublicBaseURL != nil {
		m.publicBase = &publicBase{Scheme: cfg.PublicBaseURL.Scheme, Host: cfg.PublicBaseURL.Host}
	}
	m.metrics = newMetrics()
	m.metricsHandler = newMetricsHandler(m.metrics.registry)
	m.logger = newStructuredLogger()
	m.routesByUpstream = append([]*route(nil), routes...)
	sort.SliceStable(m.routesByUpstream, func(i, j int) bool {
		return len(m.routesByUpstream[i].upstreamBasePath) > len(m.routesByUpstream[j].upstreamBasePath)
	})
	for _, r := range routes {
		r.proxy = m.buildProxy(r)
	}
	if cfg.Limits.MaxInflight > 0 {
		m.maxInflight = make(chan struct{}, cfg.Limits.MaxInflight)
		m.maxInflightWait = cfg.Limits.MaxInflightWait
	}
	if fallback, ok := transport.(*fallbackRoundTripper); ok {
		fallback.metrics = m.metrics
	}
	return m, nil
}

func (m *Mirror) Handler() http.Handler {
	return m
}

func (m *Mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.serveInternal(w, r) {
		return
	}
	start := time.Now()
	rw := &logResponseWriter{ResponseWriter: w, status: 0}
	route := m.matchRoute(r.URL.Path)
	routeLabel := routeMetricLabel(route, r.URL.Path)
	if route == nil {
		http.Error(rw, "no route matched", http.StatusNotFound)
	} else {
		if !m.acquire(rw, r) {
			m.recordRequest(routeLabel, r, rw, time.Since(start))
			return
		}
		if m.metrics != nil {
			m.metrics.inflight.Inc()
			defer m.metrics.inflight.Dec()
		}
		defer m.release()
		route.proxy.ServeHTTP(rw, r)
	}
	m.recordRequest(routeLabel, r, rw, time.Since(start))
}

func buildRoutes(cfg RuntimeConfig) ([]*route, error) {
	routes := make([]*route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		r, err := newRoute(rc)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", rc.Name, err)
		}
		routes = append(routes, r)
	}
	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[i].publicPrefix) > len(routes[j].publicPrefix)
	})
	return routes, nil
}

func (m *Mirror) matchRoute(path string) *route {
	for _, r := range m.routes {
		if r.matchesPath(path) {
			return r
		}
	}
	return nil
}

func (m *Mirror) buildProxy(r *route) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Director:       m.director(r),
		Transport:      m.transport,
		ModifyResponse: m.modifyResponse,
		ErrorHandler:   m.errorHandler,
		FlushInterval:  100 * time.Millisecond,
	}
	return proxy
}

func (m *Mirror) director(r *route) func(*http.Request) {
	return func(req *http.Request) {
		publicBase := m.resolvePublicBase(req)
		ctx := context.WithValue(req.Context(), ctxPublicBaseKey, publicBase)
		ctx = context.WithValue(ctx, ctxRouteKey, r)
		*req = *req.WithContext(ctx)

		trimmed := r.stripPrefix(req.URL.Path)
		req.URL.Scheme = r.upstream.Scheme
		req.URL.Host = r.upstream.Host
		req.URL.Path = r.joinUpstreamPath(trimmed)
		req.URL.RawPath = ""
		if !r.preserveHost {
			req.Host = r.upstream.Host
		}
	}
}

func (m *Mirror) resolvePublicBase(req *http.Request) publicBase {
	if m.publicBase != nil {
		return *m.publicBase
	}
	scheme := schemeFromRequest(req)
	return publicBase{Scheme: scheme, Host: req.Host}
}

func (m *Mirror) modifyResponse(resp *http.Response) error {
	ctx := resp.Request.Context()
	pb, ok := ctx.Value(ctxPublicBaseKey).(publicBase)
	if !ok || pb.Host == "" || pb.Scheme == "" {
		return nil
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if rewritten, ok := m.rewriteURL(loc, pb); ok {
			resp.Header.Set("Location", rewritten)
		}
	}
	values := resp.Header.Values("WWW-Authenticate")
	if len(values) > 0 {
		changed := false
		newValues := make([]string, 0, len(values))
		for _, value := range values {
			updated, ok := m.rewriteAuthHeader(value, pb)
			if ok {
				changed = true
				newValues = append(newValues, updated)
			} else {
				newValues = append(newValues, value)
			}
		}
		if changed {
			resp.Header.Del("WWW-Authenticate")
			for _, value := range newValues {
				resp.Header.Add("WWW-Authenticate", value)
			}
		}
	}
	return nil
}

func (m *Mirror) rewriteURL(raw string, pb publicBase) (string, bool) {
	u, err := parseAbsoluteURL(raw)
	if err != nil {
		return "", false
	}
	route := m.matchUpstreamURL(u)
	if route == nil {
		return "", false
	}
	mappedPath := route.mapUpstreamPath(u.Path)
	if pb.Host == "" || pb.Scheme == "" {
		return "", false
	}
	newURL := *u
	newURL.Scheme = pb.Scheme
	newURL.Host = pb.Host
	newURL.Path = mappedPath
	newURL.RawPath = ""
	return newURL.String(), true
}

func (m *Mirror) matchUpstreamURL(u *url.URL) *route {
	if u == nil || u.Host == "" {
		return nil
	}
	for _, r := range m.routesByUpstream {
		if !strings.EqualFold(u.Host, r.upstream.Host) {
			continue
		}
		if r.upstreamBasePath != "/" && !hasPathPrefix(u.Path, r.upstreamBasePath) {
			continue
		}
		if r.upstream.Scheme != "" && u.Scheme != "" && !strings.EqualFold(u.Scheme, r.upstream.Scheme) {
			continue
		}
		return r
	}
	return nil
}

func (m *Mirror) rewriteAuthHeader(value string, pb publicBase) (string, bool) {
	lower := strings.ToLower(value)
	idx := 0
	changed := false
	var b strings.Builder
	for {
		pos := strings.Index(lower[idx:], "realm=")
		if pos < 0 {
			break
		}
		pos += idx
		b.WriteString(value[idx:pos])
		b.WriteString(value[pos : pos+len("realm=")])
		start := pos + len("realm=")
		if start >= len(value) {
			idx = start
			break
		}
		if value[start] == '"' {
			end := strings.IndexByte(value[start+1:], '"')
			if end < 0 {
				idx = start
				break
			}
			end = start + 1 + end
			realm := value[start+1 : end]
			if rewritten, ok := m.rewriteURL(realm, pb); ok {
				b.WriteByte('"')
				b.WriteString(rewritten)
				b.WriteByte('"')
				changed = true
			} else {
				b.WriteString(value[start : end+1])
			}
			idx = end + 1
			continue
		}
		end := start
		for end < len(value) && value[end] != ',' {
			end++
		}
		realm := strings.TrimSpace(value[start:end])
		if rewritten, ok := m.rewriteURL(realm, pb); ok {
			b.WriteString(rewritten)
			changed = true
		} else {
			b.WriteString(value[start:end])
		}
		idx = end
	}
	b.WriteString(value[idx:])
	return b.String(), changed
}

func (m *Mirror) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	msg := "upstream error"
	if errors.Is(err, context.Canceled) {
		status = http.StatusRequestTimeout
		msg = "request canceled"
	}
	if m.logger != nil {
		m.logger.Error("upstream error", map[string]any{
			"method": r.Method,
			"url":    r.URL.String(),
			"error":  err.Error(),
		})
	}
	routeLabel := routeMetricLabel(m.matchRoute(r.URL.Path), r.URL.Path)
	if m.metrics != nil {
		m.metrics.observeUpstreamError(routeLabel)
	}
	http.Error(w, msg, status)
}

func schemeFromRequest(req *http.Request) string {
	proto := req.Header.Get("X-Forwarded-Proto")
	if proto != "" {
		parts := strings.Split(proto, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" && u.Host == "" {
		return nil, errors.New("not absolute")
	}
	return u, nil
}

func (m *Mirror) serveInternal(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/_rmirror/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return true
	case "/_rmirror/readyz":
		if m.maxInflight != nil && len(m.maxInflight) >= cap(m.maxInflight) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return true
	case "/metrics":
		if m.metricsHandler != nil {
			m.metricsHandler.ServeHTTP(w, r)
			return true
		}
		http.Error(w, "metrics unavailable", http.StatusNotFound)
		return true
	default:
		return false
	}
}

func (m *Mirror) acquire(w http.ResponseWriter, r *http.Request) bool {
	if m.maxInflight == nil {
		return true
	}
	if m.maxInflightWait <= 0 {
		select {
		case m.maxInflight <- struct{}{}:
			return true
		default:
			http.Error(w, "server busy", http.StatusTooManyRequests)
			return false
		}
	}
	timer := time.NewTimer(m.maxInflightWait)
	defer timer.Stop()
	select {
	case m.maxInflight <- struct{}{}:
		return true
	case <-timer.C:
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return false
	case <-r.Context().Done():
		http.Error(w, "request canceled", http.StatusRequestTimeout)
		return false
	}
}

func (m *Mirror) release() {
	if m.maxInflight == nil {
		return
	}
	select {
	case <-m.maxInflight:
	default:
	}
}

func (m *Mirror) recordRequest(routeLabel string, r *http.Request, rw *logResponseWriter, elapsed time.Duration) {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	reqBytes := r.ContentLength
	if reqBytes < 0 {
		reqBytes = 0
	}
	if m.metrics != nil {
		m.metrics.observeRequest(routeLabel, r.Method, status, elapsed, reqBytes, rw.bytes)
	}
	if m.accessLog && m.logger != nil {
		fields := map[string]any{
			"method":   r.Method,
			"path":     r.URL.Path,
			"status":   status,
			"bytes":    rw.bytes,
			"duration": elapsed.Milliseconds(),
			"route":    routeLabel,
		}
		if route := m.matchRoute(r.URL.Path); route != nil {
			fields["upstream"] = route.upstream.Host
		}
		m.logger.Info("request", fields)
	}
}

func routeMetricLabel(route *route, path string) string {
	if route == nil {
		return "unmatched"
	}
	if route.name != "" {
		return route.name
	}
	if route.publicPrefix == "" {
		return "/"
	}
	return route.publicPrefix
}

type logResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (l *logResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *logResponseWriter) Write(p []byte) (int, error) {
	if l.status == 0 {
		l.status = http.StatusOK
	}
	n, err := l.ResponseWriter.Write(p)
	l.bytes += int64(n)
	return n, err
}
