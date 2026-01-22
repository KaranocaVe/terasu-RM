package mirror

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fumiama/terasu"
	"github.com/fumiama/terasu/dns"
	"github.com/fumiama/terasu/ip"
)

func NewTransport(cfg RuntimeTransport) http.RoundTripper {
	configureIPv6()
	primary := newBaseTransport(cfg)
	fallbackLens := fallbackFragmentLens(cfg.FirstFragmentLen)
	fallbacks := buildFallbackTransports(cfg, fallbackLens)
	if len(fallbacks) == 0 {
		return primary
	}
	return &fallbackRoundTripper{
		primary:           primary,
		primaryFragment:   cfg.FirstFragmentLen,
		fallbacks:         fallbacks,
		fallbackFragments: fallbackLens,
	}
}

func newBaseTransport(cfg RuntimeTransport) *http.Transport {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.ForceHTTP2 {
		tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	}

	dialer := &net.Dialer{
		Timeout:   cfg.DialTimeout,
		KeepAlive: cfg.KeepAlive,
	}
	baseDialer := &mirrorDialer{
		dialer:            dialer,
		firstFragmentLen:  cfg.FirstFragmentLen,
		tlsHandshakeLimit: cfg.TLSHandshakeTimeout,
		tlsConfig:         tlsConfig,
	}

	return &http.Transport{
		Proxy:                 nil,
		DialContext:           baseDialer.DialContext,
		DialTLSContext:        baseDialer.DialTLSContext,
		ForceAttemptHTTP2:     cfg.ForceHTTP2,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: cfg.ExpectContinueTimeout,
		DisableCompression:    cfg.DisableCompression,
		TLSClientConfig:       tlsConfig,
	}
}

func buildFallbackTransports(cfg RuntimeTransport, lens []uint8) []http.RoundTripper {
	if len(lens) == 0 {
		return nil
	}
	fallbacks := make([]http.RoundTripper, 0, len(lens))
	for _, frag := range lens {
		next := cfg
		next.FirstFragmentLen = frag
		fallbacks = append(fallbacks, newBaseTransport(next))
	}
	return fallbacks
}

func fallbackFragmentLens(current uint8) []uint8 {
	switch {
	case current > 1:
		return []uint8{1, 0}
	case current == 1:
		return []uint8{0}
	default:
		return nil
	}
}

type mirrorDialer struct {
	dialer            *net.Dialer
	firstFragmentLen  uint8
	tlsHandshakeLimit time.Duration
	tlsConfig         *tls.Config
}

var ipv6Once sync.Once

func configureIPv6() {
	ipv6Once.Do(func() {
		if hasIPv6DefaultRoute() && hasGlobalIPv6() {
			return
		}
		ip.IsIPv6Available = false
	})
}

func hasGlobalIPv6() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ipAddr net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ipAddr = v.IP
			case *net.IPAddr:
				ipAddr = v.IP
			}
			if ipAddr == nil {
				continue
			}
			ipAddr = ipAddr.To16()
			if ipAddr == nil || ipAddr.To4() != nil {
				continue
			}
			if ipAddr.IsGlobalUnicast() && !ipAddr.IsPrivate() {
				return true
			}
		}
	}
	return false
}

func hasIPv6DefaultRoute() bool {
	data, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil {
		return true
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[0] == "00000000000000000000000000000000" && fields[1] == "00" {
			if fields[9] == "lo" {
				continue
			}
			return true
		}
	}
	return false
}

func (d *mirrorDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := resolveHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no upstream addresses")
	}
	var lastErr error
	for _, ip := range addrs {
		dialCtx := ctx
		var cancel context.CancelFunc
		if d.dialer.Timeout > 0 {
			dialCtx, cancel = context.WithTimeout(ctx, d.dialer.Timeout)
		}
		conn, err := d.dialer.DialContext(dialCtx, network, net.JoinHostPort(ip, port))
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream dial succeeded")
	}
	return nil, lastErr
}

func (d *mirrorDialer) DialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := resolveHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no upstream addresses")
	}
	cfg := &tls.Config{}
	if d.tlsConfig != nil {
		cfg = d.tlsConfig.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	var lastErr error
	for _, ip := range addrs {
		conn, err := d.dialWithTimeout(ctx, network, net.JoinHostPort(ip, port))
		if err != nil {
			lastErr = err
			continue
		}
		tlsConn := tls.Client(conn, cfg)
		err = d.handshake(ctx, tlsConn)
		if err == nil {
			return tlsConn, nil
		}
		_ = tlsConn.Close()
		conn, err = d.dialWithTimeout(ctx, network, net.JoinHostPort(ip, port))
		if err != nil {
			lastErr = err
			continue
		}
		tlsConn = tls.Client(conn, cfg)
		if err = d.handshakePlain(ctx, tlsConn); err == nil {
			return tlsConn, nil
		}
		_ = tlsConn.Close()
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream dial succeeded")
	}
	return nil, lastErr
}

func (d *mirrorDialer) dialWithTimeout(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.dialer.Timeout <= 0 {
		return d.dialer.DialContext(ctx, network, addr)
	}
	dialCtx, cancel := context.WithTimeout(ctx, d.dialer.Timeout)
	defer cancel()
	return d.dialer.DialContext(dialCtx, network, addr)
}

func (d *mirrorDialer) handshake(ctx context.Context, conn *tls.Conn) error {
	hsCtx := ctx
	var cancel context.CancelFunc
	if d.tlsHandshakeLimit > 0 {
		hsCtx, cancel = context.WithTimeout(ctx, d.tlsHandshakeLimit)
	}
	if cancel != nil {
		defer cancel()
	}
	if d.firstFragmentLen > 0 {
		return terasu.Use(conn).HandshakeContext(hsCtx, d.firstFragmentLen)
	}
	return conn.HandshakeContext(hsCtx)
}

func (d *mirrorDialer) handshakePlain(ctx context.Context, conn *tls.Conn) error {
	hsCtx := ctx
	var cancel context.CancelFunc
	if d.tlsHandshakeLimit > 0 {
		hsCtx, cancel = context.WithTimeout(ctx, d.tlsHandshakeLimit)
	}
	if cancel != nil {
		defer cancel()
	}
	return conn.HandshakeContext(hsCtx)
}

func resolveHost(ctx context.Context, host string) ([]string, error) {
	if !ip.IsIPv6Available {
		ips, err := dns.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err == nil && len(ips) > 0 {
			return ipStrings(ips), nil
		}
	}
	addrs, err := dns.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if !ip.IsIPv6Available {
		addrs = filterIPv4(addrs)
		if len(addrs) == 0 {
			ips, err := dns.DefaultResolver.LookupIP(ctx, "ip4", host)
			if err == nil && len(ips) > 0 {
				return ipStrings(ips), nil
			}
		}
	}
	return addrs, nil
}

func filterIPv4(addrs []string) []string {
	out := addrs[:0]
	for _, addr := range addrs {
		if strings.Contains(addr, ":") {
			continue
		}
		out = append(out, addr)
	}
	return out
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ipAddr := range ips {
		if ipAddr == nil {
			continue
		}
		if v4 := ipAddr.To4(); v4 != nil {
			out = append(out, v4.String())
		} else {
			out = append(out, ipAddr.String())
		}
	}
	return out
}

type fallbackRoundTripper struct {
	primary           http.RoundTripper
	primaryFragment   uint8
	fallbacks         []http.RoundTripper
	fallbackFragments []uint8
	metrics           *metrics
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.primary.RoundTrip(req)
	if err == nil || !shouldRetry(req, err) {
		return resp, err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	prevFrag := f.primaryFragment
	for i, fallback := range f.fallbacks {
		nextFrag := prevFrag
		if i < len(f.fallbackFragments) {
			nextFrag = f.fallbackFragments[i]
		}
		if f.metrics != nil {
			f.metrics.observeFallback(prevFrag, nextFrag)
		}
		clone, cloneErr := cloneRequest(req)
		if cloneErr != nil {
			return resp, err
		}
		resp, err = fallback.RoundTrip(clone)
		if err == nil || !shouldRetry(clone, err) {
			return resp, err
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		prevFrag = nextFrag
	}
	return resp, err
}

func (f *fallbackRoundTripper) CloseIdleConnections() {
	if f == nil {
		return
	}
	if closer, ok := f.primary.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	for _, rt := range f.fallbacks {
		if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func shouldRetry(req *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if !canRetryRequest(req) {
		return false
	}
	return isConnReset(err)
}

func canRetryRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions {
		return true
	}
	if req.Body == nil || req.ContentLength == 0 {
		return true
	}
	return req.GetBody != nil
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	clone := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return clone, nil
	}
	if req.GetBody == nil {
		return clone, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone.Body = body
	return clone, nil
}

func isConnReset(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset by peer")
}
