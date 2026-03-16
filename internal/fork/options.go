package fork

import "fmt"

// Optimizations configures the fork hot path. The zero value matches
// the conservative Phase 4 default behaviour — every optimization is
// off, so behaviour is identical to v0.1.x. Flip individual fields to
// opt in.
type Optimizations struct {
	// WarmPoolSize, when > 0, keeps a pool of pre-spawned Firecracker
	// processes ready to receive a snapshot/load call. Skips the
	// ~10-15 ms subprocess spawn on every Fork.
	WarmPoolSize int

	// UltraWarmPool, when true, preloads the snapshot into every warm
	// slot at spawn time. Fork() becomes a single PATCH /vm Resumed
	// call. Requires WarmPoolSize > 0.
	UltraWarmPool bool

	// CombinedLoadResume asks Firecracker to atomically load the
	// snapshot and resume in a single /snapshot/load call
	// (resume_vm: true). Mutually exclusive with UltraWarmPool (where
	// the snapshot was already loaded at preload time).
	CombinedLoadResume bool
}

// DefaultOptimizations is the conservative v0.1 baseline: every
// optimization off, behaviour identical to the Phase 4 acceptance tests.
func DefaultOptimizations() Optimizations { return Optimizations{} }

// AggressiveOptimizations enables everything safe for fork-latency
// benchmarks: warm pool + combined load/resume.
func AggressiveOptimizations(warmPoolSize int) Optimizations {
	return Optimizations{
		WarmPoolSize:       warmPoolSize,
		CombinedLoadResume: true,
	}
}

// UltraOptimizations is the lowest-latency configuration: warm pool
// of warmPoolSize slots, all preloaded with the parent snapshot. Fork
// time collapses to one PATCH /vm Resumed call.
func UltraOptimizations(warmPoolSize int) Optimizations {
	return Optimizations{
		WarmPoolSize:  warmPoolSize,
		UltraWarmPool: true,
	}
}

// Validate checks the option set for combinations that don't make
// sense.
func (o Optimizations) Validate() error {
	if o.WarmPoolSize < 0 {
		return fmt.Errorf("WarmPoolSize must be >= 0, got %d", o.WarmPoolSize)
	}
	if o.UltraWarmPool && o.WarmPoolSize <= 0 {
		return fmt.Errorf("UltraWarmPool requires WarmPoolSize > 0")
	}
	if o.UltraWarmPool && o.CombinedLoadResume {
		return fmt.Errorf("UltraWarmPool and CombinedLoadResume are mutually exclusive (load already happened at preload)")
	}
	return nil
}
