package fork

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SweepStale removes leftover per-fork and per-warm-slot directories
// older than maxAge under root (typically os.TempDir). Returns the
// list of paths removed for logging.
func SweepStale(root string, maxAge time.Duration) (removed []string, err error) {
	if root == "" {
		root = os.TempDir()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("sweep read %s: %w", root, err)
	}
	cutoff := time.Now().Add(-maxAge)
	var errs []string
	for _, e := range entries {
		name := e.Name()
		if !isFireforkStaleDir(name) {
			continue
		}
		if maxAge > 0 {
			info, statErr := e.Info()
			if statErr != nil {
				errs = append(errs, fmt.Sprintf("%s: stat: %v", name, statErr))
				continue
			}
			if info.ModTime().After(cutoff) {
				continue // still fresh — leave it
			}
		}
		full := filepath.Join(root, name)
		if rmErr := os.RemoveAll(full); rmErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, rmErr))
			continue
		}
		removed = append(removed, full)
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("sweep: %d removal errors: %s", len(errs), strings.Join(errs, "; "))
	}
	return removed, nil
}

// isFireforkStaleDir reports whether a basename matches one of the
// firefork temp-directory naming conventions. Kept in lockstep with
// the prefixes used by Pool.forkOne and WarmPool.spawnSlot.
func isFireforkStaleDir(name string) bool {
	switch {
	case strings.HasPrefix(name, "firefork-fork-"),
		strings.HasPrefix(name, "firefork-warm-"),
		strings.HasPrefix(name, "firefork-build-"):
		return true
	}
	return false
}
