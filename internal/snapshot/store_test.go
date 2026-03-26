package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JustAnotherDevv/firefork-ai/internal/storage"
)

// TestStoreRoundtripMock verifies Save then Load via the in-memory
// MockStorage. Exercises both compressed and raw memfile paths.
func TestStoreRoundtripMock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		compress bool
	}{
		{"raw", false},
		{"zstd", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()

			// Synthesize a "memfile" + "state" pair.
			memPath := filepath.Join(dir, "mem.bin")
			statePath := filepath.Join(dir, "state.bin")
			memBytes := make([]byte, 2*1024*1024) // 2 MiB
			rand.Read(memBytes)
			if err := os.WriteFile(memPath, memBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			stateBytes := []byte("dummy-state-bytes-15k-equiv")
			if err := os.WriteFile(statePath, stateBytes, 0o644); err != nil {
				t.Fatal(err)
			}

			mock := storage.NewMockStorage(0)
			st := &Store{S: mock, CompressMemfile: tc.compress}

			man, err := st.Save(ctx, "test", "v1", 1, 64, LocalPaths{
				MemFile: memPath, State: statePath,
			}, SaveOptions{Notes: "unit"})
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if man.Name != "test" || man.Version != "v1" {
				t.Fatalf("manifest fields wrong: %+v", man)
			}
			if tc.compress && man.MemFileCompression != "zstd" {
				t.Fatalf("expected zstd compression tag")
			}

			destDir := filepath.Join(dir, "restored")
			locals, m2, err := st.Load(ctx, "test", "v1", destDir, LoadOptions{VerifySha256: true})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if m2.MemFileSha256 != man.MemFileSha256 {
				t.Fatalf("manifest sha mismatch")
			}

			// Round-trip the memfile: should equal the original.
			got, err := os.ReadFile(locals.MemFile)
			if err != nil {
				t.Fatalf("read restored memfile: %v", err)
			}
			if !bytes.Equal(got, memBytes) {
				t.Fatalf("memfile mismatch: got %d B want %d B", len(got), len(memBytes))
			}
			gotState, _ := os.ReadFile(locals.State)
			if !bytes.Equal(gotState, stateBytes) {
				t.Fatalf("state mismatch")
			}
		})
	}
}

// TestStoreLoadMissing verifies Load returns ErrKeyNotFound when the
// manifest is absent.
func TestStoreLoadMissing(t *testing.T) {
	ctx := context.Background()
	mock := storage.NewMockStorage(0)
	st := &Store{S: mock}
	_, _, err := st.Load(ctx, "ghost", "v1", t.TempDir(), LoadOptions{})
	if err == nil {
		t.Fatalf("expected error for missing manifest")
	}
	if !errors.Is(err, storage.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}
