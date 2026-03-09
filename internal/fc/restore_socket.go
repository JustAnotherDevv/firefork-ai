package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// RestoreOnSocket drives snapshot/load on an EXISTING firecracker
// API socket (the warm-pool path). This bypasses fc.Restore's
// subprocess spawn entirely — the firecracker process already exists,
func RestoreOnSocket(ctx context.Context, socketPath string, p SnapshotPaths, opts RestoreOptions) (*Machine, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("RestoreOnSocket: socketPath required")
	}

	// drop the dead `if !x { x = true }` default-flip;
	// honour opts.ResumeOnLoad as-given. All in-tree callers set true
	// explicitly so behaviour is unchanged at the call sites.
	client := unixSocketClient(socketPath)

	loadBody := map[string]any{
		"snapshot_path":         p.StatePath,
		"mem_backend":           map[string]string{"backend_type": "File", "backend_path": p.MemFilePath},
		"enable_diff_snapshots": false,
	}
	if opts.CombinedLoadResume {
		loadBody["resume_vm"] = true
	}
	if err := apiPut(ctx, client, "http://unix/snapshot/load", loadBody); err != nil {
		return nil, fmt.Errorf("snapshot/load: %w", err)
	}

	if opts.ResumeOnLoad && !opts.CombinedLoadResume {
		if err := apiPatch(ctx, client, "http://unix/vm", map[string]any{"state": "Resumed"}); err != nil {
			return nil, fmt.Errorf("PATCH /vm: %w", err)
		}
	}

	// We don't have an sdk Machine here (the SDK wants to own the
	// subprocess). Return a Machine wrapper with cfg.SocketPath only;
	// later StopVMM uses HTTP API rather than the SDK's process kill.
	return &Machine{
		cfg: Config{SocketPath: socketPath},
		m:   nil, // SDK Machine not used in warm path
	}, nil
}

// LoadOnSocket loads a snapshot on an existing firecracker API socket
// WITHOUT resuming. After this returns, the VM is loaded and paused
// ready for a single PATCH /vm Resumed to bring it live.
func LoadOnSocket(ctx context.Context, socketPath string, p SnapshotPaths) error {
	if socketPath == "" {
		return fmt.Errorf("LoadOnSocket: socketPath required")
	}
	if p.MemFilePath == "" || p.StatePath == "" {
		return fmt.Errorf("LoadOnSocket: snapshot paths required")
	}
	client := unixSocketClient(socketPath)
	return apiPut(ctx, client, "http://unix/snapshot/load", map[string]any{
		"snapshot_path":         p.StatePath,
		"mem_backend":           map[string]string{"backend_type": "File", "backend_path": p.MemFilePath},
		"enable_diff_snapshots": false,
	})
}

// ResumeOnSocket sends a single PATCH /vm Resumed to a paused VM on
// the given socket. This is the entire fork-time cost for an
// ultra-warm slot (Tier A1) — typically ~1-2 ms.
func ResumeOnSocket(ctx context.Context, socketPath string) (*Machine, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("ResumeOnSocket: socketPath required")
	}
	client := unixSocketClient(socketPath)
	if err := apiPatch(ctx, client, "http://unix/vm", map[string]any{"state": "Resumed"}); err != nil {
		return nil, fmt.Errorf("PATCH /vm: %w", err)
	}
	return &Machine{cfg: Config{SocketPath: socketPath}, m: nil}, nil
}

// unixSocketClient returns an http.Client that tunnels every request
// over the given UNIX-domain socket. Used by every *OnSocket call.
func unixSocketClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
}

// apiPut issues a PUT to the firecracker API socket with JSON body.
func apiPut(ctx context.Context, c *http.Client, url string, body any) error {
	return apiCall(ctx, c, http.MethodPut, url, body)
}

// apiPatch issues a PATCH.
func apiPatch(ctx context.Context, c *http.Client, url string, body any) error {
	return apiCall(ctx, c, http.MethodPatch, url, body)
}

func apiCall(ctx context.Context, c *http.Client, method, url string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(resp.Body)
		return fmt.Errorf("%s %s -> %d: %s", method, url, resp.StatusCode, msg.String())
	}
	return nil
}
