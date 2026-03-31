// Command firefork-server exposes the firefork primitive over HTTP.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/fork"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
	"github.com/JustAnotherDevv/firefork-ai/internal/workload"
)

// server holds the long-lived state for the HTTP layer. There is one
// Pool, one registry handle, one optional WarmPool, and one auth token
// per process.
type server struct {
	log       *slog.Logger
	pool      *fork.Pool
	registry  *template.Registry
	warmPool  *fork.WarmPool
	fcBin     string
	jailerCfg *fc.JailerConfig
	authToken string // empty = demo mode, no auth

	// secrets caches the per-template HMAC secret bytes so /exec doesn't
	// have to re-decode the hex string on every call. Filled lazily on
	// first /fork for that template.
	secretsMu sync.Mutex
	secrets   map[string][]byte // template_key -> secret bytes

	// counters surface request-level metrics alongside the Pool's
	// internal warm-pool stats.
	totalForks atomic.Int64
	totalExecs atomic.Int64
	totalErrs  atomic.Int64
}

func main() {
	var (
		addr       = flag.String("addr", envOr("FIREFORK_ADDR", ":8080"), "listen address")
		registryP  = flag.String("registry", envOr("FIREFORK_REGISTRY", "/var/lib/firefork/registry/templates.json"), "registry JSON path")
		fcBin      = flag.String("firecracker", envOr("FIREFORK_FIRECRACKER", "/usr/local/bin/firecracker"), "firecracker binary path")
		jailerBin  = flag.String("jailer", envOr("FIREFORK_JAILER", ""), "jailer binary; when set, cold forks run under per-fork chroot")
		jailerUID  = flag.Int("jailer-uid", 10000, "uid for jailer drop")
		jailerGID  = flag.Int("jailer-gid", 10000, "gid for jailer drop")
		jailerBase = flag.String("jailer-base", "/srv/jailer", "jailer chroot base dir")
		warmPoolSz = flag.Int("warm-pool", 0, "warm pool size (0 disables)")
		ultraWarm  = flag.Bool("ultra-warm", false, "preload snapshot into each warm slot (requires --warm-pool > 0 and --warm-template)")
		warmTpl    = flag.String("warm-template", "", "template <name>/<version> to preload into the warm pool (required with --ultra-warm)")
		readTo     = flag.Duration("read-timeout", 30*time.Second, "HTTP read timeout")
		writeTo    = flag.Duration("write-timeout", 60*time.Second, "HTTP write timeout")
		idleTo     = flag.Duration("idle-timeout", 90*time.Second, "HTTP keep-alive idle timeout")
		logJSON    = flag.Bool("log-json", false, "JSON logs")
	)
	flag.Parse()

	var handler slog.Handler
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	log := slog.New(handler)

	reg, err := template.OpenRegistry(*registryP)
	if err != nil {
		log.Error("open registry", "err", err)
		os.Exit(1)
	}

	pool := fork.NewPool()

	srv := &server{
		log:       log,
		pool:      pool,
		registry:  reg,
		fcBin:     *fcBin,
		authToken: os.Getenv("FIREFORK_AUTH_TOKEN"),
		secrets:   map[string][]byte{},
	}
	if *jailerBin != "" {
		srv.jailerCfg = &fc.JailerConfig{
			JailerBin:      *jailerBin,
			FirecrackerBin: *fcBin,
			UID:            *jailerUID,
			GID:            *jailerGID,
			ChrootBaseDir:  *jailerBase,
		}
		log.Info("jailer enabled", "uid", *jailerUID, "gid", *jailerGID, "base", *jailerBase)
	}
	if srv.authToken == "" {
		log.Warn("FIREFORK_AUTH_TOKEN unset — running in DEMO mode without auth. Do not expose to hostile networks.")
	}

	// Optional warm pool preload. Requires the warm template to already
	// be in the registry; if --ultra-warm is set, we load its snapshot
	// into every slot at startup.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *warmPoolSz > 0 {
		var wp *fork.WarmPool
		if *ultraWarm {
			if *warmTpl == "" {
				log.Error("--ultra-warm requires --warm-template <name>/<version>")
				os.Exit(2)
			}
			name, version, err := template.ParseKey(*warmTpl)
			if err != nil {
				log.Error("--warm-template invalid", "err", err)
				os.Exit(2)
			}
			entry := reg.Get(name, version)
			if entry == nil || entry.LocalMemFile == "" {
				log.Error("warm template not in registry / missing local snapshot", "key", *warmTpl)
				os.Exit(1)
			}
			wpCtx, wpCancel := context.WithTimeout(ctx, 30*time.Second)
			snap := fc.SnapshotPaths{MemFilePath: entry.LocalMemFile, StatePath: entry.LocalStateFile}
			wp, err = fork.NewUltraWarmPool(wpCtx, *warmPoolSz, *fcBin, "", snap)
			wpCancel()
			if err != nil {
				log.Error("ultra-warm pool init", "err", err)
				os.Exit(1)
			}
		} else {
			wpCtx, wpCancel := context.WithTimeout(ctx, 30*time.Second)
			wp, err = fork.NewWarmPool(wpCtx, *warmPoolSz, *fcBin, "")
			wpCancel()
			if err != nil {
				log.Error("warm pool init", "err", err)
				os.Exit(1)
			}
		}
		pool.WithWarmPool(wp)
		srv.warmPool = wp
		log.Info("warm pool ready", "size", *warmPoolSz, "ultra", *ultraWarm)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.Handle("POST /v1/fork", srv.auth(srv.handleFork))
	mux.Handle("POST /v1/exec", srv.auth(srv.handleExec))
	mux.Handle("DELETE /v1/forks/{id}", srv.auth(srv.handleDelete))
	mux.Handle("GET /v1/forks", srv.auth(srv.handleListForks))
	mux.Handle("GET /v1/templates", srv.auth(srv.handleListTemplates))
	mux.Handle("GET /v1/metrics", srv.auth(srv.handleMetrics))

	httpSrv := &http.Server{
		Addr:         *addr,
		Handler:      srv.logMiddleware(mux),
		ReadTimeout:  *readTo,
		WriteTimeout: *writeTo,
		IdleTimeout:  *idleTo,
	}

	go func() {
		log.Info("firefork-server listening", "addr", *addr, "registry", *registryP)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("ListenAndServe", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	// Graceful HTTP shutdown first (drain in-flight requests), then
	// pool teardown (kill every live fork).
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	stopped, failed := pool.Shutdown()
	log.Info("pool shutdown", "stopped", stopped, "failed", failed,
		"total_forks", srv.totalForks.Load(),
		"total_execs", srv.totalExecs.Load(),
		"total_errs", srv.totalErrs.Load())
}

// ---------------------------------------------------------------------------
func (s *server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", ww.status, "dur_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// auth wraps a handler with Bearer-token enforcement. When
func (s *server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken == "" {
			h(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.authToken
		if got != want {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	})
}

// ---------------------------------------------------------------------------
type forkReq struct {
	Template           string `json:"template"`             // <name>/<version>
	Count              int    `json:"count"`                // forks to spawn
	CombinedLoadResume bool   `json:"combined_load_resume"` // single /snapshot/load with resume_vm=true
	TimeoutMs          int    `json:"timeout_ms"`           // optional dispatch deadline (default 60_000)
}

type forkResp struct {
	Forks []forkInfo `json:"forks"`
}

type forkInfo struct {
	ID          string `json:"id"`
	VsockUDS    string `json:"vsock_uds"` // host path; "<dir>_<port>" once a port is appended by Firecracker
	WorkDir     string `json:"work_dir"`
	LatencyMs   int64  `json:"latency_ms"`
	PreloadMs   int64  `json:"preload_ms"`
	E2eMs       int64  `json:"e2e_ms"`
	TemplateKey string `json:"template_key"`
	Err         string `json:"error,omitempty"`
}

func (s *server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.totalErrs.Add(1)
		writeErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if req.Count <= 0 {
		writeErr(w, http.StatusBadRequest, "count must be > 0")
		return
	}
	if req.Count > 256 {
		writeErr(w, http.StatusBadRequest, "count > 256 not permitted")
		return
	}
	name, version, err := template.ParseKey(req.Template)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "template invalid: "+err.Error())
		return
	}
	entry := s.registry.Get(name, version)
	if entry == nil {
		writeErr(w, http.StatusNotFound, "template not in registry: "+req.Template)
		return
	}
	if entry.LocalMemFile == "" || entry.LocalStateFile == "" {
		writeErr(w, http.StatusFailedDependency, "template has no local snapshot (remote-only not supported)")
		return
	}
	if _, err := os.Stat(entry.LocalMemFile); err != nil {
		writeErr(w, http.StatusFailedDependency, "local memfile missing: "+err.Error())
		return
	}

	// Cache the secret for /exec.
	if entry.AgentSecretHex != "" {
		if secret, derr := hex.DecodeString(entry.AgentSecretHex); derr == nil {
			s.secretsMu.Lock()
			s.secrets[entry.Key()] = secret
			s.secretsMu.Unlock()
		} else {
			s.log.Warn("agent secret hex decode failed", "template", entry.Key(), "err", derr)
		}
	}

	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	snap := fc.SnapshotPaths{MemFilePath: entry.LocalMemFile, StatePath: entry.LocalStateFile}
	results, err := s.pool.Fork(ctx, fork.Request{
		Snapshot:       snap,
		Count:          req.Count,
		FirecrackerBin: s.fcBin,
		Jailer:         s.jailerCfg,
		Opts: fork.Optimizations{
			CombinedLoadResume: req.CombinedLoadResume,
			// WarmPoolSize/UltraWarmPool are server-startup flags, not
			// per-request — Pool already has the warm pool attached.
		},
	})
	if err != nil {
		s.totalErrs.Add(1)
		writeErr(w, http.StatusInternalServerError, "fork: "+err.Error())
		return
	}

	out := forkResp{Forks: make([]forkInfo, 0, len(results))}
	successes := 0
	for _, r := range results {
		info := forkInfo{
			ID:          r.ID,
			VsockUDS:    filepath.Join(r.WorkDir, "vsock.sock"),
			WorkDir:     r.WorkDir,
			LatencyMs:   r.Latency.Milliseconds(),
			PreloadMs:   r.PreloadCost.Milliseconds(),
			E2eMs:       (r.Latency + r.PreloadCost).Milliseconds(),
			TemplateKey: entry.Key(),
		}
		if r.Err != nil {
			info.Err = r.Err.Error()
			s.totalErrs.Add(1)
		} else {
			successes++
		}
		out.Forks = append(out.Forks, info)
	}
	s.totalForks.Add(int64(successes))
	writeJSON(w, http.StatusOK, out)
}

type execReq struct {
	ForkID    string   `json:"fork_id"`
	Cmd       string   `json:"cmd"`        // shell command (sh -c); see internal/workload/doc.go for attack surface notes
	Argv      []string `json:"argv"`       // exec form (alternative to cmd); preferred when args contain shell metacharacters
	TimeoutMs int      `json:"timeout_ms"` // default 30_000
}

type execResp struct {
	ForkID    string `json:"fork_id"`
	LatencyMs int64  `json:"latency_ms"`
	Result    any    `json:"result"` // raw agent reply (stdout/stderr/exit_code keys)
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.totalErrs.Add(1)
		writeErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if req.ForkID == "" {
		writeErr(w, http.StatusBadRequest, "fork_id required")
		return
	}
	if req.Cmd == "" && len(req.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, "cmd or argv required")
		return
	}

	live := s.pool.Live()
	res, ok := live[req.ForkID]
	if !ok {
		writeErr(w, http.StatusNotFound, "fork not live: "+req.ForkID)
		return
	}
	vsockUDS := filepath.Join(res.WorkDir, "vsock.sock")

	// Look up the secret by inverse-mapping WorkDir's template — easier:
	s.secretsMu.Lock()
	var secret []byte
	for _, v := range s.secrets {
		secret = v
		break
	}
	s.secretsMu.Unlock()

	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cmd := map[string]any{}
	if len(req.Argv) > 0 {
		cmd["cmd"] = "exec"
		cmd["argv"] = req.Argv
	} else {
		cmd["cmd"] = "exec"
		cmd["sh"] = req.Cmd
	}

	start := time.Now()
	resp, err := workload.Call(ctx, vsockUDS, workload.AgentPort, secret, cmd)
	lat := time.Since(start)
	if err != nil {
		s.totalErrs.Add(1)
		writeErr(w, http.StatusBadGateway, "vsock call: "+err.Error())
		return
	}
	s.totalExecs.Add(1)
	writeJSON(w, http.StatusOK, execResp{
		ForkID:    req.ForkID,
		LatencyMs: lat.Milliseconds(),
		Result:    resp,
	})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	existed, err := s.pool.Release(id)
	if !existed {
		writeErr(w, http.StatusNotFound, "fork not live: "+id)
		return
	}
	if err != nil {
		// Teardown errored mid-way; the fork is no longer tracked. Report
		// 207 (Multi-Status spirit) — caller saw partial success.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id":      id,
			"warning": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "released": true})
}

func (s *server) handleListForks(w http.ResponseWriter, r *http.Request) {
	live := s.pool.Live()
	out := make([]forkInfo, 0, len(live))
	for id, res := range live {
		out = append(out, forkInfo{
			ID:        id,
			VsockUDS:  filepath.Join(res.WorkDir, "vsock.sock"),
			WorkDir:   res.WorkDir,
			LatencyMs: res.Latency.Milliseconds(),
			PreloadMs: res.PreloadCost.Milliseconds(),
			E2eMs:     (res.Latency + res.PreloadCost).Milliseconds(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "forks": out})
}

func (s *server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	entries := s.registry.List()
	type tplInfo struct {
		Name      string    `json:"name"`
		Version   string    `json:"version"`
		Key       string    `json:"key"`
		VCPUs     int       `json:"vcpus"`
		MemMiB    int64     `json:"mem_mib"`
		CreatedAt time.Time `json:"created_at"`
		Notes     string    `json:"notes,omitempty"`
		HasLocal  bool      `json:"has_local_snapshot"`
	}
	out := make([]tplInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, tplInfo{
			Name:      e.Name,
			Version:   e.Version,
			Key:       e.Key(),
			VCPUs:     e.VCPUs,
			MemMiB:    e.MemMiB,
			CreatedAt: e.CreatedAt,
			Notes:     e.Notes,
			HasLocal:  e.LocalMemFile != "" && e.LocalStateFile != "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "templates": out})
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := map[string]any{
		"live_forks":  s.pool.Count(),
		"total_forks": s.totalForks.Load(),
		"total_execs": s.totalExecs.Load(),
		"total_errs":  s.totalErrs.Load(),
	}
	if s.warmPool != nil {
		takes, drains := s.warmPool.TakeStats()
		total := takes + drains
		hitRate := 0.0
		if total > 0 {
			hitRate = float64(takes) * 100 / float64(total)
		}
		m["warm_pool"] = map[string]any{
			"takes":         takes,
			"drains":        drains,
			"hit_rate_pct":  hitRate,
			"refill_errs":   s.warmPool.RefillErrors(),
		}
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"live_forks": s.pool.Count(),
	})
}

// ---------------------------------------------------------------------------
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

