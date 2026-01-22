package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	configPath := flag.String("config", "daemon.json", "path to daemon config JSON")
	validateOnly := flag.Bool("validate", false, "validate config and exit")
	printDefault := flag.Bool("print-default-config", false, "print a default daemon config to stdout")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("rmirrord version=%s commit=%s date=%s\n", version, commit, date)
		return
	}
	if *printDefault {
		cfg := DefaultDaemonConfig()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "print default config failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logger := newAppLogger()

	cfg, err := loadDaemonConfig(*configPath)
	if err != nil {
		logger.Fatal("load config failed", map[string]any{"error": err.Error()})
	}
	runtimeCfg, err := cfg.runtime(*configPath)
	if err != nil {
		logger.Fatal("invalid config", map[string]any{"error": err.Error()})
	}
	if *validateOnly {
		logger.Info("config ok", nil)
		return
	}

	logger.Info("startup", map[string]any{"version": version, "commit": commit, "date": date})
	supervisor := newSupervisor(logger)
	if err := supervisor.Apply(runtimeCfg); err != nil {
		logger.Fatal("start failed", map[string]any{"error": err.Error()})
	}

	stop := make(chan os.Signal, 1)
	reload := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	if runtime.GOOS != "windows" {
		signal.Notify(reload, syscall.SIGHUP)
	}

	var reloadMu sync.Mutex
	for {
		select {
		case sig := <-stop:
			logger.Info("signal received", map[string]any{"signal": sig.String()})
			supervisor.StopAll(runtimeCfg.shutdownTimeout)
			return
		case <-reload:
			reloadMu.Lock()
			cfg, err := loadDaemonConfig(*configPath)
			if err != nil {
				logger.Error("reload failed", map[string]any{"error": err.Error()})
				reloadMu.Unlock()
				continue
			}
			nextRuntime, err := cfg.runtime(*configPath)
			if err != nil {
				logger.Error("reload failed", map[string]any{"error": err.Error()})
				reloadMu.Unlock()
				continue
			}
			if err := supervisor.Apply(nextRuntime); err != nil {
				logger.Error("reload failed", map[string]any{"error": err.Error()})
			} else {
				logger.Info("reload succeeded", nil)
				runtimeCfg = nextRuntime
			}
			reloadMu.Unlock()
		}
	}
}

type DaemonConfig struct {
	Command         string           `json:"command"`
	WorkingDir      string           `json:"working_dir"`
	ShutdownTimeout string           `json:"shutdown_timeout"`
	Restart         RestartConfig     `json:"restart"`
	Instances       []InstanceConfig  `json:"instances"`
}

type RestartConfig struct {
	Enabled  *bool  `json:"enabled"`
	MinDelay string `json:"min_delay"`
	MaxDelay string `json:"max_delay"`
}

type InstanceConfig struct {
	Name           string            `json:"name"`
	Config         string            `json:"config"`
	CheckUpstreams bool              `json:"check_upstreams"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	Disabled       bool              `json:"disabled"`
	Command        string            `json:"command"`
	WorkingDir     string            `json:"working_dir"`
	Restart        *RestartConfig     `json:"restart"`
}

func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{
		ShutdownTimeout: "10s",
		Restart: RestartConfig{
			Enabled:  boolPtr(true),
			MinDelay: "1s",
			MaxDelay: "30s",
		},
		Instances: []InstanceConfig{
			{Name: "docker", Config: "examples/docker.json"},
			{Name: "github", Config: "examples/github.json"},
			{Name: "huggingface", Config: "examples/huggingface.json"},
		},
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func loadDaemonConfig(path string) (DaemonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DaemonConfig{}, err
	}
	var cfg DaemonConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return DaemonConfig{}, err
	}
	if dec.More() {
		return DaemonConfig{}, errors.New("unexpected trailing JSON")
	}
	return cfg, nil
}

type daemonRuntime struct {
	defaultCommand  string
	defaultWorkDir  string
	shutdownTimeout time.Duration
	defaultRestart  restartPolicy
	instances       []instanceSpec
}

type restartPolicy struct {
	enabled  bool
	minDelay time.Duration
	maxDelay time.Duration
}

type instanceSpec struct {
	name          string
	configPath    string
	command       string
	workingDir    string
	args          []string
	env           map[string]string
	restart       restartPolicy
	checkUpstreams bool
}

func (cfg DaemonConfig) runtime(path string) (daemonRuntime, error) {
	baseDir := filepath.Dir(path)
	shutdownTimeout := 10 * time.Second
	if cfg.ShutdownTimeout != "" {
		parsed, err := time.ParseDuration(cfg.ShutdownTimeout)
		if err != nil {
			return daemonRuntime{}, fmt.Errorf("shutdown_timeout: %w", err)
		}
		if parsed < 0 {
			return daemonRuntime{}, errors.New("shutdown_timeout must be >= 0")
		}
		shutdownTimeout = parsed
	}

	defaultRestart, err := parseRestart(cfg.Restart, restartPolicy{
		enabled:  true,
		minDelay: time.Second,
		maxDelay: 30 * time.Second,
	})
	if err != nil {
		return daemonRuntime{}, fmt.Errorf("restart: %w", err)
	}

	defaultCommand, err := resolveCommand(cfg.Command, baseDir)
	if err != nil {
		return daemonRuntime{}, fmt.Errorf("command: %w", err)
	}

	defaultWorkDir := cfg.WorkingDir
	if defaultWorkDir != "" {
		defaultWorkDir = resolvePath(baseDir, defaultWorkDir)
	}

	if len(cfg.Instances) == 0 {
		return daemonRuntime{}, errors.New("instances must not be empty")
	}

	seen := make(map[string]struct{}, len(cfg.Instances))
	instances := make([]instanceSpec, 0, len(cfg.Instances))
	for i, inst := range cfg.Instances {
		if strings.TrimSpace(inst.Name) == "" {
			return daemonRuntime{}, fmt.Errorf("instances[%d].name must not be empty", i)
		}
		if _, ok := seen[inst.Name]; ok {
			return daemonRuntime{}, fmt.Errorf("instances[%d].name duplicated", i)
		}
		seen[inst.Name] = struct{}{}
		if strings.TrimSpace(inst.Config) == "" {
			return daemonRuntime{}, fmt.Errorf("instances[%d].config must not be empty", i)
		}
		if hasFlag(inst.Args, "-config") || hasFlag(inst.Args, "-check-upstreams") {
			return daemonRuntime{}, fmt.Errorf("instances[%d].args must not include -config or -check-upstreams", i)
		}
		configPath := resolvePath(baseDir, inst.Config)
		if !inst.Disabled {
			if _, err := os.Stat(configPath); err != nil {
				return daemonRuntime{}, fmt.Errorf("instances[%d].config: %w", i, err)
			}
		}
		command := inst.Command
		if command == "" {
			command = defaultCommand
		} else {
			command, err = resolveCommand(command, baseDir)
			if err != nil {
				return daemonRuntime{}, fmt.Errorf("instances[%d].command: %w", i, err)
			}
		}
		workDir := inst.WorkingDir
		if workDir == "" {
			workDir = defaultWorkDir
		} else {
			workDir = resolvePath(baseDir, workDir)
		}
		restart := defaultRestart
		if inst.Restart != nil {
			restart, err = parseRestart(*inst.Restart, defaultRestart)
			if err != nil {
				return daemonRuntime{}, fmt.Errorf("instances[%d].restart: %w", i, err)
			}
		}

		args := []string{"-config", configPath}
		if inst.CheckUpstreams {
			args = append(args, "-check-upstreams")
		}
		args = append(args, inst.Args...)

		instances = append(instances, instanceSpec{
			name:          inst.Name,
			configPath:    configPath,
			command:       command,
			workingDir:    workDir,
			args:          args,
			env:           inst.Env,
			restart:       restart,
			checkUpstreams: inst.CheckUpstreams,
		})
	}

	return daemonRuntime{
		defaultCommand:  defaultCommand,
		defaultWorkDir:  defaultWorkDir,
		shutdownTimeout: shutdownTimeout,
		defaultRestart:  defaultRestart,
		instances:       instances,
	}, nil
}

func parseRestart(cfg RestartConfig, def restartPolicy) (restartPolicy, error) {
	out := def
	if cfg.Enabled != nil {
		out.enabled = *cfg.Enabled
	}
	if cfg.MinDelay != "" {
		parsed, err := time.ParseDuration(cfg.MinDelay)
		if err != nil {
			return restartPolicy{}, err
		}
		if parsed < 0 {
			return restartPolicy{}, errors.New("min_delay must be >= 0")
		}
		out.minDelay = parsed
	}
	if cfg.MaxDelay != "" {
		parsed, err := time.ParseDuration(cfg.MaxDelay)
		if err != nil {
			return restartPolicy{}, err
		}
		if parsed < 0 {
			return restartPolicy{}, errors.New("max_delay must be >= 0")
		}
		out.maxDelay = parsed
	}
	if out.maxDelay < out.minDelay {
		return restartPolicy{}, errors.New("max_delay must be >= min_delay")
	}
	return out, nil
}

func resolvePath(baseDir, value string) string {
	if value == "" {
		return value
	}
	if filepath.IsAbs(value) {
		return value
	}
	if baseDir == "" {
		return value
	}
	return filepath.Join(baseDir, value)
}

func resolveCommand(cmd, baseDir string) (string, error) {
	if cmd == "" {
		cmd = defaultCommand()
	}
	if !filepath.IsAbs(cmd) && strings.Contains(cmd, string(os.PathSeparator)) && baseDir != "" {
		cmd = filepath.Join(baseDir, cmd)
	}
	return exec.LookPath(cmd)
}

func defaultCommand() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		name := "rmirror"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "rmirror"
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

type supervisor struct {
	logger  *appLogger
	mu      sync.Mutex
	runners map[string]*runner
}

func newSupervisor(logger *appLogger) *supervisor {
	return &supervisor{
		logger:  logger,
		runners: make(map[string]*runner),
	}
}

func (s *supervisor) Apply(runtimeCfg daemonRuntime) error {
	desired := make(map[string]instanceSpec, len(runtimeCfg.instances))
	for _, inst := range runtimeCfg.instances {
		desired[inst.name] = inst
	}

	var toStop []*runner
	var toStart []instanceSpec
	var toReload []*runner

	s.mu.Lock()
	for name, runner := range s.runners {
		spec, ok := desired[name]
		if !ok {
			delete(s.runners, name)
			toStop = append(toStop, runner)
			continue
		}
		if !runner.spec.equal(spec) {
			delete(s.runners, name)
			toStop = append(toStop, runner)
			toStart = append(toStart, spec)
			continue
		}
		toReload = append(toReload, runner)
	}
	for name, spec := range desired {
		if _, ok := s.runners[name]; ok {
			continue
		}
		toStart = append(toStart, spec)
	}
	s.mu.Unlock()

	for _, runner := range toStop {
		runner.stop(runtimeCfg.shutdownTimeout)
	}
	for _, spec := range toStart {
		runner := newRunner(spec, s.logger)
		runner.start()
		s.mu.Lock()
		s.runners[spec.name] = runner
		s.mu.Unlock()
	}
	for _, runner := range toReload {
		if err := runner.reload(); err != nil {
			s.logger.Error("reload instance failed", map[string]any{"name": runner.spec.name, "error": err.Error()})
			runner.stop(runtimeCfg.shutdownTimeout)
			spec := desired[runner.spec.name]
			next := newRunner(spec, s.logger)
			next.start()
			s.mu.Lock()
			s.runners[spec.name] = next
			s.mu.Unlock()
		}
	}
	return nil
}

func (s *supervisor) StopAll(timeout time.Duration) {
	s.mu.Lock()
	runners := make([]*runner, 0, len(s.runners))
	for _, runner := range s.runners {
		runners = append(runners, runner)
	}
	s.runners = make(map[string]*runner)
	s.mu.Unlock()

	for _, runner := range runners {
		runner.stop(timeout)
	}
}

type runner struct {
	spec     instanceSpec
	logger   *appLogger
	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping atomic.Bool
	stopped  chan struct{}
	stopCh   chan struct{}
}

func newRunner(spec instanceSpec, logger *appLogger) *runner {
	return &runner{
		spec:    spec,
		logger: logger,
		stopped: make(chan struct{}),
		stopCh:  make(chan struct{}),
	}
}

func (r *runner) start() {
	go r.loop()
}

func (r *runner) loop() {
	defer close(r.stopped)
	backoff := r.spec.restart.minDelay

	for {
		if r.stopping.Load() {
			return
		}
		cmd := exec.Command(r.spec.command, r.spec.args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if r.spec.workingDir != "" {
			cmd.Dir = r.spec.workingDir
		}
		if len(r.spec.env) > 0 {
			cmd.Env = mergeEnv(os.Environ(), r.spec.env)
		}

		if err := cmd.Start(); err != nil {
			r.logger.Error("instance start failed", map[string]any{"name": r.spec.name, "error": err.Error()})
			if !r.spec.restart.enabled {
				return
			}
			r.sleepBackoff(backoff)
			backoff = nextBackoff(backoff, r.spec.restart.maxDelay)
			continue
		}
		r.setCmd(cmd)
		r.logger.Info("instance started", map[string]any{"name": r.spec.name, "pid": cmd.Process.Pid})
		err := cmd.Wait()
		r.clearCmd()
		if r.stopping.Load() {
			return
		}
		exitCode := exitStatus(err)
		fields := map[string]any{
			"name": r.spec.name,
			"code": exitCode,
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		r.logger.Error("instance exited", fields)
		if !r.spec.restart.enabled {
			return
		}
		r.sleepBackoff(backoff)
		backoff = nextBackoff(backoff, r.spec.restart.maxDelay)
	}
}

func (r *runner) reload() error {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return errors.New("instance not running")
	}
	if runtime.GOOS == "windows" {
		return errors.New("SIGHUP not supported on windows")
	}
	return cmd.Process.Signal(syscall.SIGHUP)
}

func (r *runner) stop(timeout time.Duration) {
	if r.stopping.CompareAndSwap(false, true) {
		close(r.stopCh)
	}
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = terminate(cmd.Process)
	}
	select {
	case <-r.stopped:
		return
	case <-time.After(timeout):
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-r.stopped
	}
}

func (r *runner) setCmd(cmd *exec.Cmd) {
	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()
}

func (r *runner) clearCmd() {
	r.mu.Lock()
	r.cmd = nil
	r.mu.Unlock()
}

func (r *runner) sleepBackoff(delay time.Duration) {
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	select {
	case <-timer.C:
	case <-r.stopCh:
	}
	timer.Stop()
}

func nextBackoff(current, max time.Duration) time.Duration {
	if current <= 0 {
		return current
	}
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func terminate(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return proc.Kill()
	}
	return proc.Signal(syscall.SIGTERM)
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func mergeEnv(base []string, override map[string]string) []string {
	if len(override) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(override))
	used := make(map[string]struct{}, len(override))
	for k := range override {
		used[k] = struct{}{}
	}
	for _, kv := range base {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 0 {
			continue
		}
		if _, ok := used[parts[0]]; ok {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(override))
	for k := range override {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+override[k])
	}
	return out
}

func (s instanceSpec) equal(other instanceSpec) bool {
	if s.name != other.name ||
		s.configPath != other.configPath ||
		s.command != other.command ||
		s.workingDir != other.workingDir ||
		s.checkUpstreams != other.checkUpstreams ||
		!restartEqual(s.restart, other.restart) {
		return false
	}
	if !stringSliceEqual(s.args, other.args) {
		return false
	}
	if !stringMapEqual(s.env, other.env) {
		return false
	}
	return true
}

func restartEqual(a, b restartPolicy) bool {
	return a.enabled == b.enabled && a.minDelay == b.minDelay && a.maxDelay == b.maxDelay
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
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
