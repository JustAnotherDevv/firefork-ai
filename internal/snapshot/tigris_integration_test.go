package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/storage"
)

// TestTigrisSaveLoad is an integration test against a real Tigris (or
// any S3-compatible) bucket. Skipped unless the endpoint + bucket +
// AWS creds are set. : env-var naming used to drift between
// TIGRIS_* (used by cmd/seed-template + .env.example) and
// FIREFORK_TIGRIS_* (only this test). Now both work — TIGRIS_* +
// AWS_ACCESS_KEY_ID is the canonical surface; FIREFORK_TIGRIS_* is
// kept as a fallback for any cached env on the build host.
func TestTigrisSaveLoad(t *testing.T) {
	endpoint := firstNonEmpty(os.Getenv("TIGRIS_ENDPOINT"), os.Getenv("FIREFORK_TIGRIS_ENDPOINT"))
	bucket := firstNonEmpty(os.Getenv("TIGRIS_BUCKET"), os.Getenv("FIREFORK_TIGRIS_BUCKET"))
	accessKey := firstNonEmpty(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("FIREFORK_TIGRIS_ACCESS_KEY"))
	secretKey := firstNonEmpty(os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("FIREFORK_TIGRIS_SECRET_KEY"))
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		t.Skip("Tigris env vars not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	s3, err := storage.NewS3Storage(ctx, storage.S3Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		Region:    "auto",
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}

	// Use a unique versioned prefix so parallel CI runs don't clash.
	name := "firefork-test"
	version := fmt.Sprintf("integ-%d", time.Now().UnixNano())

	dir := t.TempDir()
	memPath := filepath.Join(dir, "mem.bin")
	statePath := filepath.Join(dir, "state.bin")
	memBytes := make([]byte, 4*1024*1024) // 4 MiB so multipart kicks in for upload
	rand.Read(memBytes)
	if err := os.WriteFile(memPath, memBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("integration-state"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := &Store{S: s3, CompressMemfile: true, CompressionLevel: 3}

	saveStart := time.Now()
	man, err := st.Save(ctx, name, version, 1, 64, LocalPaths{
		MemFile: memPath, State: statePath,
	}, SaveOptions{Notes: "integration test"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	saveDur := time.Since(saveStart)
	t.Logf("Save: %v (uploaded mem %d B compressed -> remote %s)", saveDur, man.MemFileSize, man.MemFileKey)

	// Best-effort cleanup at the end.
	t.Cleanup(func() {
		// Manual key deletes via Storage interface aren't part of the
		// surface; the integration test leaks keys under
		// firefork-test/integ-* which is fine.
	})

	destDir := filepath.Join(dir, "restored")
	loadStart := time.Now()
	locals, m2, err := st.Load(ctx, name, version, destDir, LoadOptions{VerifySha256: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loadDur := time.Since(loadStart)
	t.Logf("Load: %v (parallel download + decompress)", loadDur)

	if m2.MemFileSize != man.MemFileSize || m2.MemFileSha256 != man.MemFileSha256 {
		t.Fatalf("manifest mismatch: %+v vs %+v", m2, man)
	}

	gotMem, err := os.ReadFile(locals.MemFile)
	if err != nil {
		t.Fatalf("read restored memfile: %v", err)
	}
	if !bytes.Equal(gotMem, memBytes) {
		t.Fatalf("memfile mismatch: got %d B want %d B", len(gotMem), len(memBytes))
	}
}

// firstNonEmpty returns the first non-empty value in candidates.
// Used for fallback chains.
func firstNonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}
