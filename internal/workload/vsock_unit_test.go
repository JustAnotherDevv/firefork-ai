package workload

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeAgentServer stands up a UNIX-domain socket that mimics the
// Firecracker vsock proxy enough to exercise Call. handler runs once
// per incoming connection.
type fakeAgentServer struct {
	t       *testing.T
	path    string
	ln      net.Listener
	done    chan struct{}
	handler func(net.Conn)
}

func startFakeAgent(t *testing.T, handler func(net.Conn)) *fakeAgentServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets not supported by this test on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "vsock.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fs := &fakeAgentServer{t: t, path: path, ln: ln, done: make(chan struct{}), handler: handler}
	go fs.serve()
	t.Cleanup(fs.stop)
	return fs
}

func (fs *fakeAgentServer) serve() {
	defer close(fs.done)
	for {
		conn, err := fs.ln.Accept()
		if err != nil {
			return
		}
		go fs.handler(conn)
	}
}

func (fs *fakeAgentServer) stop() {
	_ = fs.ln.Close()
	<-fs.done
}

// TestCall_AcceptsNormalResponse exercises the happy path: ack + small
// JSON reply. Regression check that the LimitReader wrapping didn't
// break normal traffic.
func TestCall_AcceptsNormalResponse(t *testing.T) {
	fs := startFakeAgent(t, func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := br.ReadString('\n'); err != nil { // CONNECT line
			return
		}
		fmt.Fprintf(c, "OK 5\n")
		if _, err := br.ReadString('\n'); err != nil { // cmd JSON
			return
		}
		fmt.Fprintf(c, `{"ok":true,"text":"hi"}`+"\n")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := Call(ctx, fs.path, AgentPort, nil, map[string]any{"cmd": "echo", "text": "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, _ := resp["text"].(string); got != "hi" {
		t.Fatalf("text = %q, want %q", got, "hi")
	}
}

// TestCall_RejectsOversizedResponse drives an agent that emits a
// response far larger than MaxResponseBytes without a newline. Call
// must surface ErrResponseTooLarge, not OOM the test runner.
func TestCall_RejectsOversizedResponse(t *testing.T) {
	fs := startFakeAgent(t, func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		fmt.Fprintf(c, "OK 5\n")
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		// Spam bytes; deliberately no newline so the host has to
		// hit its byte cap to stop.
		spam := strings.Repeat("A", 1<<20) // 1 MiB chunk
		for i := 0; i < 20; i++ {          // 20 MiB total > 16 MiB cap
			if _, err := c.Write([]byte(spam)); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Call(ctx, fs.path, AgentPort, nil, map[string]any{"cmd": "echo"})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
}

// TestCall_SignsWithSecret confirms that when a secret is supplied,
// Call adds a hex HMAC-SHA256 tag under AuthField that the agent
// can verify by removing the tag and recomputing over canonical JSON.
func TestCall_SignsWithSecret(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxxx")
	got := make(chan map[string]any, 1)
	fs := startFakeAgent(t, func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n') // CONNECT
		fmt.Fprintf(c, "OK 5\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		var cmd map[string]any
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			return
		}
		got <- cmd
		fmt.Fprintf(c, `{"ok":true}`+"\n")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Call(ctx, fs.path, AgentPort, secret, map[string]any{
		"cmd": "echo", "text": "hi",
	}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	cmd := <-got
	tag, ok := cmd[AuthField].(string)
	if !ok || tag == "" {
		t.Fatalf("cmd missing %s field: %v", AuthField, cmd)
	}

	// Recompute expected tag and compare.
	delete(cmd, AuthField)
	canon, err := canonicalJSON(cmd)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(canon)
	want := hex.EncodeToString(mac.Sum(nil))
	if tag != want {
		t.Fatalf("auth tag mismatch: got %s want %s\ncanon=%s", tag, want, canon)
	}
}

// TestCanonicalJSON_SortsKeys is a determinism guard so the Go and
// Python HMAC inputs match byte-for-byte.
func TestCanonicalJSON_SortsKeys(t *testing.T) {
	out, err := canonicalJSON(map[string]any{
		"zebra": 1, "apple": "x", "mango": []any{"a", "b"},
	})
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	const want = `{"apple":"x","mango":["a","b"],"zebra":1}`
	if string(out) != want {
		t.Fatalf("canonicalJSON =\n  %s\nwant\n  %s", out, want)
	}
}

// 0005f: Go's default json.Marshal escapes `<`, `>`, `&` as \uXXXX
// for HTML safety; Python's json.dumps does not. The Go canonical
// MUST disable that escape so the HMACs match.
func TestCanonicalJSON_NoHTMLEscape(t *testing.T) {
	out, err := canonicalJSON(map[string]any{
		"cmd":  "exec",
		"argv": []any{"sh", "-c", "echo hi > /tmp/marker"},
	})
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	const want = `{"argv":["sh","-c","echo hi > /tmp/marker"],"cmd":"exec"}`
	if string(out) != want {
		t.Fatalf("canonicalJSON =\n  %s\nwant\n  %s", out, want)
	}
}

// TestCall_RejectsBadAck guards the ack-prefix check.
func TestCall_RejectsBadAck(t *testing.T) {
	fs := startFakeAgent(t, func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n')
		fmt.Fprintf(c, "REJECT\n")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := Call(ctx, fs.path, AgentPort, nil, map[string]any{"cmd": "echo"})
	if err == nil {
		t.Fatal("Call: want error on bad ack, got nil")
	}
	if !strings.Contains(err.Error(), "ack") {
		t.Fatalf("err = %v, want ack-related error", err)
	}
}

// TestParseAck covers the strict ack grammar. The bug
// and conflated "ERROR <code>" with generic protocol errors.
func TestParseAck(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"happy", "OK 5\n", false},
		{"happy_high_port", "OK 65535\n", false},
		{"happy_no_lf", "OK 1234", false},
		{"happy_crlf", "OK 5\r\n", false},
		{"OKwhatever_rejected", "OKwhatever\n", true},
		{"ok_lowercase_rejected", "ok 5\n", true},
		{"OK_no_port", "OK\n", true},
		{"OK_garbage_port", "OK notanint\n", true},
		{"ERROR_reply", "ERROR 12\n", true},
		{"empty", "", true},
		{"random", "BLAH 5\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := parseAck(c.in)
			if c.wantErr && err == nil {
				t.Fatalf("parseAck(%q): want err, got nil", c.in)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("parseAck(%q): unexpected err: %v", c.in, err)
			}
		})
	}
}

// TestCall_RejectsERROR confirms that the agent's "ERROR <code>"
// reply surfaces verbatim instead of being silently treated as a
// generic ack error.
func TestCall_RejectsERROR(t *testing.T) {
	fs := startFakeAgent(t, func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n')
		fmt.Fprintf(c, "ERROR 13\n")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := Call(ctx, fs.path, AgentPort, nil, map[string]any{"cmd": "echo"})
	if err == nil {
		t.Fatal("Call: want error on ERROR ack, got nil")
	}
	if !strings.Contains(err.Error(), "ERROR 13") {
		t.Fatalf("err = %v, want literal ERROR 13 in message", err)
	}
}
