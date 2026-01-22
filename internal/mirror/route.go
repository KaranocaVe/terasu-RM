package mirror

import (
	"net/url"
	"strings"

	"net/http/httputil"
)

type route struct {
	name              string
	publicPrefix      string
	publicPrefixSlash string
	upstream          *url.URL
	upstreamBasePath  string
	preserveHost      bool
	proxy             *httputil.ReverseProxy
}

func newRoute(cfg RouteConfig) (*route, error) {
	if cfg.PublicPrefix == "" {
		cfg.PublicPrefix = "/"
	}
	prefix := normalizePath(cfg.PublicPrefix)
	if prefix == "" {
		prefix = "/"
	}
	upstream, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	basePath := normalizePath(upstream.Path)
	if basePath == "" {
		basePath = "/"
	}
	upstream.Path = basePath
	upstream.RawPath = ""
	upstream.RawQuery = ""
	upstream.Fragment = ""

	r := &route{
		name:         cfg.Name,
		publicPrefix: prefix,
		upstream:     upstream,
		preserveHost: cfg.PreserveHost,
	}
	if prefix == "/" {
		r.publicPrefixSlash = "/"
	} else {
		r.publicPrefixSlash = prefix + "/"
	}
	if basePath == "/" {
		r.upstreamBasePath = "/"
	} else {
		r.upstreamBasePath = strings.TrimSuffix(basePath, "/")
		if r.upstreamBasePath == "" {
			r.upstreamBasePath = "/"
		}
	}
	return r, nil
}

func (r *route) matchesPath(path string) bool {
	if r.publicPrefix == "/" {
		return true
	}
	if path == r.publicPrefix {
		return true
	}
	return strings.HasPrefix(path, r.publicPrefixSlash)
}

func (r *route) stripPrefix(path string) string {
	if r.publicPrefix == "/" {
		if path == "" {
			return "/"
		}
		return path
	}
	trimmed := strings.TrimPrefix(path, r.publicPrefix)
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return trimmed
}

func (r *route) joinUpstreamPath(path string) string {
	return joinPaths(r.upstreamBasePath, path)
}

func (r *route) mapUpstreamPath(upstreamPath string) string {
	if r.upstreamBasePath != "/" && hasPathPrefix(upstreamPath, r.upstreamBasePath) {
		if upstreamPath == r.upstreamBasePath {
			upstreamPath = "/"
		} else {
			upstreamPath = strings.TrimPrefix(upstreamPath, r.upstreamBasePath)
			if upstreamPath == "" {
				upstreamPath = "/"
			}
		}
	}
	return joinPaths(r.publicPrefix, upstreamPath)
}

func joinPaths(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	default:
		return a + b
	}
}

func hasPathPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}
