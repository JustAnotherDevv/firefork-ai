package fork

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/JustAnotherDevv/firefork-ai/internal/cliutil"
	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
)

// WarmSlot is a pre-spawned Firecracker process waiting for a snapshot
// to load. It owns its own work directory + API socket + subprocess.
type WarmSlot struct {
	ID         string
	SocketPath string
	WorkDir    string
	Cmd        *exec.Cmd
	createdAt  time.Time

	// SpawnCost is wall-clock time spent on this slot before it
	// became idle-ready: subprocess spawn + (for ultra-warm) the
	// PUT /snapshot/load. Honest-reporting input for
	// Pool.forkOne stamps it onto Result.PreloadCost so the CLI can
	// surface "1.2ms resume + 92ms preload" rather than hiding the
	// preload cost behind the warm-pool optimisation.
	SpawnCost time.Duration

	// Preloaded is set when this slot already finished snapshot/load
	// at spawn time. Fork = single PATCH /vm Resumed.
	Preloaded bool

	// jailer is non-nil when this slot was spawned via
	// StartJailedFirecracker. Close + Refill consult it to call
	// fc.CleanupChroot on the per-slot chroot tree.
	jailer *fc.JailerConfig
}

// WarmPool maintains N idle Firecracker processes ready to load a
// snapshot on demand. Each slot bypasses the subprocess spawn
// (~10-15 ms) and API server bootstrap that a cold Pool.Fork pays.
type WarmPool struct {
	size           int
	firecrackerBin string
	workRoot       string

	mu      sync.Mutex
	idle    []*WarmSlot
	closed  bool
	refills chan struct{}

	// preload, if non-nil, makes every slot (initial + refilled) run
	// fc.LoadOnSocket at spawn time so the snapshot is mmap'd and the
	// VM is paused. Fork then becomes one HTTP round-trip.
	preload *fc.SnapshotPaths

	// jailerTemplate, if non-nil, launches every slot via the jailer
	// Per-slot JailerConfig is cloned from this template
	// with ID overridden to the slot's UUID. ExtraHostFiles on the
	// template apply to every slot — typical use: hardlink the parent
	// snapshot's embedded rootfs path so LoadOnSocket can resolve it.
	jailerTemplate *fc.JailerConfig

	// refillErrs counts background Refill failures (spawn or preload).
	refillErrs atomic.Uint64

	// takes / drains are the warm-pool hit-rate counters. Every Take
	// call that hands back a slot bumps takes; every call that returns
	// (nil, nil) because idle was empty bumps drains. Hit rate =
	// takes / (takes + drains). cmd/fork prints this at end-of-run.
	takes  atomic.Uint64
	drains atomic.Uint64

	// lastRefillFailNs records the wall-clock time (Unix nanos) of the
	// most recent Refill failure. Drain-triggered refills consult it
	// to skip kicking a new spawn while we're inside the backoff
	// window; without this, a persistent spawn failure (e.g. disk
	// full) would have every cold-path fallback fire another spawnSlot
	// at the same broken state in a tight loop. 0 means "no failure yet".
	lastRefillFailNs atomic.Int64
}

// refillBackoff is the minimum gap between Refill failure and the
// next drain-triggered Refill. Conservative — drain-on-success still
// triggers immediately; only the failure-throttle uses this.
const refillBackoff = 500 * time.Millisecond

// RefillErrors returns the cumulative count of background Refill
// failures (spawn or preload). Wire this into Phase 9 metrics so a
// stuck pool is visible instead of silently shrinking.
func (p *WarmPool) RefillErrors() uint64 {
	return p.refillErrs.Load()
}

// TakeStats returns the lifetime warm-pool hit/miss counters
// takes = successful Take calls; drains = calls that
// returned (nil, nil) because idle was empty and the caller had to
// fall back to cold spawn.
func (p *WarmPool) TakeStats() (takes, drains uint64) {
	return p.takes.Load(), p.drains.Load()
}

// WarmPoolOpt is a functional option for NewWarmPool / NewUltraWarmPool.
type WarmPoolOpt func(*WarmPool)

// WithJailer makes every slot spawn under /usr/local/bin/jailer with
// the supplied JailerConfig as a template. Per-slot ID is overridden
// per slot; UID/GID/ChrootBaseDir/ExtraHostFiles are shared.
func WithJailer(jcfg fc.JailerConfig) WarmPoolOpt {
	return func(p *WarmPool) {
		jcfgCopy := jcfg
		p.jailerTemplate = &jcfgCopy
	}
}

// NewWarmPool spawns size idle Firecracker processes. Each is ready to
// have a snapshot loaded. firecrackerBin is the firecracker binary
// path; empty defaults to /usr/local/bin/firecracker.
func NewWarmPool(ctx context.Context, size int, firecrackerBin, workRoot string, opts ...WarmPoolOpt) (*WarmPool, error) {
	if size < 1 {
		return nil, fmt.Errorf("WarmPool size must be >= 1")
	}
	if firecrackerBin == "" {
		firecrackerBin = "/usr/local/bin/firecracker"
	}
	if workRoot == "" {
		workRoot = os.TempDir()
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return nil, fmt.Errorf("workRoot: %w", err)
	}

	p := &WarmPool{
		size:           size,
		firecrackerBin: firecrackerBin,
		workRoot:       workRoot,
		refills:        make(chan struct{}, size),
	}
	for _, opt := range opts {
		opt(p)
	}

	// Spawn all slots in parallel.
	type result struct {
		slot *WarmSlot
		err  error
	}
	resCh := make(chan result, size)
	for i := 0; i < size; i++ {
		go func() {
			slot, err := p.spawnSlot(ctx)
			resCh <- result{slot, err}
		}()
	}
	results := make([]result, 0, size)
	for i := 0; i < size; i++ {
		results = append(results, <-resCh)
	}

	var firstErr error
	var firstErrIdx int
	for i, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			firstErrIdx = i
		}
	}
	if firstErr != nil {
		for _, r := range results {
			if r.slot == nil {
				continue
			}
			_ = r.slot.Cmd.Process.Kill()
			if r.slot.jailer != nil {
				_ = fc.CleanupChroot(*r.slot.jailer)
			} else {
				_ = os.RemoveAll(r.slot.WorkDir)
			}
		}
		return nil, fmt.Errorf("spawnSlot[%d]: %w", firstErrIdx, firstErr)
	}

	// All N spawned successfully — publish under mu.
	p.mu.Lock()
	for _, r := range results {
		p.idle = append(p.idle, r.slot)
	}
	p.mu.Unlock()
	return p, nil
}

// NewUltraWarmPool is NewWarmPool + Preload in one call. After this
// returns, every slot is loaded with snap and paused; fork-time cost
// is a single PATCH /vm Resumed.
func NewUltraWarmPool(ctx context.Context, size int, firecrackerBin, workRoot string, snap fc.SnapshotPaths, opts ...WarmPoolOpt) (*WarmPool, error) {
	p, err := NewWarmPool(ctx, size, firecrackerBin, workRoot, opts...)
	if err != nil {
		return nil, err
	}
	if err := p.Preload(ctx, snap); err != nil {
		p.Close()
		return nil, fmt.Errorf("preload: %w", err)
	}
	return p, nil
}

// Preload runs fc.LoadOnSocket on every currently-idle slot in
// parallel and remembers snap so future Refills also preload.
// Returns the first error encountered.
func (p *WarmPool) Preload(ctx context.Context, snap fc.SnapshotPaths) error {
	if snap.MemFilePath == "" || snap.StatePath == "" {
		return fmt.Errorf("Preload: snapshot paths required")
	}
	p.mu.Lock()
	// re-Preloading with a *different* snapshot would
	// flagged Preloaded=true — next Take would resume into the wrong
	// snapshot. The pool is sized to a single template anyway; reject
	// the mismatch loudly rather than silently corrupting forks.
	if p.preload != nil &&
		(p.preload.MemFilePath != snap.MemFilePath || p.preload.StatePath != snap.StatePath) {
		p.mu.Unlock()
		return fmt.Errorf("Preload: pool already preloaded with %s; refusing to re-key to %s",
			p.preload.MemFilePath, snap.MemFilePath)
	}
	p.preload = &snap
	slots := append([]*WarmSlot(nil), p.idle...)
	p.mu.Unlock()

	type r struct{ err error }
	ch := make(chan r, len(slots))
	for _, s := range slots {
		s := s
		go func() {
			loadSnap, err := p.snapForSlot(s, snap)
			if err != nil {
				ch <- r{fmt.Errorf("prep slot %s: %w", s.ID, err)}
				return
			}
			preStart := time.Now()
			if err := fc.LoadOnSocket(ctx, s.SocketPath, loadSnap); err != nil {
				ch <- r{fmt.Errorf("load slot %s: %w", s.ID, err)}
				return
			}
			// Preloaded write goes under mu — Pool.forkOne reads it
			// from the other side of Take.
			p.mu.Lock()
			s.Preloaded = true
			s.SpawnCost += time.Since(preStart)
			p.mu.Unlock()
			ch <- r{nil}
		}()
	}
	var firstErr error
	for range slots {
		if x := <-ch; x.err != nil && firstErr == nil {
			firstErr = x.err
		}
	}
	return firstErr
}

// PrepareSnapForJailedSlot is the exported form of snapForSlot for
// callers driving a warm slot from outside the pool (e.g. Pool.forkOne
// taking a non-preloaded jailed slot). Returns chroot-relative paths
// after hardlinking the host snapshot files into the slot's chroot.
func (p *WarmPool) PrepareSnapForJailedSlot(slot *WarmSlot, hostSnap fc.SnapshotPaths) (fc.SnapshotPaths, error) {
	return p.snapForSlot(slot, hostSnap)
}

// snapForSlot returns the SnapshotPaths to pass to LoadOnSocket for a
// given slot. Non-jailed slots pass through unchanged. Jailed slots
// get the snap files hardlinked into their chroot at canonical
// locations (DefaultChrootLayout), and the returned paths are the
// inside-chroot views.
func (p *WarmPool) snapForSlot(slot *WarmSlot, hostSnap fc.SnapshotPaths) (fc.SnapshotPaths, error) {
	if slot.jailer == nil {
		return hostSnap, nil
	}
	layout := fc.DefaultChrootLayout()
	hostFiles := map[string]string{
		layout.MemFile:   hostSnap.MemFilePath,
		layout.StateFile: hostSnap.StatePath,
	}
	if _, err := fc.PrepareChroot(*slot.jailer, hostFiles); err != nil {
		return fc.SnapshotPaths{}, err
	}
	return fc.SnapshotPaths{
		MemFilePath: layout.MemFile,
		StatePath:   layout.StateFile,
	}, nil
}

// IsPreloaded reports whether this pool is in ultra-warm mode (slots
// have a snapshot loaded + paused). Callers use this to decide
// between RestoreOnSocket (load+resume) and ResumeOnSocket
// (resume-only) fork paths.
func (p *WarmPool) IsPreloaded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preload != nil
}

// spawnSlot starts one firecracker process bound to a unique API
// socket. The process blocks waiting for HTTP requests; we hand the
// socket path to the caller who will then drive snapshot/load.
func (p *WarmPool) spawnSlot(ctx context.Context) (*WarmSlot, error) {
	id := uuid.NewString()

	if p.jailerTemplate != nil {
		return p.spawnJailedSlot(ctx, id)
	}

	spawnStart := time.Now()

	dir := filepath.Join(p.workRoot, "firefork-warm-"+id)
	if err := cliutil.MkPrivateDir(dir); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "fc.sock")

	// context.Background() is intentional here. The
	// subprocess must outlive any single spawn ctx (e.g. when this
	// runs inside Refill triggered by a Take whose ctx is about to
	// be consumed by the cold-path fallback). The pool reaps the
	// subprocess on Close + Refill error paths instead.
	cmd := exec.CommandContext(context.Background(), p.firecrackerBin, "--api-sock", sock)
	// Pipe stdout/stderr to per-slot log files so we can debug
	// crashes without polluting the test output.
	logf, _ := os.Create(filepath.Join(dir, "firecracker.log"))
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	// Wait for the API socket to appear (Firecracker writes it ~ms
	// after launch). Cap at 5 s. : poll respects ctx — a
	// caller cancelling mid-spawn no longer blocks for the full 5 s.
	deadline := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return &WarmSlot{
				ID: id, SocketPath: sock, WorkDir: dir, Cmd: cmd,
				createdAt: time.Now(),
				SpawnCost: time.Since(spawnStart),
			}, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("spawnSlot: %w", ctx.Err())
		}
	}
	_ = cmd.Process.Kill()
	_ = os.RemoveAll(dir)
	return nil, fmt.Errorf("API socket %s did not appear within 5s", sock)
}

// spawnJailedSlot is spawnSlot's jailer-aware variant. Each slot gets
// its own chroot under jailerTemplate.ChrootBaseDir/firecracker/<id>/root.
func (p *WarmPool) spawnJailedSlot(ctx context.Context, id string) (*WarmSlot, error) {
	spawnStart := time.Now()

	jcfg := *p.jailerTemplate
	jcfg.ID = id

	logf, _ := os.Create(filepath.Join(p.workRoot, "firefork-warm-"+id+".log"))
	cmd, apiSock, err := fc.StartJailedFirecracker(ctx, jcfg, jcfg.ExtraHostFiles, logf, logf)
	if err != nil {
		return nil, fmt.Errorf("StartJailedFirecracker: %w", err)
	}
	return &WarmSlot{
		ID:         id,
		SocketPath: apiSock,
		WorkDir:    jcfg.ChrootRoot(),
		Cmd:        cmd,
		createdAt:  time.Now(),
		SpawnCost:  time.Since(spawnStart),
		jailer:     &jcfg,
	}, nil
}

// Take pops one warm slot from the pool. The caller now owns the slot
// — they must drive snapshot/load (or just resume if slot.Preloaded)
// via slot.SocketPath, and on completion either keep the slot as
// their live VM or clean up via kill+rm.
func (p *WarmPool) Take() (*WarmSlot, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("WarmPool closed")
	}
	if len(p.idle) == 0 {
		p.mu.Unlock()
		p.drains.Add(1)
		// failure — see refillBackoff doc on the WarmPool struct.
		if last := p.lastRefillFailNs.Load(); last == 0 ||
			time.Since(time.Unix(0, last)) >= refillBackoff {
			// Bound to context.Background() because the immediate
			// caller's ctx is about to be consumed by the cold-path
			// fork — we don't want to cancel the replacement slot's
			// spawn alongside it.
			p.Refill(context.Background())
		}
		return nil, nil
	}
	slot := p.idle[len(p.idle)-1]
	p.idle = p.idle[:len(p.idle)-1]
	p.mu.Unlock()
	p.takes.Add(1)
	return slot, nil
}

// Refill spawns one new slot in the background to replace a consumed
// before going idle. Best-effort; logs are written to per-slot files.
func (p *WarmPool) Refill(ctx context.Context) {
	go func() {
		slot, err := p.spawnSlot(ctx)
		if err != nil {
			// don't silently swallow — observable counter.
			p.refillErrs.Add(1)
			p.lastRefillFailNs.Store(time.Now().UnixNano())
			return
		}
		p.mu.Lock()
		snap := p.preload
		closed := p.closed
		p.mu.Unlock()
		if closed {
			_ = slot.Cmd.Process.Kill()
			if slot.jailer != nil {
				_ = fc.CleanupChroot(*slot.jailer)
			} else {
				_ = os.RemoveAll(slot.WorkDir)
			}
			return
		}
		if snap != nil {
			loadSnap, err := p.snapForSlot(slot, *snap)
			if err != nil {
				p.refillErrs.Add(1)
				p.lastRefillFailNs.Store(time.Now().UnixNano())
				_ = slot.Cmd.Process.Kill()
				if slot.jailer != nil {
					_ = fc.CleanupChroot(*slot.jailer)
				} else {
					_ = os.RemoveAll(slot.WorkDir)
				}
				return
			}
			preStart := time.Now()
			if err := fc.LoadOnSocket(ctx, slot.SocketPath, loadSnap); err != nil {
				p.refillErrs.Add(1)
				p.lastRefillFailNs.Store(time.Now().UnixNano())
				_ = slot.Cmd.Process.Kill()
				if slot.jailer != nil {
					_ = fc.CleanupChroot(*slot.jailer)
				} else {
					_ = os.RemoveAll(slot.WorkDir)
				}
				return
			}
			// Same race-safe write pattern as Preload.
			p.mu.Lock()
			slot.Preloaded = true
			slot.SpawnCost += time.Since(preStart)
			p.mu.Unlock()
		}
		p.mu.Lock()
		if !p.closed {
			p.idle = append(p.idle, slot)
		} else {
			_ = slot.Cmd.Process.Kill()
			if slot.jailer != nil {
				_ = fc.CleanupChroot(*slot.jailer)
			} else {
				_ = os.RemoveAll(slot.WorkDir)
			}
		}
		p.mu.Unlock()
	}()
}

// IdleCount returns the current number of warm slots available.
func (p *WarmPool) IdleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// Close kills any remaining idle slots and prevents further Take/Refill.
func (p *WarmPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, s := range p.idle {
		_ = s.Cmd.Process.Kill()
		if s.jailer != nil {
			_ = fc.CleanupChroot(*s.jailer)
		} else {
			_ = os.RemoveAll(s.WorkDir)
		}
	}
	p.idle = nil
}
