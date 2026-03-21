package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMockRoundtrip(t *testing.T) {
	ctx := context.Background()
	m := NewMockStorage(0)
	payload := []byte("hello firefork")
	if err := m.Put(ctx, "k1", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := m.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get mismatch: got %q want %q", got, payload)
	}
	if n, err := m.Head(ctx, "k1"); err != nil || n != int64(len(payload)) {
		t.Fatalf("Head: n=%d err=%v", n, err)
	}
}

func TestMockNotFound(t *testing.T) {
	ctx := context.Background()
	m := NewMockStorage(0)
	if _, err := m.Get(ctx, "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get missing: want ErrKeyNotFound, got %v", err)
	}
	if _, err := m.Head(ctx, "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Head missing: want ErrKeyNotFound, got %v", err)
	}
}

func TestMockStream(t *testing.T) {
	ctx := context.Background()
	m := NewMockStorage(0)
	payload := bytes.Repeat([]byte("x"), 1024)
	if err := m.PutStream(ctx, "stream", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	rc, err := m.GetStream(ctx, "stream")
	if err != nil {
		t.Fatalf("GetStream: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("stream mismatch: %d vs %d bytes", len(got), len(payload))
	}
}

func TestMockGetRange(t *testing.T) {
	ctx := context.Background()
	m := NewMockStorage(0)
	payload := []byte("0123456789abcdef")
	if err := m.Put(ctx, "k", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := m.GetRange(ctx, "k", 4, 6)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "456789" {
		t.Fatalf("GetRange: got %q want %q", got, "456789")
	}
}
