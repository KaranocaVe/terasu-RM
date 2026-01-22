package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"rewriting-mirror/internal/mirror"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config JSON")
	validateOnly := flag.Bool("validate", false, "validate config and exit")
	printDefault := flag.Bool("print-default-config", false, "print a default config to stdout")
	showVersion := flag.Bool("version", false, "print version and exit")
	checkUpstreams := flag.Bool("check-upstreams", false, "check upstreams before serving")
	flag.Parse()

	if *showVersion {
		fmt.Printf("rmirror version=%s commit=%s date=%s\n", version, commit, date)
		return
	}
	if *printDefault {
		cfg := mirror.DefaultConfig()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "print default config failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logger := newAppLogger()

	cfg, err := mirror.LoadConfig(*configPath)
	if err != nil {
		logger.Fatal("load config failed", map[string]any{"error": err.Error()})
	}
	runtime, err := cfg.Runtime()
	if err != nil {
		logger.Fatal("invalid config", map[string]any{"error": err.Error()})
	}
	if *validateOnly {
		logger.Info("config ok", nil)
		return
	}
	logger.Info("startup", map[string]any{"version": version, "commit": commit, "date": date})

	transport := mirror.NewTransport(runtime.Transport)
	if *checkUpstreams {
		logger.Info("upstream check started", nil)
		if err := runUpstreamChecks(runtime, transport); err != nil {
			logger.Fatal("upstream check failed", map[string]any{"error": err.Error()})
		}
		logger.Info("upstream check ok", nil)
	}

	handler := newDynamicHandler()
	proxy, err := mirror.New(runtime, transport)
	if err != nil {
		logger.Fatal("failed to initialize mirror", map[string]any{"error": err.Error()})
	}
	handler.Store(&activeState{runtime: runtime, transport: transport, handler: proxy.Handler()})

	srv := &http.Server{
		Addr:              runtime.Listen,
		Handler:           handler,
		ReadHeaderTimeout: runtime.Timeouts.ReadHeaderTimeout,
		ReadTimeout:       runtime.Timeouts.ReadTimeout,
		WriteTimeout:      runtime.Timeouts.WriteTimeout,
		IdleTimeout:       runtime.Timeouts.IdleTimeout,
		MaxHeaderBytes:    runtime.Timeouts.MaxHeaderBytes,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", map[string]any{"addr": runtime.Listen})
		if runtime.TLS != nil {
			errCh <- srv.ListenAndServeTLS(runtime.TLS.CertFile, runtime.TLS.KeyFile)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	reload := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	signal.Notify(reload, syscall.SIGHUP)

	var reloadMu sync.Mutex
	go func() {
		for range reload {
			reloadMu.Lock()
			if err := reloadConfig(*configPath, *checkUpstreams, handler); err != nil {
				logger.Error("reload failed", map[string]any{"error": err.Error()})
			} else {
				logger.Info("reload succeeded", nil)
			}
			reloadMu.Unlock()
		}
	}()

	select {
	case sig := <-stop:
		logger.Info("signal received", map[string]any{"signal": sig.String()})
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", map[string]any{"error": err.Error()})
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), runtime.Timeouts.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", map[string]any{"error": err.Error()})
	}
}

type activeState struct {
	runtime   mirror.RuntimeConfig
	transport http.RoundTripper
	handler   http.Handler
}

type dynamicHandler struct {
	current atomic.Value
}

func newDynamicHandler() *dynamicHandler {
	return &dynamicHandler{}
}

func (d *dynamicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	state, ok := d.current.Load().(*activeState)
	if !ok || state == nil || state.handler == nil {
		http.Error(w, "handler unavailable", http.StatusServiceUnavailable)
		return
	}
	state.handler.ServeHTTP(w, r)
}

func (d *dynamicHandler) Store(state *activeState) {
	d.current.Store(state)
}

func reloadConfig(path string, checkUpstreams bool, handler *dynamicHandler) error {
	cfg, err := mirror.LoadConfig(path)
	if err != nil {
		return err
	}
	runtime, err := cfg.Runtime()
	if err != nil {
		return err
	}
	transport := mirror.NewTransport(runtime.Transport)
	if checkUpstreams {
		if err := runUpstreamChecks(runtime, transport); err != nil {
			return err
		}
	}
	proxy, err := mirror.New(runtime, transport)
	if err != nil {
		return err
	}
	next := &activeState{runtime: runtime, transport: transport, handler: proxy.Handler()}
	prev, _ := handler.current.Load().(*activeState)
	handler.Store(next)
	if prev != nil {
		if closer, ok := prev.transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
	return nil
}

func runUpstreamChecks(runtime mirror.RuntimeConfig, transport http.RoundTripper) error {
	timeout := runtime.Transport.ResponseHeaderTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	var failures []string
	for _, route := range runtime.Routes {
		target, err := parseUpstreamURL(route.Upstream)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if target.Path == "" {
			target.Path = "/"
		}
		if err := checkUpstream(client, target.String()); err != nil {
			failures = append(failures, target.String()+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func parseUpstreamURL(raw string) (*url.URL, error) {
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

func checkUpstream(client *http.Client, target string) error {
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil || (resp != nil && resp.StatusCode == http.StatusMethodNotAllowed) {
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), timeout)
		defer cancel2()
		req, err = http.NewRequestWithContext(ctx2, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err = client.Do(req)
	}
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		return err
	}
	if resp.StatusCode >= 500 {
		return errors.New("upstream returned " + resp.Status)
	}
	return nil
}

type appLogger struct {
	logger *log.Logger
}

func newAppLogger() *appLogger {
	return &appLogger{logger: log.New(os.Stdout, "", 0)}
}

func (l *appLogger) Info(msg string, fields map[string]any) {
	l.log("info", msg, fields)
}

func (l *appLogger) Error(msg string, fields map[string]any) {
	l.log("error", msg, fields)
}

func (l *appLogger) Fatal(msg string, fields map[string]any) {
	l.log("error", msg, fields)
	os.Exit(1)
}

func (l *appLogger) log(level, msg string, fields map[string]any) {
	entry := map[string]any{
		"ts":    time.Now().Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	if fields != nil {
		for k, v := range fields {
			entry[k] = v
		}
	}
	data, err := json.Marshal(entry)
	if err != nil {
		l.logger.Printf("{\"ts\":%q,\"level\":%q,\"msg\":%q,\"error\":%q}", time.Now().Format(time.RFC3339Nano), level, msg, err.Error())
		return
	}
	l.logger.Print(string(data))
}
