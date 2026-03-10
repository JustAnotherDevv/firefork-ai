package workload

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AgentPort is the TCP-style port number our in-guest agent listens
// on. Firecracker maps vsock connections to a UNIX-domain socket on
// the host: "<vsockUDS>_<port>".
const AgentPort = 1234

// MaxResponseBytes caps the JSON response the agent may return on a
// single Call. A misbehaving — or attacker-controlled — guest could
// otherwise write unbounded data to the vsock and OOM the host. 16
// MiB is ~16,000× larger than any expected legitimate response (the
// largest today is a few KiB); callers needing more should chunk.
const MaxResponseBytes = 16 << 20

// ErrResponseTooLarge is returned by [Call] when the agent's reply
// stream exceeds [MaxResponseBytes] without yielding a complete
// newline-terminated frame.
var ErrResponseTooLarge = errors.New("workload: vsock response exceeded MaxResponseBytes")

// AuthField is the JSON key the agent expects to carry the
// HMAC-SHA256 authentication tag on every non-ping command when a
// secret has been negotiated. The agent verifies by removing this key,
// re-serializing the command with sorted keys (no whitespace), and
// comparing the resulting HMAC.
const AuthField = "auth"

// Call dials the in-guest agent over Firecracker's vsock UDS,
// performs the CONNECT handshake, sends a JSON command (one line
// terminated with \n), and returns the JSON response.
func Call(ctx context.Context, vsockUDS string, port int, secret []byte, cmd map[string]any) (map[string]any, error) {
	// 1. Dial the host-side base UDS that Firecracker exposes.
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", vsockUDS)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", vsockUDS, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	}

	// 2. CONNECT handshake.
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}

	lr := &io.LimitedReader{R: conn, N: MaxResponseBytes + 1}
	r := bufio.NewReaderSize(lr, 64<<10)

	ack, err := r.ReadString('\n')
	if err != nil {
		if lr.N <= 0 {
			return nil, ErrResponseTooLarge
		}
		return nil, fmt.Errorf("vsock ack read: %w", err)
	}
	// the firecracker vsock proxy answers either
	//   "OK <peer_port>\n"      on success
	//   "ERROR <code>\n"        on failure
	// and never distinguished ERROR responses (they fell through as
	// "unexpected" with the entire line as message). Parse strictly:
	if err := parseAck(ack); err != nil {
		return nil, err
	}

	// 3. Optionally sign the cmd. We clone to avoid mutating the
	//    caller's map and to keep the canonical-bytes computation
	//    self-contained.
	signedCmd := cmd
	if len(secret) > 0 {
		signed, err := signCmd(cmd, secret)
		if err != nil {
			return nil, fmt.Errorf("sign cmd: %w", err)
		}
		signedCmd = signed
	}

	body, err := json.Marshal(signedCmd)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write cmd: %w", err)
	}

	// 4. Read the JSON response line.
	line, err := r.ReadBytes('\n')
	if err != nil {
		if lr.N <= 0 {
			return nil, ErrResponseTooLarge
		}
		return nil, fmt.Errorf("read response: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(line, &out); err != nil {
		return nil, fmt.Errorf("parse response %q: %w", string(line), err)
	}
	return out, nil
}

// WaitForAgent polls Call(ping) until it succeeds or ctx is cancelled.
// Useful right after [fc.Machine.Start] when the guest is still booting.
func WaitForAgent(ctx context.Context, vsockUDS string, port int) (map[string]any, []byte, error) {
	var lastErr error
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(60 * time.Second)
	}
	for time.Now().Before(deadline) {
		resp, err := Call(ctx, vsockUDS, port, nil, map[string]any{"cmd": "ping"})
		if err == nil {
			secret, _ := extractAgentSecret(resp)
			return resp, secret, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	// surface BOTH the last Call error and the ctx err
	// floor whenever lastErr was non-nil — operators chasing a stuck
	// build couldn't tell "agent never answered" from "we ran out of
	// time waiting".
	if lastErr == nil {
		return nil, nil, fmt.Errorf("WaitForAgent: %w", ctx.Err())
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, fmt.Errorf("WaitForAgent: %w", errors.Join(lastErr, ctxErr))
	}
	return nil, nil, fmt.Errorf("WaitForAgent: %w", lastErr)
}

// extractAgentSecret pulls the agent_secret_hex field from a ping
// reply if present. Missing field is not an error — that's a legacy
// agent and the caller will operate unsigned.
func extractAgentSecret(pong map[string]any) ([]byte, error) {
	v, ok := pong["agent_secret_hex"]
	if !ok {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("agent_secret_hex: not a string (%T)", v)
	}
	if s == "" {
		return nil, nil
	}
	return hex.DecodeString(s)
}

// parseAck enforces the firecracker vsock proxy's "OK <peer_port>\n" /
// "ERROR <code>\n" reply grammar. Trailing \r is
// tolerated for paranoid implementations; everything else is treated
// as a protocol error and surfaced.
func parseAck(line string) error {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		return fmt.Errorf("vsock ack: empty line")
	}
	parts := strings.SplitN(trimmed, " ", 2)
	switch parts[0] {
	case "OK":
		if len(parts) != 2 {
			return fmt.Errorf("vsock ack: missing peer_port in %q", trimmed)
		}
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return fmt.Errorf("vsock ack: peer_port not an int in %q", trimmed)
		}
		return nil
	case "ERROR":
		return fmt.Errorf("vsock ack: peer rejected: %q", trimmed)
	default:
		return fmt.Errorf("vsock ack: unexpected token %q (full line %q)", parts[0], trimmed)
	}
}

// signCmd returns a copy of cmd with AuthField set to
// HMAC-SHA256(canonical_json(cmd_without_auth), secret), hex-encoded.
func signCmd(cmd map[string]any, secret []byte) (map[string]any, error) {
	cp := make(map[string]any, len(cmd)+1)
	for k, v := range cmd {
		if k == AuthField {
			continue // never let caller pre-set auth
		}
		cp[k] = v
	}
	canon, err := canonicalJSON(cp)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(canon)
	cp[AuthField] = hex.EncodeToString(mac.Sum(nil))
	return cp, nil
}

// canonicalJSON serializes m as JSON with keys sorted lexicographically
// and no whitespace. Matches Python's
// json.dumps(d, sort_keys=True, separators=(',', ':')) byte-for-byte
// so the agent's HMAC verification produces the same digest.
func canonicalJSON(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	encodeValue := func(v any) ([]byte, error) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		// json.Encoder always appends a trailing newline — strip it.
		out := b.Bytes()
		if len(out) > 0 && out[len(out)-1] == '\n' {
			out = out[:len(out)-1]
		}
		return out, nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := encodeValue(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := encodeValue(m[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
