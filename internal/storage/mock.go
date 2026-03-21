package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// MockStorage is an in-memory [Storage] implementation with configurable
// per-Get latency. Used in unit tests to exercise snapshot-store
// behaviour without requiring a live S3 endpoint.
type MockStorage struct {
	latency time.Duration

	mu   sync.RWMutex
	data map[string][]byte

	gets        atomic.Int64
	puts        atomic.Int64
	bytesServed atomic.Int64
}

// NewMockStorage returns a MockStorage that simulates the given
// latency on each Get/GetStream/GetRange/Head. Pass 0 for "as fast as
// possible".
func NewMockStorage(latency time.Duration) *MockStorage {
	return &MockStorage{
		latency: latency,
		data:    make(map[string][]byte),
	}
}

func (m *MockStorage) wait(ctx context.Context) error {
	if m.latency > 0 {
		select {
		case <-time.After(m.latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	} else if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Get implements [Storage].
func (m *MockStorage) Get(ctx context.Context, key string) ([]byte, error) {
	m.gets.Add(1)
	if err := m.wait(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	v, ok := m.data[key]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mock get %q: %w", key, ErrKeyNotFound)
	}
	out := make([]byte, len(v))
	copy(out, v)
	m.bytesServed.Add(int64(len(out)))
	return out, nil
}

// GetStream implements [Storage].
func (m *MockStorage) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	buf, err := m.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf)), nil
}

// GetRange implements [Storage].
func (m *MockStorage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	buf, err := m.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > int64(len(buf)) {
		return nil, fmt.Errorf("mock get range %q: offset %d out of bounds [0, %d]", key, offset, len(buf))
	}
	end := offset + length
	if end > int64(len(buf)) {
		end = int64(len(buf))
	}
	return io.NopCloser(bytes.NewReader(buf[offset:end])), nil
}

// Head implements [Storage].
func (m *MockStorage) Head(ctx context.Context, key string) (int64, error) {
	if err := m.wait(ctx); err != nil {
		return 0, err
	}
	m.mu.RLock()
	v, ok := m.data[key]
	m.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("mock head %q: %w", key, ErrKeyNotFound)
	}
	return int64(len(v)), nil
}

// Put implements [Storage]. Seeds the store with a key/value.
func (m *MockStorage) Put(ctx context.Context, key string, value []byte) error {
	m.puts.Add(1)
	if err := m.wait(ctx); err != nil {
		return err
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	m.mu.Lock()
	m.data[key] = cp
	m.mu.Unlock()
	return nil
}

// PutStream implements [Storage] by buffering r into memory.
func (m *MockStorage) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	_ = size
	buf, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("mock put stream %q: %w", key, err)
	}
	return m.Put(ctx, key, buf)
}

// Stats returns counters for assertions in tests.
func (m *MockStorage) Stats() MockStats {
	return MockStats{
		Gets:        m.gets.Load(),
		Puts:        m.puts.Load(),
		BytesServed: m.bytesServed.Load(),
	}
}

// MockStats is a snapshot of MockStorage counters.
type MockStats struct {
	Gets        int64
	Puts        int64
	BytesServed int64
}
