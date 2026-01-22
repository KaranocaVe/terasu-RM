package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const (
	defaultListen                = "127.0.0.1:5000"
	defaultReadHeaderTimeout     = 10 * time.Second
	defaultIdleTimeout           = 60 * time.Second
	defaultShutdownTimeout       = 5 * time.Second
	defaultMaxHeaderBytes        = 1 << 20
	defaultDialTimeout           = 10 * time.Second
	defaultKeepAlive             = 30 * time.Second
	defaultMaxIdleConns          = 256
	defaultMaxIdleConnsPerHost   = 64
	defaultIdleConnTimeout       = 90 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultExpectContinueTimeout = 1 * time.Second
	defaultFirstFragmentLen      = 3
)

// Config is loaded from JSON.
type Config struct {
	Listen        string          `json:"listen"`
	PublicBaseURL string          `json:"public_base_url"`
	AccessLog     bool            `json:"access_log"`
	TLS           *TLSConfig      `json:"tls"`
	Timeouts      ServerTimeouts  `json:"timeouts"`
	Transport     TransportConfig `json:"transport"`
	Limits        LimitsConfig    `json:"limits"`
	Routes        []RouteConfig   `json:"routes"`
}

type TLSConfig struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type ServerTimeouts struct {
	ReadHeaderTimeout string `json:"read_header_timeout"`
	ReadTimeout       string `json:"read_timeout"`
	WriteTimeout      string `json:"write_timeout"`
	IdleTimeout       string `json:"idle_timeout"`
	ShutdownTimeout   string `json:"shutdown_timeout"`
	MaxHeaderBytes    int    `json:"max_header_bytes"`
}

type TransportConfig struct {
	FirstFragmentLen      int    `json:"first_fragment_len"`
	DialTimeout           string `json:"dial_timeout"`
	KeepAlive             string `json:"keepalive"`
	MaxIdleConns          int    `json:"max_idle_conns"`
	MaxIdleConnsPerHost   int    `json:"max_idle_conns_per_host"`
	MaxConnsPerHost       int    `json:"max_conns_per_host"`
	IdleConnTimeout       string `json:"idle_conn_timeout"`
	TLSHandshakeTimeout   string `json:"tls_handshake_timeout"`
	ResponseHeaderTimeout string `json:"response_header_timeout"`
	ExpectContinueTimeout string `json:"expect_continue_timeout"`
	ForceHTTP2            bool   `json:"force_http2"`
	DisableCompression    bool   `json:"disable_compression"`
}

type LimitsConfig struct {
	MaxInflight     int    `json:"max_inflight"`
	MaxInflightWait string `json:"max_inflight_wait"`
}

type RouteConfig struct {
	Name         string `json:"name"`
	PublicPrefix string `json:"public_prefix"`
	Upstream     string `json:"upstream"`
	PreserveHost bool   `json:"preserve_host"`
}

type RuntimeConfig struct {
	Listen        string
	PublicBaseURL *url.URL
	AccessLog     bool
	TLS           *TLSConfig
	Timeouts      RuntimeTimeouts
	Transport     RuntimeTransport
	Limits        RuntimeLimits
	Routes        []RouteConfig
}

type RuntimeTimeouts struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
}

type RuntimeTransport struct {
	FirstFragmentLen      uint8
	DialTimeout           time.Duration
	KeepAlive             time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	IdleConnTimeout       time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	ExpectContinueTimeout time.Duration
	ForceHTTP2            bool
	DisableCompression    bool
}

type RuntimeLimits struct {
	MaxInflight     int
	MaxInflightWait time.Duration
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Runtime() (RuntimeConfig, error) {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	publicBase, err := parsePublicBaseURL(c.PublicBaseURL)
	if err != nil {
		return RuntimeConfig{}, err
	}
	readHeaderTimeout, err := parseDuration(c.Timeouts.ReadHeaderTimeout, defaultReadHeaderTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read_header_timeout: %w", err)
	}
	readTimeout, err := parseDuration(c.Timeouts.ReadTimeout, 0)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read_timeout: %w", err)
	}
	writeTimeout, err := parseDuration(c.Timeouts.WriteTimeout, 0)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("write_timeout: %w", err)
	}
	idleTimeout, err := parseDuration(c.Timeouts.IdleTimeout, defaultIdleTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("idle_timeout: %w", err)
	}
	shutdownTimeout, err := parseDuration(c.Timeouts.ShutdownTimeout, defaultShutdownTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("shutdown_timeout: %w", err)
	}
	maxHeaderBytes := c.Timeouts.MaxHeaderBytes
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = defaultMaxHeaderBytes
	}

	dialTimeout, err := parseDuration(c.Transport.DialTimeout, defaultDialTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("dial_timeout: %w", err)
	}
	keepAlive, err := parseDuration(c.Transport.KeepAlive, defaultKeepAlive)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("keepalive: %w", err)
	}
	idleConnTimeout, err := parseDuration(c.Transport.IdleConnTimeout, defaultIdleConnTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("idle_conn_timeout: %w", err)
	}
	tlsHandshakeTimeout, err := parseDuration(c.Transport.TLSHandshakeTimeout, defaultTLSHandshakeTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("tls_handshake_timeout: %w", err)
	}
	responseHeaderTimeout, err := parseDuration(c.Transport.ResponseHeaderTimeout, defaultResponseHeaderTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("response_header_timeout: %w", err)
	}
	expectContinueTimeout, err := parseDuration(c.Transport.ExpectContinueTimeout, defaultExpectContinueTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("expect_continue_timeout: %w", err)
	}
	maxInflight := c.Limits.MaxInflight
	if maxInflight < 0 {
		return RuntimeConfig{}, errors.New("max_inflight must be >= 0")
	}
	maxInflightWait, err := parseDuration(c.Limits.MaxInflightWait, 0)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("max_inflight_wait: %w", err)
	}

	maxIdleConns := c.Transport.MaxIdleConns
	if maxIdleConns <= 0 {
		maxIdleConns = defaultMaxIdleConns
	}
	maxIdleConnsPerHost := c.Transport.MaxIdleConnsPerHost
	if maxIdleConnsPerHost <= 0 {
		maxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	}
	firstFragmentLen := c.Transport.FirstFragmentLen
	if firstFragmentLen == 0 {
		firstFragmentLen = defaultFirstFragmentLen
	}
	if firstFragmentLen < 0 || firstFragmentLen > 255 {
		return RuntimeConfig{}, errors.New("first_fragment_len must be between 0 and 255")
	}

	cfg := RuntimeConfig{
		Listen:        c.Listen,
		PublicBaseURL: publicBase,
		AccessLog:     c.AccessLog,
		TLS:           c.TLS,
		Timeouts: RuntimeTimeouts{
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			ShutdownTimeout:   shutdownTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
		},
		Transport: RuntimeTransport{
			FirstFragmentLen:      uint8(firstFragmentLen),
			DialTimeout:           dialTimeout,
			KeepAlive:             keepAlive,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			MaxConnsPerHost:       c.Transport.MaxConnsPerHost,
			IdleConnTimeout:       idleConnTimeout,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			ExpectContinueTimeout: expectContinueTimeout,
			ForceHTTP2:            c.Transport.ForceHTTP2,
			DisableCompression:    c.Transport.DisableCompression,
		},
		Limits: RuntimeLimits{
			MaxInflight:     maxInflight,
			MaxInflightWait: maxInflightWait,
		},
		Routes: c.Routes,
	}
	if err := cfg.validateRoutes(); err != nil {
		return RuntimeConfig{}, err
	}
	return cfg, nil
}

func (c RuntimeConfig) validateRoutes() error {
	if len(c.Routes) == 0 {
		return errors.New("routes must not be empty")
	}
	seen := map[string]struct{}{}
	for i, route := range c.Routes {
		if route.Upstream == "" {
			return fmt.Errorf("routes[%d].upstream must not be empty", i)
		}
		if route.PublicPrefix == "" {
			route.PublicPrefix = "/"
		}
		prefix := normalizePath(route.PublicPrefix)
		if prefix == "/" {
			prefix = ""
		}
		if _, ok := seen[prefix]; ok {
			return fmt.Errorf("routes[%d].public_prefix duplicates another route", i)
		}
		seen[prefix] = struct{}{}
		if _, err := parseUpstream(route.Upstream); err != nil {
			return fmt.Errorf("routes[%d].upstream: %w", i, err)
		}
	}
	return nil
}

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	return time.ParseDuration(raw)
}

func parsePublicBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errors.New("public_base_url must include scheme and host")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("public_base_url must not include a path")
	}
	u.Path = ""
	u.RawPath = ""
	u.Fragment = ""
	u.RawQuery = ""
	return u, nil
}

func normalizePath(raw string) string {
	if raw == "" {
		return "/"
	}
	p := raw
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = path.Clean(p)
	if p == "." {
		p = "/"
	}
	if p != "/" && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func parseUpstream(raw string) (*url.URL, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return nil, errors.New("upstream must not be empty")
	}
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("upstream scheme must be http or https")
	}
	if u.Host == "" {
		return nil, errors.New("upstream must include host")
	}
	return u, nil
}

func DefaultConfig() Config {
	return Config{
		Listen:        defaultListen,
		PublicBaseURL: "",
		AccessLog:     true,
		Timeouts: ServerTimeouts{
			ReadHeaderTimeout: defaultReadHeaderTimeout.String(),
			ReadTimeout:       "",
			WriteTimeout:      "",
			IdleTimeout:       defaultIdleTimeout.String(),
			ShutdownTimeout:   defaultShutdownTimeout.String(),
			MaxHeaderBytes:    defaultMaxHeaderBytes,
		},
		Transport: TransportConfig{
			FirstFragmentLen:      defaultFirstFragmentLen,
			DialTimeout:           defaultDialTimeout.String(),
			KeepAlive:             defaultKeepAlive.String(),
			MaxIdleConns:          defaultMaxIdleConns,
			MaxIdleConnsPerHost:   defaultMaxIdleConnsPerHost,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       defaultIdleConnTimeout.String(),
			TLSHandshakeTimeout:   defaultTLSHandshakeTimeout.String(),
			ResponseHeaderTimeout: defaultResponseHeaderTimeout.String(),
			ExpectContinueTimeout: defaultExpectContinueTimeout.String(),
			ForceHTTP2:            true,
			DisableCompression:    false,
		},
		Limits: LimitsConfig{
			MaxInflight:     0,
			MaxInflightWait: "",
		},
		Routes: []RouteConfig{
			{
				Name:         "docker-registry",
				PublicPrefix: "/",
				Upstream:     "https://registry-1.docker.io",
				PreserveHost: false,
			},
			{
				Name:         "docker-auth",
				PublicPrefix: "/_auth",
				Upstream:     "https://auth.docker.io",
				PreserveHost: false,
			},
			{
				Name:         "docker-blob",
				PublicPrefix: "/_blob",
				Upstream:     "https://production.cloudflare.docker.com",
				PreserveHost: false,
			},
		},
	}
}
