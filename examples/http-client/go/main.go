// Command http-client-go demonstrates driving firefork-server from a Go
// program over HTTP. Spawns one fork, runs a command inside it via
// /v1/exec, then deletes the fork.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type forkInfo struct {
	ID          string `json:"id"`
	VsockUDS    string `json:"vsock_uds"`
	LatencyMs   int    `json:"latency_ms"`
	PreloadMs   int    `json:"preload_ms"`
	TemplateKey string `json:"template_key"`
	Err         string `json:"error,omitempty"`
}

type forkResp struct {
	Forks []forkInfo `json:"forks"`
}

type execResp struct {
	ForkID    string         `json:"fork_id"`
	LatencyMs int            `json:"latency_ms"`
	Result    map[string]any `json:"result"`
}

type client struct {
	base  string
	token string
	http  *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func main() {
	var (
		base     = flag.String("base", "http://localhost:8080", "firefork-server base URL")
		template = flag.String("template", "python/v1", "<name>/<version>")
		cmdStr   = flag.String("cmd", "uname -a && id", "shell command to run in the fork")
	)
	flag.Parse()

	c := &client{
		base:  *base,
		token: os.Getenv("FIREFORK_AUTH_TOKEN"),
		http:  &http.Client{Timeout: 60 * time.Second},
	}
	if c.token == "" {
		log.Println("[warn] FIREFORK_AUTH_TOKEN unset; server is in DEMO mode or you'll get 401")
	}

	ctx := context.Background()

	// 1. Spawn one fork.
	var spawn forkResp
	if err := c.do(ctx, "POST", "/v1/fork", map[string]any{
		"template": *template, "count": 1,
	}, &spawn); err != nil {
		log.Fatalf("/v1/fork: %v", err)
	}
	if len(spawn.Forks) == 0 || spawn.Forks[0].Err != "" {
		log.Fatalf("fork failed: %+v", spawn)
	}
	f := spawn.Forks[0]
	fmt.Printf("forked %s id=%s latency=%dms\n", f.TemplateKey, f.ID, f.LatencyMs)

	// 2. Ensure we tear it down even on early exit.
	defer func() {
		if err := c.do(ctx, "DELETE", "/v1/forks/"+f.ID, nil, nil); err != nil {
			log.Printf("delete: %v", err)
		}
	}()

	// 3. Run a command inside it.
	var exec execResp
	if err := c.do(ctx, "POST", "/v1/exec", map[string]any{
		"fork_id": f.ID, "cmd": *cmdStr, "timeout_ms": 10000,
	}, &exec); err != nil {
		log.Fatalf("/v1/exec: %v", err)
	}
	fmt.Printf("exec latency=%dms result=%v\n", exec.LatencyMs, exec.Result)
}
