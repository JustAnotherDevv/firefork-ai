#!/usr/bin/env bash
# Diagnostic: launch jailer manually, inspect what /proc/<pid>/root says.
set -u

sudo pkill -9 firecracker 2>/dev/null || true
sleep 1
sudo rm -rf /srv/jailer/firecracker/diag-1

(sudo /usr/local/bin/jailer \
   --id diag-1 \
   --exec-file /usr/local/bin/firecracker \
   --uid 10000 --gid 10000 \
   --chroot-base-dir /srv/jailer -- &) 2>/dev/null

sleep 1
echo "=== ps -ef | grep ==="
ps -ef | grep -E 'jailer|firecracker' | grep -v grep
echo
echo "=== /proc inspection ==="
for p in $(pgrep firecracker); do
  echo "pid=$p"
  echo "  root: $(sudo readlink /proc/$p/root 2>&1)"
  sudo grep '^Uid:' /proc/$p/status
  echo "  cwd:  $(sudo readlink /proc/$p/cwd 2>&1)"
done

echo
sudo pkill -9 firecracker 2>/dev/null || true
sudo pkill -9 jailer 2>/dev/null || true
sudo rm -rf /srv/jailer/firecracker/diag-1
