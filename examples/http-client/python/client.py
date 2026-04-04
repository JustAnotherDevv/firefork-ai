#!/usr/bin/env python3
"""
Drive firefork-server from Python. Spawns one fork, runs a command via
/v1/exec, deletes the fork.

Stdlib only (no httpx dependency). Python 3.9+.

Usage:
    export FIREFORK_AUTH_TOKEN=...
    python3 examples/http-client/python/client.py \\
        --base http://localhost:8080 \\
        --template python/v1
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request


def call(base: str, token: str, method: str, path: str, body=None):
    """Send a single JSON request to the server."""
    url = base.rstrip("/") + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            raw = resp.read()
            if not raw:
                return None
            return json.loads(raw)
    except urllib.error.HTTPError as e:
        sys.exit(f"HTTP {e.code}: {e.read().decode(errors='replace')}")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default="http://localhost:8080")
    ap.add_argument("--template", default="python/v1")
    ap.add_argument("--cmd", default="uname -a && id")
    args = ap.parse_args()

    token = os.environ.get("FIREFORK_AUTH_TOKEN", "")
    if not token:
        print("[warn] FIREFORK_AUTH_TOKEN unset; server is in DEMO mode or you'll get 401",
              file=sys.stderr)

    # 1. Spawn one fork.
    spawn = call(args.base, token, "POST", "/v1/fork",
                 {"template": args.template, "count": 1})
    if not spawn or not spawn.get("forks"):
        sys.exit(f"unexpected /v1/fork reply: {spawn}")
    f = spawn["forks"][0]
    if f.get("error"):
        sys.exit(f"fork failed: {f['error']}")
    print(f"forked {f['template_key']} id={f['id']} latency={f['latency_ms']}ms")

    try:
        # 2. Run a command inside it.
        exec_result = call(args.base, token, "POST", "/v1/exec",
                           {"fork_id": f["id"], "cmd": args.cmd, "timeout_ms": 10000})
        print(f"exec latency={exec_result['latency_ms']}ms")
        print(f"result: {json.dumps(exec_result['result'], indent=2)}")
    finally:
        # 3. Tear down.
        call(args.base, token, "DELETE", f"/v1/forks/{f['id']}", None)


if __name__ == "__main__":
    main()
