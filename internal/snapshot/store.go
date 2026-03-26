package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/storage"
)

// Store wraps a [storage.Storage] backend with snapshot-aware
// operations: Save uploads a (memfile, state, manifest) triple under a
// consistent key scheme; Load downloads the same triple back to local
// files, optionally decompressing the memfile.
type Store struct {
	S storage.Storage

	// CompressMemfile is the default-on/off for new Save calls.
	CompressMemfile bool

	// CompressionLevel is the default zstd level for new Save calls.
	CompressionLevel int
}

// LocalPaths points to local files for a snapshot bundle. Used both
// as input to [Store.Save] and as the destination for [Store.Load].
type LocalPaths struct {
	MemFile string
	State   string
}

// SaveOptions adjusts how a snapshot is uploaded.
type SaveOptions struct {
	// Notes is a free-form description copied into the manifest.
	Notes string

	// KernelVersion is informational; copied into the manifest.
	KernelVersion string

	// CompressMemfile, when non-nil, overrides Store.CompressMemfile
	// for this Save. nil = inherit from the Store.
	CompressMemfile *bool

	// CompressionLevel, when > 0, overrides Store.CompressionLevel
	// for this Save. 0 = inherit from the Store.
	CompressionLevel int
}

// Save uploads the memfile, state file, and a generated manifest under
// `<name>/<version>/{memfile,state,manifest}`. Returns the manifest
// that was written (the same one stored remotely).
func (st *Store) Save(ctx context.Context, name, version string, vcpus int, memMiB int64, local LocalPaths, opt SaveOptions) (*Manifest, error) {
	if st.S == nil {
		return nil, errors.New("Store.Save: Storage backend is nil")
	}
	if name == "" || version == "" {
		return nil, errors.New("Store.Save: name and version required")
	}
	if local.MemFile == "" || local.State == "" {
		return nil, errors.New("Store.Save: LocalPaths.MemFile + State required")
	}

	// per-Save compression settings come from opt first,
	// falling back to the Store's defaults. Save never mutates the
	// Store.
	compressMemfile := st.CompressMemfile
	if opt.CompressMemfile != nil {
		compressMemfile = *opt.CompressMemfile
	}
	compressionLevel := st.CompressionLevel
	if opt.CompressionLevel > 0 {
		compressionLevel = opt.CompressionLevel
	}

	stateKey := StateKey(name, version)
	memKey := MemFileKey(name, version, compressMemfile)

	// 1. Upload state file. Stream from disk via PutStream + FileSize
	// so we don't load the whole file into RAM (today ~28 KB, but
	// differential snapshots / large device state could grow it).
	stateSize, err := FileSize(local.State)
	if err != nil {
		return nil, fmt.Errorf("state size: %w", err)
	}
	stateFile, err := os.Open(local.State)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	if err := st.S.PutStream(ctx, stateKey, stateFile, stateSize); err != nil {
		_ = stateFile.Close()
		return nil, fmt.Errorf("upload state: %w", err)
	}
	_ = stateFile.Close()
	stateSha, err := FileSha256(local.State)
	if err != nil {
		return nil, fmt.Errorf("state sha: %w", err)
	}

	// 2. Upload memfile — optionally zstd-streaming on the fly.
	memSrc, err := os.Open(local.MemFile)
	if err != nil {
		return nil, fmt.Errorf("open memfile: %w", err)
	}
	defer memSrc.Close()

	memSize, err := FileSize(local.MemFile)
	if err != nil {
		return nil, fmt.Errorf("memfile size: %w", err)
	}

	var (
		uploadReader io.Reader = memSrc
		uploadSize             = memSize
		compTag      string
		// .zst when compressing, the raw memfile otherwise). We hash
		// this path directly rather than recomputing compression.
		uploadedArtifactPath = local.MemFile
	)
	if compressMemfile {
		// We compress to a temp file first because (a) S3 multipart
		// compression SHA-256 in the manifest. The compression cost
		// is one full memfile read + one zstd encode (~230 ms / 256 MiB
		// at level 3) which is acceptable for the Save path.
		tmpDir, err := os.MkdirTemp("", "firefork-save-*")
		if err != nil {
			return nil, fmt.Errorf("mktmp: %w", err)
		}
		defer os.RemoveAll(tmpDir)
		compPath := filepath.Join(tmpDir, "mem.zst")
		if err := CompressFile(local.MemFile, compPath, compressionLevel); err != nil {
			return nil, fmt.Errorf("compress memfile: %w", err)
		}
		compFile, err := os.Open(compPath)
		if err != nil {
			return nil, fmt.Errorf("open compressed: %w", err)
		}
		defer compFile.Close()
		uploadReader = compFile
		uploadSize, _ = FileSize(compPath)
		compTag = "zstd"
		uploadedArtifactPath = compPath
	}
	if err := st.S.PutStream(ctx, memKey, uploadReader, uploadSize); err != nil {
		return nil, fmt.Errorf("upload memfile: %w", err)
	}
	// a: hash the *uploaded* artifact directly. klauspost
	// zstd is deterministic at fixed level so re-running CompressFile
	// but at the cost of another full encode for no information gain.
	memSha, err := FileSha256(uploadedArtifactPath)
	if err != nil {
		return nil, fmt.Errorf("memfile sha: %w", err)
	}

	// 3. Build + upload manifest.
	man := &Manifest{
		SchemaVersion:      CurrentManifestSchemaVersion,
		Name:               name,
		Version:            version,
		CreatedAt:          time.Now().UTC(),
		VCPUs:              vcpus,
		MemMiB:             memMiB,
		KernelVersion:      opt.KernelVersion,
		MemFileKey:         memKey,
		MemFileSize:        uploadSize,
		MemFileSha256:      memSha,
		MemFileCompression: compTag,
		StateKey:           stateKey,
		StateSize:          stateSize,
		StateSha256:        stateSha,
		Notes:              opt.Notes,
	}
	buf, err := man.Marshal()
	if err != nil {
		return nil, err
	}
	if err := st.S.Put(ctx, ManifestKey(name, version), buf); err != nil {
		return nil, fmt.Errorf("upload manifest: %w", err)
	}
	return man, nil
}

// LoadOptions controls Load behaviour.
type LoadOptions struct {
	// VerifySha256, when true, recomputes SHA-256 on downloaded files
	// and errors if they don't match the manifest. Adds ~one file
	// read per artefact.
	VerifySha256 bool
}

// Load downloads a snapshot bundle into dest. Returns the populated
// LocalPaths.
func (st *Store) Load(ctx context.Context, name, version, destDir string, opt LoadOptions) (LocalPaths, *Manifest, error) {
	if st.S == nil {
		return LocalPaths{}, nil, errors.New("Store.Load: Storage backend is nil")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return LocalPaths{}, nil, fmt.Errorf("mkdir destDir: %w", err)
	}

	// 1. Manifest.
	manBytes, err := st.S.Get(ctx, ManifestKey(name, version))
	if err != nil {
		return LocalPaths{}, nil, fmt.Errorf("get manifest: %w", err)
	}
	man, err := UnmarshalManifest(manBytes)
	if err != nil {
		return LocalPaths{}, nil, err
	}

	// 2. State file (small).
	stateBytes, err := st.S.Get(ctx, man.StateKey)
	if err != nil {
		return LocalPaths{}, nil, fmt.Errorf("get state: %w", err)
	}
	statePath := filepath.Join(destDir, "state.bin")
	if err := os.WriteFile(statePath, stateBytes, 0o644); err != nil {
		return LocalPaths{}, nil, fmt.Errorf("write state: %w", err)
	}

	// 3. Memfile — try parallel range download when possible.
	memDownloadPath := filepath.Join(destDir, filepath.Base(man.MemFileKey))
	if err := downloadMemfile(ctx, st.S, man.MemFileKey, memDownloadPath); err != nil {
		return LocalPaths{}, nil, fmt.Errorf("download memfile: %w", err)
	}

	// 4. Integrity check on the *uploaded* artifact, BEFORE decompress
	//    (b). The manifest's MemFileSha256 covers whichever
	//    file shape was uploaded — the .zst for compressed bundles,
	//    the raw memfile otherwise. Verifying before decompress means
	//    a tampered bucket can't drive zstd through a maliciously-
	//    crafted stream, and the existing decompress-bomb cap is a
	//    belt-and-suspenders extra rather than the sole defense.
	if opt.VerifySha256 {
		if man.MemFileSha256 == "" {
			return LocalPaths{}, nil, fmt.Errorf("manifest missing MemFileSha256; refusing to load unverifiable snapshot")
		}
		sha, err := FileSha256(memDownloadPath)
		if err != nil {
			return LocalPaths{}, nil, fmt.Errorf("hash memfile: %w", err)
		}
		if sha != man.MemFileSha256 {
			return LocalPaths{}, nil, fmt.Errorf("memfile sha mismatch: got %s want %s", sha, man.MemFileSha256)
		}
	}

	// 5. Decompress if needed — Firecracker can't mmap a zstd stream.
	memPath := memDownloadPath
	if man.MemFileCompression == "zstd" {
		memPath = filepath.Join(destDir, "memfile.bin")
		maxRaw := man.MemMiB*1024*1024 + DecompressSlackBytes
		if err := DecompressFileBounded(memDownloadPath, memPath, maxRaw); err != nil {
			return LocalPaths{}, nil, fmt.Errorf("decompress memfile: %w", err)
		}
		_ = os.Remove(memDownloadPath)
	}

	// 6. State sha — small file, hashed after the memfile so error
	//    ordering matches "biggest threat first".
	if opt.VerifySha256 {
		if sha, err := FileSha256(statePath); err != nil {
			return LocalPaths{}, nil, err
		} else if sha != man.StateSha256 {
			return LocalPaths{}, nil, fmt.Errorf("state sha mismatch: got %s want %s", sha, man.StateSha256)
		}
	}

	return LocalPaths{MemFile: memPath, State: statePath}, man, nil
}

// downloadMemfile prefers the parallel ranged downloader on an
// *S3Storage and falls back to a single stream on anything else.
func downloadMemfile(ctx context.Context, s storage.Storage, key, dstPath string) error {
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	if s3s, ok := s.(*storage.S3Storage); ok {
		_, err := s3s.DownloadParallel(ctx, key, dst)
		return err
	}
	// Generic single-stream fallback (mock + future backends).
	rc, err := s.GetStream(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(dst, rc)
	return err
}
