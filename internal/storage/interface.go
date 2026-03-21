package storage

import (
	"context"
	"errors"
	"io"
)

// ErrKeyNotFound is returned by [Storage.Get] / [Storage.GetStream] /
// [Storage.Head] when the requested key does not exist in the
// underlying object store. Implementations may wrap it.
var ErrKeyNotFound = errors.New("storage: key not found")

// Storage is the slow-tier blob store fronted by the per-host caches.
type Storage interface {
	// Get reads the entire object into memory. Suitable for small
	// payloads (manifests, state files, < a few MiB). For larger
	// objects prefer GetStream.
	Get(ctx context.Context, key string) ([]byte, error)

	// GetStream returns a streaming reader for the object. Caller MUST
	// Close the returned ReadCloser.
	GetStream(ctx context.Context, key string) (io.ReadCloser, error)

	// GetRange streams a half-open byte range [offset, offset+length)
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Head returns the object's size in bytes without reading any of
	// the payload.
	Head(ctx context.Context, key string) (int64, error)

	// Put uploads a small in-memory payload as a single object.
	Put(ctx context.Context, key string, value []byte) error

	// PutStream uploads a streaming payload. Implementations should
	// use S3 multipart for large objects so the whole memfile never
	// needs to be resident in memory. size may be -1 if unknown
	// (the implementation will buffer chunks).
	PutStream(ctx context.Context, key string, r io.Reader, size int64) error
}
