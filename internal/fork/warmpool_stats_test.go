package fork

import (
	"runtime"
	"testing"
	"time"
)

// TestTakeStats_DrainBumps confirms that a Take call on an empty
// WarmPool increments the drain counter and leaves takes at zero.
func TestTakeStats_DrainBumps(t *testing.T) {
	p := &WarmPool{
		size:           1,
		firecrackerBin: "/nonexistent/firecracker",
	}
	// Pre-stamp a recent failure so Take's drain path does NOT kick a
	// Refill goroutine (which would otherwise race the test).
	p.lastRefillFailNs.Store(time.Now().UnixNano())

	slot, err := p.Take()
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if slot != nil {
		t.Fatalf("Take: expected nil slot on empty pool, got %+v", slot)
	}
	takes, drains := p.TakeStats()
	if takes != 0 || drains != 1 {
		t.Fatalf("TakeStats = (%d, %d), want (0, 1)", takes, drains)
	}
}

// TestTakeStats_HitBumps confirms a non-empty pool bumps takes, not
// drains, on a Take call.
func TestTakeStats_HitBumps(t *testing.T) {
	p := &WarmPool{size: 1, firecrackerBin: "/nonexistent/firecracker"}
	p.idle = append(p.idle, &WarmSlot{ID: "stub"})

	slot, err := p.Take()
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if slot == nil {
		t.Fatal("Take: expected slot, got nil")
	}
	takes, drains := p.TakeStats()
	if takes != 1 || drains != 0 {
		t.Fatalf("TakeStats = (%d, %d), want (1, 0)", takes, drains)
	}
}

// TestRefillBackoff_SkipsWithinWindow verifies :
// a drain-triggered Refill is skipped while the failure-backoff
// window is still open. Without backoff a stuck spawn (disk full,
// etc.) would have every Take fire another doomed spawnSlot in a
// tight loop.
func TestRefillBackoff_SkipsWithinWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		// firecrackerBin path checks differ; this test only needs to
		// observe the *absence* of Refill spawning so the gate is
		// platform-agnostic, but keep it focused on linux where the
		// firefork runtime ships.
		t.Skip("backoff test focused on linux")
	}
	p := &WarmPool{size: 1, firecrackerBin: "/nonexistent/firecracker"}
	p.lastRefillFailNs.Store(time.Now().UnixNano())

	// Hold a sentinel so we can check refillErrs didn't change after
	// the Take call. If Refill HAD run, spawnSlot would fail and bump
	// refillErrs.
	before := p.RefillErrors()
	_, _ = p.Take()
	time.Sleep(50 * time.Millisecond) // give a hypothetical Refill goroutine time to fail
	after := p.RefillErrors()

	if after != before {
		t.Fatalf("Refill ran during backoff window: refillErrs %d -> %d", before, after)
	}
	if d := p.drains.Load(); d != 1 {
		t.Fatalf("drains = %d, want 1 (drain accounting unaffected by backoff)", d)
	}
}

// TestRefillBackoff_FiresAfterWindow verifies the backoff EXPIRES:
// once refillBackoff has elapsed since the last failure, the next
// drain DOES kick Refill (counter climbs).
func TestRefillBackoff_FiresAfterWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backoff test focused on linux")
	}
	p := &WarmPool{size: 1, firecrackerBin: "/nonexistent/firecracker"}
	// Stamp a failure well outside the backoff window.
	p.lastRefillFailNs.Store(time.Now().Add(-2 * refillBackoff).UnixNano())

	before := p.RefillErrors()
	_, _ = p.Take()
	// Wait for the goroutine spawnSlot exec failure to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.RefillErrors() > before {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.RefillErrors(); got <= before {
		t.Fatalf("Refill did not fire after backoff window: refillErrs stuck at %d", got)
	}
}

