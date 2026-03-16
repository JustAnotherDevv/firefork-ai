// Package fork implements the CoW fork primitive: given a single
// source snapshot, spawn N Firecracker processes that each
// MAP_PRIVATE the same memfile. Reads of clean pages share the host
// page cache; writes go to per-fork anonymous copy-on-write pages.
package fork

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JustAnotherDevv/firefork-ai/internal/cliutil"
	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
)

// Request describes a fork operation.
type Request struct {
	// Snapshot is the (memfile, state) pair to restore from. All N
	// forks share the same memfile read-only via MAP_PRIVATE.
	Snapshot fc.SnapshotPaths

	// Count is the number of forks to spawn.
	Count int

	// WorkDir is where per-fork temp directories (socket, jailer-like
	// chroots) are created. Defaults to os.TempDir.
	WorkDir string

	// FirecrackerBin overrides the default firecracker binary path.
	FirecrackerBin string

	// Opts toggles the optimizations defined in options.go. Zero
	// value = conservative defaults matching v0.1 behavior.
	Opts Optimizations

	// Jailer, when non-nil, runs each cold-path fork inside its own
	// /usr/local/bin/jailer chroot with privilege drop to
	// Jailer.UID:Jailer.GID. Jailer.ID is overridden per fork (the
	// supplied value, if any, is ignored — each fork gets a fresh UUID).
	Jailer *fc.JailerConfig
}

// Result is the outcome of forking one VM.
type Result struct {
	// ID is a per-fork UUID used in directory names + logs.
	ID string

	// Machine is the live restored VM, or nil if Err is non-nil.
	Machine *fc.Machine

	// SocketPath is the Firecracker API socket for this fork.
	SocketPath string

	// WorkDir is the fork's temp directory (caller may want to clean up).
	WorkDir string

	// Latency from fork dispatch to "Running" (Resume returned). Excludes
	// guest userspace warmup; the snapshot already captured that.
	Latency time.Duration

	// PreloadCost is the per-slot wall-clock cost that was paid before
	// this fork's Take returned: subprocess spawn for the warm path,
	// plus PUT /snapshot/load for the ultra-warm path. Zero for cold
	// forks (where Latency already covers the whole work).
	PreloadCost time.Duration

	// Err is non-nil if the fork failed.
	Err error

	// warmCmd is non-nil when this Result was produced via the warm
	// (or ultra-warm) pool path. The SDK Machine pointer is nil in
	// that case, so Shutdown uses this handle to kill the underlying
	// Firecracker process directly. For cold-path Results this is nil
	// and StopVMM via the SDK Machine does the work.
	warmCmd *exec.Cmd

	// jailer, when non-nil, is the per-fork JailerConfig used to
	// launch this fork. Shutdown calls fc.CleanupChroot(jailer) after
	// killing the firecracker process to remove the per-instance
	// chroot tree under /srv/jailer/firecracker/<id>/root/.
	jailer *fc.JailerConfig
}

// Pool spawns and tracks live forks. Safe for concurrent use; a
// single Pool instance can drive many concurrent Fork calls.
type Pool struct {
	mu   sync.Mutex
	live map[string]*Result

	// warmPool, if non-nil, supplies pre-spawned Firecracker processes
	// to skip subprocess startup in the fork hot path. Configured via
	// Pool.WithWarmPool.
	warmPool *WarmPool
}

func NewPool() *Pool { return &Pool{live: map[string]*Result{}} }

// WithWarmPool attaches a WarmPool to this Pool. Subsequent Fork calls
// will draw from the warm pool first, falling back to cold spawn if it
// runs dry. The Pool takes ownership and will Close it on Shutdown.
func (p *Pool) WithWarmPool(wp *WarmPool) *Pool {
	p.mu.Lock()
	p.warmPool = wp
	p.mu.Unlock()
	return p
}

// Fork spawns Count microVMs from the same snapshot in parallel.
func (p *Pool) Fork(ctx context.Context, req Request) ([]*Result, error) {
	if req.Count <= 0 {
		return nil, fmt.Errorf("fork: Count must be > 0")
	}
	if req.Snapshot.MemFilePath == "" || req.Snapshot.StatePath == "" {
		return nil, fmt.Errorf("fork: snapshot paths required")
	}
	if err := req.Opts.Validate(); err != nil {
		return nil, fmt.Errorf("fork: invalid Opts: %w", err)
	}
	workRoot := req.WorkDir
	if workRoot == "" {
		workRoot = os.TempDir()
	}

	out := make([]*Result, req.Count)
	var wg sync.WaitGroup
	for i := 0; i < req.Count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Convert panics in forkOne to an Err on this Result so
			// a single bad fork doesn't take the orchestrator down.
			defer func() {
				if r := recover(); r != nil {
					out[i] = &Result{Err: fmt.Errorf("fork goroutine panic: %v", r)}
				}
			}()
			out[i] = p.forkOne(ctx, req, workRoot)
		}(i)
	}
	wg.Wait()
	// Surface ctx cancellation distinctly from per-fork errors so the
	// caller can tell "I cancelled" from "all forks legitimately failed".
	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("fork: context: %w", err)
	}
	return out, nil
}

// forkOne handles a single fork. Returns a Result with Err set on failure.
func (p *Pool) forkOne(ctx context.Context, req Request, workRoot string) *Result {
	id := uuid.NewString()
	start := time.Now()

	// Try warm path first if a pool is attached.
	p.mu.Lock()
	wp := p.warmPool
	p.mu.Unlock()

	if wp != nil {
		slot, err := wp.Take()
		if err == nil && slot != nil {
			var (
				m    *fc.Machine
				lerr error
			)
			if slot.Preloaded {
				// Ultra-warm path (Tier A1): snapshot is already
				// loaded + paused on this slot. Fork = single
				// PATCH /vm Resumed.
				m, lerr = fc.ResumeOnSocket(ctx, slot.SocketPath)
			} else {
				// Standard warm path: drive snapshot/load on the
				// existing API socket. For jailed slots the host
				// paths in req.Snapshot are unreachable from inside
				// the chroot — hardlink them under the canonical
				// /memfile.bin, /state.bin locations and rewrite the
				// SnapshotPaths the API call sees. Mirrors the
				// snapForSlot logic used by Preload for ultra-warm.
				loadSnap := req.Snapshot
				if slot.jailer != nil {
					var prepErr error
					loadSnap, prepErr = wp.PrepareSnapForJailedSlot(slot, req.Snapshot)
					if prepErr != nil {
						lerr = fmt.Errorf("warm-jailed prep: %w", prepErr)
					}
				}
				if lerr == nil {
					m, lerr = fc.RestoreOnSocket(ctx, slot.SocketPath, loadSnap, fc.RestoreOptions{
						MemBackend:         fc.MemBackendFile,
						ResumeOnLoad:       true,
						CombinedLoadResume: req.Opts.CombinedLoadResume,
					})
				}
			}
			if lerr != nil {
				_ = slot.Cmd.Process.Kill()
				if slot.jailer != nil {
					_ = fc.CleanupChroot(*slot.jailer)
				} else {
					_ = os.RemoveAll(slot.WorkDir)
				}
				wp.Refill(ctx)
				return &Result{ID: id, WorkDir: slot.WorkDir, Err: fmt.Errorf("warm restore: %w", lerr)}
			}
			// Slot is now consumed; queue replenishment.
			wp.Refill(ctx)
			res := &Result{
				ID:          slot.ID,
				Machine:     m,
				SocketPath:  slot.SocketPath,
				WorkDir:     slot.WorkDir,
				Latency:     time.Since(start),
				PreloadCost: slot.SpawnCost,
				warmCmd:     slot.Cmd,
				jailer:      slot.jailer,
			}
			p.mu.Lock()
			p.live[slot.ID] = res
			p.mu.Unlock()
			return res
		}
		// Pool drained; fall through to cold path.
	}

	// Cold path. Two sub-variants: jailed (Request.Jailer != nil) and
	// legacy/unjailed (Jailer nil; default). The jailed variant is
	// opt-in in v0.2.1.
	if req.Jailer != nil {
		return p.forkOneJailed(ctx, req, id, start)
	}

	dir := filepath.Join(workRoot, "firefork-fork-"+id)
	if err := cliutil.MkPrivateDir(dir); err != nil {
		return &Result{ID: id, Err: fmt.Errorf("mkdir: %w", err)}
	}

	cfg := fc.Config{
		SocketPath:     filepath.Join(dir, "fc.sock"),
		FirecrackerBin: req.FirecrackerBin,
	}

	m, err := fc.Restore(ctx, cfg, req.Snapshot, fc.RestoreOptions{
		MemBackend:         fc.MemBackendFile,
		ResumeOnLoad:       true,
		CombinedLoadResume: req.Opts.CombinedLoadResume,
	})
	if err != nil {
		// failed Results weren't added to p.live, so
		// Shutdown never visited them and the per-fork workdir leaked
		// forever. Clean up inline before returning the error result.
		_ = os.RemoveAll(dir)
		return &Result{ID: id, Err: fmt.Errorf("restore: %w", err)}
	}

	res := &Result{
		ID:         id,
		Machine:    m,
		SocketPath: cfg.SocketPath,
		WorkDir:    dir,
		Latency:    time.Since(start),
	}
	p.mu.Lock()
	p.live[id] = res
	p.mu.Unlock()
	return res
}

// forkOneJailed is the cold-path variant that launches firecracker
// inside a per-fork jailer chroot. The caller's req.Jailer is used as
// a template (UID, GID, ChrootBaseDir, binaries); ID is overridden
// per fork.
func (p *Pool) forkOneJailed(ctx context.Context, req Request, id string, start time.Time) *Result {
	jcfg := *req.Jailer
	jcfg.ID = id

	layout := fc.DefaultChrootLayout()
	hostFiles := map[string]string{
		layout.MemFile:   req.Snapshot.MemFilePath,
		layout.StateFile: req.Snapshot.StatePath,
	}
	// Merge in any caller-supplied extras (typically the rootfs path
	// that pre-jailer snapshots embed by absolute host path).
	for k, v := range jcfg.ExtraHostFiles {
		hostFiles[k] = v
	}
	cmd, apiSock, err := fc.StartJailedFirecracker(ctx, jcfg, hostFiles, nil, nil)
	if err != nil {
		// chroot may have been partially populated before
		// StartJailedFirecracker returned an error. Best-effort clean.
		_ = fc.CleanupChroot(jcfg)
		return &Result{ID: id, Err: fmt.Errorf("StartJailedFirecracker: %w", err)}
	}

	insideSnap := fc.SnapshotPaths{
		MemFilePath: layout.MemFile,
		StatePath:   layout.StateFile,
	}
	m, err := fc.RestoreOnSocket(ctx, apiSock, insideSnap, fc.RestoreOptions{
		MemBackend:         fc.MemBackendFile,
		ResumeOnLoad:       true,
		CombinedLoadResume: req.Opts.CombinedLoadResume,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = fc.CleanupChroot(jcfg)
		return &Result{ID: id, WorkDir: jcfg.ChrootRoot(), Err: fmt.Errorf("jailed restore: %w", err)}
	}

	res := &Result{
		ID:         id,
		Machine:    m,
		SocketPath: apiSock,
		WorkDir:    jcfg.ChrootRoot(),
		Latency:    time.Since(start),
		warmCmd:    cmd,
		jailer:     &jcfg,
	}
	p.mu.Lock()
	p.live[id] = res
	p.mu.Unlock()
	return res
}

// Shutdown stops every live fork and removes their working directories.
// Best-effort: errors during teardown are returned only as a count.
// Also closes the attached WarmPool if any.
func (p *Pool) Shutdown() (stopped, failed int) {
	p.mu.Lock()
	snap := p.live
	p.live = map[string]*Result{}
	wp := p.warmPool
	p.warmPool = nil
	p.mu.Unlock()

	for _, r := range snap {
		switch {
		case r.warmCmd != nil && r.warmCmd.Process != nil:
			// Warm/ultra-warm/jailer path: SDK Machine is nil; kill
			// the subprocess directly.
			if err := r.warmCmd.Process.Kill(); err != nil {
				failed++
			} else {
				stopped++
			}
			_, _ = r.warmCmd.Process.Wait()
		case r.Machine != nil:
			if err := r.Machine.StopVMM(); err != nil {
				failed++
			} else {
				stopped++
			}
		}
		if r.jailer != nil {
			// Jailed cold path: tear down the per-fork chroot tree.
			// CleanupChroot handles a missing root gracefully.
			_ = fc.CleanupChroot(*r.jailer)
		} else {
			_ = os.RemoveAll(r.WorkDir)
		}
	}
	if wp != nil {
		wp.Close()
	}
	return
}

// Count returns the number of live forks.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.live)
}

// Live returns a snapshot of the currently-live forks keyed by ID.
// Safe for read-only inspection (the returned Result pointers are the
// same ones Fork handed back to callers; do not mutate them).
func (p *Pool) Live() map[string]*Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*Result, len(p.live))
	for k, v := range p.live {
		out[k] = v
	}
	return out
}

// Release terminates a single live fork by ID and removes its working
// directory. Returns (true, nil) when the fork existed and was torn down,
// (false, nil) when no live fork has that ID. A non-nil error signals
// that teardown started but at least one syscall (Kill/StopVMM/
// RemoveAll/CleanupChroot) failed; the fork is removed from the live
// map regardless to avoid double-tear-down.
func (p *Pool) Release(id string) (bool, error) {
	p.mu.Lock()
	r, ok := p.live[id]
	if ok {
		delete(p.live, id)
	}
	p.mu.Unlock()
	if !ok {
		return false, nil
	}

	var firstErr error
	switch {
	case r.warmCmd != nil && r.warmCmd.Process != nil:
		if err := r.warmCmd.Process.Kill(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("kill: %w", err)
		}
		_, _ = r.warmCmd.Process.Wait()
	case r.Machine != nil:
		if err := r.Machine.StopVMM(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("StopVMM: %w", err)
		}
	}
	if r.jailer != nil {
		if err := fc.CleanupChroot(*r.jailer); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("CleanupChroot: %w", err)
		}
	} else if r.WorkDir != "" {
		if err := os.RemoveAll(r.WorkDir); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("RemoveAll: %w", err)
		}
	}
	return true, firstErr
}
