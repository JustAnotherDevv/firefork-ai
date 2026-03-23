package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/cliutil"
)

// MaxRegistryFileBytes caps how much of the on-disk registry JSON we
// extending Notes to 100 MiB) would otherwise bloat every OpenRegistry
// call. Typical legitimate registries are < 100 KiB.
const MaxRegistryFileBytes = 1 << 20 // 1 MiB

// MaxNotesBytes caps the per-entry Notes field length. Keeps the
// registry compact and bounds memory blast per template.
const MaxNotesBytes = 4096

// Registry is a JSON file mapping `name/version` → registry entry.
// It is the host-side record of which templates have been built and
// where their local snapshot files (and optionally remote manifest)
// live. The orchestrator + forking layer consult Registry to find a
// template at fork time.
type Registry struct {
	path string

	mu      sync.Mutex
	entries map[string]*Entry
}

// Entry is one row in the registry.
type Entry struct {
	Name           string    `json:"name"`
	Version        string    `json:"version"`
	VCPUs          int       `json:"vcpus"`
	MemMiB         int64     `json:"mem_mib"`
	LocalMemFile   string    `json:"local_mem_file,omitempty"`
	LocalStateFile string    `json:"local_state_file,omitempty"`
	ManifestKey    string    `json:"manifest_key,omitempty"`
	RemoteBucket   string    `json:"remote_bucket,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Notes          string    `json:"notes,omitempty"`

	// AgentSecretHex is the hex-encoded HMAC shared secret the
	// in-guest agent generated at build time. Required by every
	// non-ping workload.Call against forks of this snapshot.
	// Empty on legacy templates / unsigned-mode builds.
	AgentSecretHex string `json:"agent_secret_hex,omitempty"`
}

// Key returns the canonical lookup key for an entry.
func (e *Entry) Key() string { return e.Name + "/" + e.Version }

// OpenRegistry loads (or creates) a registry at path.
func OpenRegistry(path string) (*Registry, error) {
	if path == "" {
		return nil, errors.New("OpenRegistry: path required")
	}
	if err := cliutil.MkPrivateDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("registry mkdir: %w", err)
	}
	r := &Registry{path: path, entries: map[string]*Entry{}}
	// bounded read so an attacker (or just a corrupted
	// file) can't make every OpenRegistry call eat unbounded RAM.
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("registry open: %w", err)
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: MaxRegistryFileBytes + 1}
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("registry read: %w", err)
	}
	if int64(len(b)) > MaxRegistryFileBytes {
		return nil, fmt.Errorf("registry %s exceeds %d bytes; refuse to load", path, MaxRegistryFileBytes)
	}
	if len(b) == 0 {
		return r, nil
	}
	var doc struct {
		Entries map[string]*Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("registry parse: %w", err)
	}
	if doc.Entries != nil {
		r.entries = doc.Entries
	}
	return r, nil
}

// Put adds or replaces an entry by its (Name, Version) key.
func (r *Registry) Put(e *Entry) error {
	if e == nil || e.Name == "" || e.Version == "" {
		return errors.New("registry: Entry needs Name+Version")
	}
	if len(e.Notes) > MaxNotesBytes {
		e.Notes = e.Notes[:MaxNotesBytes]
	}
	r.mu.Lock()
	r.entries[e.Key()] = e
	r.mu.Unlock()
	return r.persist()
}

// Get returns the entry for (name, version), or nil if absent.
func (r *Registry) Get(name, version string) *Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[name+"/"+version]
}

// List returns all entries sorted by name then version.
func (r *Registry) List() []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// Delete removes an entry. Returns true if present.
func (r *Registry) Delete(name, version string) (bool, error) {
	r.mu.Lock()
	key := name + "/" + version
	_, ok := r.entries[key]
	if ok {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, r.persist()
}

// persist writes the registry to disk atomically (write to tmp, then
// rename). Must be called with the mutex held by caller? It re-locks
// internally — keep callers lock-free.
func (r *Registry) persist() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	doc := struct {
		Entries map[string]*Entry `json:"entries"`
	}{Entries: r.entries}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("registry write tmp: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("registry rename: %w", err)
	}
	// Belt-and-suspenders: chmod after rename in case the tmp had
	// the wrong mode (e.g. previous file's perms inherited).
	if err := os.Chmod(r.path, 0o600); err != nil {
		return fmt.Errorf("registry chmod: %w", err)
	}
	return nil
}

// Path returns the on-disk registry path.
func (r *Registry) Path() string { return r.path }
