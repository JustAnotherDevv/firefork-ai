package snapshot

import (
	"container/list"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/JustAnotherDevv/firefork-ai/internal/cliutil"
)

// ErrInvalidCacheName is returned by Path/Get/Insert when name or
// version would let a caller escape Root. The original
// code did a bare filepath.Join, so Path("../../etc", "v") could drive
// RemoveAll at /etc/v during eviction.
var ErrInvalidCacheName = errors.New("snapshot.LocalCache: name/version must not contain path separators, '..', or control chars")

// validCacheComponent rejects path-traversal characters in cache name
// and version components. Mirrors the constraints in template.ParseKey
// but kept local to avoid an import cycle.
func validCacheComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.HasPrefix(s, ".") {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00\n\r\t") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}

// LocalCache is a bounded-size LRU directory cache for snapshot
// bundles. Each entry is a directory containing the downloaded
// (memfile, state, manifest) for one (Name, Version) tuple.
type LocalCache struct {
	Root     string
	MaxBytes int64

	mu       sync.Mutex
	bytes    int64
	order    *list.List // front = most recent
	byKey    map[string]*list.Element
}

type entry struct {
	key  string
	path string
	size int64
}

// NewLocalCache creates an empty cache rooted at root. The directory
// is created if missing.
func NewLocalCache(root string, maxBytes int64) (*LocalCache, error) {
	if root == "" {
		return nil, errors.New("LocalCache: root required")
	}
	if maxBytes <= 0 {
		return nil, errors.New("LocalCache: MaxBytes must be > 0")
	}
	if err := cliutil.MkPrivateDir(root); err != nil {
		return nil, fmt.Errorf("LocalCache mkdir: %w", err)
	}
	return &LocalCache{
		Root:     root,
		MaxBytes: maxBytes,
		order:    list.New(),
		byKey:    map[string]*list.Element{},
	}, nil
}

// CacheKey is the cache lookup key for a snapshot.
func CacheKey(name, version string) string { return name + "/" + version }

// Path returns the directory path where a bundle would live in the
// cache. Existence not guaranteed — call Get to look up. Returns an
// empty string (and never panics) on invalid name/version.
func (c *LocalCache) Path(name, version string) string {
	if !validCacheComponent(name) || !validCacheComponent(version) {
		return ""
	}
	return filepath.Join(c.Root, name, version)
}

// Get returns the local directory for (name, version) if present, or
// "" if not. Bumps recency.
func (c *LocalCache) Get(name, version string) string {
	if !validCacheComponent(name) || !validCacheComponent(version) {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[CacheKey(name, version)]
	if !ok {
		return ""
	}
	c.order.MoveToFront(el)
	return el.Value.(*entry).path
}

// Insert records that a directory has been populated for (name,
// version). The on-disk size is measured by walking the directory so
// the cache's accounting can't drift from filesystem reality.
func (c *LocalCache) Insert(name, version string) error {
	if !validCacheComponent(name) || !validCacheComponent(version) {
		return ErrInvalidCacheName
	}
	path := c.Path(name, version)
	size, err := DirSize(path)
	if err != nil {
		return fmt.Errorf("LocalCache: measure %s: %w", path, err)
	}
	if size > c.MaxBytes {
		return fmt.Errorf("LocalCache: entry %d B exceeds cache budget %d B", size, c.MaxBytes)
	}
	key := CacheKey(name, version)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Replace if already present.
	if el, ok := c.byKey[key]; ok {
		c.bytes -= el.Value.(*entry).size
		c.bytes += size
		el.Value.(*entry).size = size
		c.order.MoveToFront(el)
		c.evictLocked()
		return nil
	}

	for c.bytes+size > c.MaxBytes && c.order.Len() > 0 {
		c.evictOneLocked()
	}
	e := &entry{key: key, path: path, size: size}
	el := c.order.PushFront(e)
	c.byKey[key] = el
	c.bytes += size
	return nil
}

// Bytes returns the current resident size of the cache.
func (c *LocalCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Len returns the number of entries currently cached.
func (c *LocalCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// evictLocked drops LRU entries until bytes <= MaxBytes. Mutex held.
func (c *LocalCache) evictLocked() {
	for c.bytes > c.MaxBytes && c.order.Len() > 0 {
		c.evictOneLocked()
	}
}

func (c *LocalCache) evictOneLocked() {
	tail := c.order.Back()
	if tail == nil {
		return
	}
	e := tail.Value.(*entry)
	c.order.Remove(tail)
	delete(c.byKey, e.key)
	c.bytes -= e.size
	_ = os.RemoveAll(e.path)
}

// DirSize sums the sizes of all regular files under path. Used after
// downloading a snapshot bundle to record its size for the LRU.
func DirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
