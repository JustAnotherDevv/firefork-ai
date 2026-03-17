package fork

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepStale(t *testing.T) {
	root := t.TempDir()

	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if age > 0 {
			old := time.Now().Add(-age)
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	// Mix of stale, fresh, and unrelated directories.
	staleFork := mk("firefork-fork-aaa", 2*time.Hour)
	staleWarm := mk("firefork-warm-bbb", 2*time.Hour)
	staleBuild := mk("firefork-build-ccc", 2*time.Hour)
	freshFork := mk("firefork-fork-ddd", 5*time.Minute)
	unrelated := mk("not-firefork", 2*time.Hour)

	removed, err := SweepStale(root, 1*time.Hour)
	if err != nil {
		t.Fatalf("SweepStale: %v", err)
	}

	// Stale firefork dirs gone.
	for _, p := range []string{staleFork, staleWarm, staleBuild} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got %v", p, err)
		}
	}
	// Fresh + unrelated survive.
	for _, p := range []string{freshFork, unrelated} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s still present: %v", p, err)
		}
	}
	if len(removed) != 3 {
		t.Errorf("removed len=%d, want 3 (%v)", len(removed), removed)
	}
}

func TestSweepStale_ZeroAgeMatchesAll(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "firefork-fork-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := SweepStale(root, 0)
	if err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed=%v want 1", removed)
	}
}

func TestSweepStale_NoMatches(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "not-ours"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := SweepStale(root, 0)
	if err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed=%v want empty", removed)
	}
}
