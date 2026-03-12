#!/usr/bin/env python3
"""firefork in-guest agent.

Runs inside every Firecracker microVM. Listens on AF_VSOCK port 1234
and accepts newline-delimited JSON commands from the host orchestrator.

Authentication
--------------
On startup the agent generates a 32-byte random secret, writes it to
``/run/firefork/agent.secret`` (mode 0o600), and exposes the hex form
via the ``ping`` reply ("agent_secret_hex" field).

The host calls ``ping`` once at boot (unauthenticated), captures the
secret, and signs every subsequent command with HMAC-SHA256 over a
canonical JSON serialization (sorted keys, no whitespace) of the
command — placing the hex tag in the ``auth`` field.

Every non-ping command must carry a valid ``auth`` tag if the secret
file exists. Missing secret file (e.g. older builds, /run filesystem
absent) falls back to unauthenticated operation for backward compat.

Supported commands:
    {"cmd":"ping"}                  → {"ok":true,"pid":<int>,"uptime_s":<float>,
                                       "agent_secret_hex":"..."}
    {"cmd":"echo","text":"...","auth":"..."}
                                    → {"ok":true,"text":"..."}
    {"cmd":"exec","argv":[...],"timeout":N?,"auth":"..."}
                                    → {"ok":bool,"rc":int,"stdout":"...","stderr":"..."}
    {"cmd":"python","src":"...","auth":"..."}
                                    → {"ok":bool,"vars":[...],"error":"...?"}
    {"cmd":"write_file","path":"...","content":"...","auth":"..."}
                                    → {"ok":bool,"bytes":N}
    {"cmd":"read_file","path":"...","auth":"..."}
                                    → {"ok":bool,"content":"..."}
"""
from __future__ import annotations

import hmac
import hashlib
import json
import os
import secrets
import socket
import subprocess
import sys
import time
import traceback

VSOCK_PORT = 1234
START_TIME = time.monotonic()

# Symmetric cap on inbound command size — protects the guest from a
# misbehaving (or attacker-controlled) host that streams unbounded
# bytes into the vsock without ever sending a newline. Matches
# MaxResponseBytes in internal/workload/vsock.go.
MAX_CMD_BYTES = 16 * 1024 * 1024

SECRET_PATH = "/run/firefork/agent.secret"
AUTH_FIELD = "auth"
# Commands that never require auth (bootstrap / pre-rekey).
UNAUTH_CMDS = {"ping"}

# Loaded at startup by _init_secret(); None means we operate unsigned
# (backward compat with snapshots that didn't carry a secret).
SECRET: bytes | None = None
SECRET_HEX: str = ""


def _init_secret() -> None:
    """Generate or load the per-boot secret. Best-effort: failure to
    create the file leaves SECRET None and we accept unsigned cmds."""
    global SECRET, SECRET_HEX
    try:
        os.makedirs(os.path.dirname(SECRET_PATH), exist_ok=True)
        if os.path.exists(SECRET_PATH):
            with open(SECRET_PATH, "rb") as f:
                raw = f.read().strip()
            try:
                SECRET = bytes.fromhex(raw.decode())
            except Exception:
                SECRET = None
        if SECRET is None:
            SECRET = secrets.token_bytes(32)
            with open(SECRET_PATH, "w") as f:
                f.write(SECRET.hex())
            os.chmod(SECRET_PATH, 0o600)
        SECRET_HEX = SECRET.hex()
        print(f"agent: secret loaded ({len(SECRET)} bytes)", flush=True)
    except Exception as e:
        SECRET = None
        SECRET_HEX = ""
        print(f"agent: WARNING secret init failed ({e}); running unauthenticated", flush=True)


def _canonical_json(d: dict) -> bytes:
    """Match Go's workload.canonicalJSON byte-for-byte: top-level keys
    sorted, no whitespace."""
    return json.dumps(d, sort_keys=True, separators=(",", ":")).encode()


def _verify_auth(cmd: dict) -> bool:
    if SECRET is None:
        return True  # unsigned mode (no secret loaded)
    tag = cmd.pop(AUTH_FIELD, None)
    if not isinstance(tag, str):
        return False
    expected = hmac.new(SECRET, _canonical_json(cmd), hashlib.sha256).hexdigest()
    return hmac.compare_digest(tag, expected)


def serve() -> None:
    s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((socket.VMADDR_CID_ANY, VSOCK_PORT))
    s.listen(8)
    print(f"agent: listening on vsock port {VSOCK_PORT} pid={os.getpid()}", flush=True)
    while True:
        conn, _ = s.accept()
        try:
            handle(conn)
        except Exception:
            traceback.print_exc()
        finally:
            try:
                conn.close()
            except Exception:
                pass


def handle(conn: socket.socket) -> None:
    raw = b""
    conn.settimeout(30.0)
    while b"\n" not in raw:
        chunk = conn.recv(4096)
        if not chunk:
            break
        raw += chunk
        if len(raw) > MAX_CMD_BYTES:
            reply(conn, {"ok": False, "error": f"cmd too large (>{MAX_CMD_BYTES} bytes)"})
            return
    if not raw:
        return
    try:
        cmd = json.loads(raw.decode())
    except Exception as e:
        reply(conn, {"ok": False, "error": f"bad json: {e}"})
        return
    if not isinstance(cmd, dict):
        reply(conn, {"ok": False, "error": "cmd must be a JSON object"})
        return

    name = cmd.get("cmd")
    if name not in UNAUTH_CMDS:
        if not _verify_auth(cmd):
            reply(conn, {"ok": False, "error": "unauthenticated"})
            return

    resp = dispatch(cmd)
    reply(conn, resp)


def reply(conn: socket.socket, obj) -> None:
    conn.sendall((json.dumps(obj) + "\n").encode())


def dispatch(cmd: dict):
    name = cmd.get("cmd")
    if name == "ping":
        return {
            "ok": True,
            "pid": os.getpid(),
            "uptime_s": time.monotonic() - START_TIME,
            "agent_secret_hex": SECRET_HEX,
        }
    if name == "echo":
        return {"ok": True, "text": cmd.get("text", "")}
    if name == "exec":
        argv = cmd.get("argv", [])
        if not argv:
            return {"ok": False, "error": "exec: argv required"}
        timeout = cmd.get("timeout", 30)
        try:
            out = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            return {"ok": False, "error": "timeout"}
        except Exception as e:
            return {"ok": False, "error": str(e)}
        return {
            "ok": out.returncode == 0,
            "rc": out.returncode,
            "stdout": out.stdout,
            "stderr": out.stderr,
        }
    if name == "python":
        src = cmd.get("src", "")
        g = {"__name__": "__agent__"}
        try:
            exec(compile(src, "<agent>", "exec"), g)
        except Exception as e:
            return {"ok": False, "error": str(e), "trace": traceback.format_exc()}
        vars_ = [k for k in g if not k.startswith("_") and k != "__name__"]
        return {"ok": True, "vars": vars_}
    if name == "write_file":
        path = cmd.get("path")
        content = cmd.get("content", "")
        if not path:
            return {"ok": False, "error": "write_file: path required"}
        try:
            os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
            with open(path, "w") as f:
                n = f.write(content)
            return {"ok": True, "bytes": n}
        except Exception as e:
            return {"ok": False, "error": str(e)}
    if name == "read_file":
        path = cmd.get("path")
        if not path:
            return {"ok": False, "error": "read_file: path required"}
        try:
            with open(path) as f:
                return {"ok": True, "content": f.read()}
        except Exception as e:
            return {"ok": False, "error": str(e)}
    return {"ok": False, "error": f"unknown cmd {name!r}"}


if __name__ == "__main__":
    _init_secret()
    try:
        serve()
    except KeyboardInterrupt:
        print("agent: shutting down", flush=True)
        sys.exit(0)
