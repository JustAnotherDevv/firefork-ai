// branching pattern: spawn N forks of one template via firefork-server,
// run one tool call per fork in parallel, gather the results, and
// tear them all down.
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
	"sort"
	"sync"
	"time"
)

type forkInfo struct {
	ID          string `json:"id"`
	LatencyMs   int    `json:"latency_ms"`
	PreloadMs   int    `json:"preload_ms"`
	TemplateKey string `json:"template_key"`
	Err         string `json:"error,omitempty"`
}

type spawnResp struct {
	Forks []forkInfo `json:"forks"`
}

type execResp struct {
	ForkID    string         `json:"fork_id"`
	LatencyMs int            `json:"latency_ms"`
	Result    map[string]any `json:"result"`
}

type branch struct {
	forkID  string
	wallMs  int
	stdout  string
	exitErr error
}

func main() {
	var (
		base     = flag.String("base", "http://localhost:8080", "firefork-server base URL")
		tplKey   = flag.String("template", "python/v1", "<name>/<version>")
		branches = flag.Int("branches", 8, "number of parallel forks")
		cmd      = flag.String("cmd", `python3 -c "import random,os; print('branch result:', random.randint(0,1<<20))"`, "shell command each branch runs")
	)
	flag.Parse()

	token := os.Getenv("FIREFORK_AUTH_TOKEN")
	if token == "" {
		log.Println("[warn] FIREFORK_AUTH_TOKEN unset; server is in DEMO mode or you'll get 401")
	}
	c := &http.Client{Timeout: 60 * time.Second}

	ctx := context.Background()

	// 1. Spawn N forks. Server handles count up to 256.
	t0 := time.Now()
	var spawn spawnResp
	if err := post(ctx, c, *base+"/v1/fork", token, map[string]any{
		"template": *tplKey, "count": *branches,
	}, &spawn); err != nil {
		log.Fatalf("spawn: %v", err)
	}
	spawnWall := time.Since(t0)
	if len(spawn.Forks) != *branches {
		log.Fatalf("got %d forks, wanted %d", len(spawn.Forks), *branches)
	}
	fmt.Printf("spawned %d forks of %s in %v (wall)\n",
		*branches, *tplKey, spawnWall)

	// 2. Always clean up. Best-effort.
	defer func() {
		var wg sync.WaitGroup
		for _, f := range spawn.Forks {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				_ = del(ctx, c, *base+"/v1/forks/"+id, token)
			}(f.ID)
		}
		wg.Wait()
	}()

	// 3. Run the command in every fork concurrently.
	t1 := time.Now()
	results := make([]branch, *branches)
	var wg sync.WaitGroup
	for i, f := range spawn.Forks {
		if f.Err != "" {
			results[i] = branch{forkID: f.ID, exitErr: fmt.Errorf("spawn err: %s", f.Err)}
			continue
		}
		wg.Add(1)
		go func(i int, fid string) {
			defer wg.Done()
			start := time.Now()
			var resp execResp
			if err := post(ctx, c, *base+"/v1/exec", token, map[string]any{
				"fork_id": fid, "cmd": *cmd, "timeout_ms": 10000,
			}, &resp); err != nil {
				results[i] = branch{forkID: fid, exitErr: err}
				return
			}
			stdout, _ := resp.Result["stdout"].(string)
			results[i] = branch{
				forkID: fid,
				wallMs: int(time.Since(start).Milliseconds()),
				stdout: stdout,
			}
		}(i, f.ID)
	}
	wg.Wait()
	execWall := time.Since(t1)

	// 4. Report.
	fmt.Printf("\nall branches done in %v (wall)\n", execWall)
	fmt.Printf("breakdown:\n")
	sort.Slice(results, func(i, j int) bool { return results[i].wallMs < results[j].wallMs })
	for i, r := range results {
		if r.exitErr != nil {
			fmt.Printf("  [%2d] FAIL %s: %v\n", i, r.forkID[:8], r.exitErr)
			continue
		}
		fmt.Printf("  [%2d] %dms %s: %q\n", i, r.wallMs, r.forkID[:8],
			truncate(r.stdout, 60))
	}

	fmt.Printf("\ntotal: spawn=%v + exec=%v = %v\n",
		spawnWall, execWall, spawnWall+execWall)
}

func post(ctx context.Context, c *http.Client, url, token string, body, out any) error {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
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

func del(ctx context.Context, c *http.Client, url, token string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
