"""SSH into the Contabo VPS and run the diagnostic script. Returns combined stdout/stderr."""
import paramiko
import sys
from pathlib import Path

HOST = "5.189.164.223"
USER = "root"
PASS = "MlI3rM2K5VeP7Lo00NP6Ud5gWw"

DIAG = Path(__file__).parent / "diag.sh"

def main():
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(HOST, username=USER, password=PASS, timeout=20)
    with DIAG.open() as f:
        script = f.read()
    # Upload script via cat, then exec
    stdin, stdout, stderr = client.exec_command("bash -s", timeout=60)
    stdin.write(script)
    stdin.channel.shutdown_write()
    out = stdout.read().decode(errors="replace")
    err = stderr.read().decode(errors="replace")
    print(out)
    if err.strip():
        print("--- STDERR ---", file=sys.stderr)
        print(err, file=sys.stderr)
    client.close()

if __name__ == "__main__":
    main()
