#!/usr/bin/env bash
set +e
echo "=== virt flag count (need >= 1) ==="
egrep -c "(vmx|svm)" /proc/cpuinfo
echo
echo "=== /dev/kvm ==="
ls -la /dev/kvm 2>&1
echo
echo "=== modprobe attempts ==="
sudo modprobe kvm 2>&1 && echo "kvm: loaded"
sudo modprobe kvm-intel 2>&1 && echo "kvm-intel: loaded"
sudo modprobe kvm-amd 2>&1 && echo "kvm-amd: loaded"
echo
echo "=== lsmod kvm ==="
lsmod | grep -i kvm || echo "no kvm modules loaded"
echo
echo "=== kvm-ok ==="
sudo apt-get update -qq 2>/dev/null
sudo apt-get install -y -qq cpu-checker 2>/dev/null
sudo kvm-ok 2>&1
echo
echo "=== cpu model ==="
grep -m1 "model name" /proc/cpuinfo
echo
echo "=== resources ==="
nproc
free -h | head -2
df -h / | tail -1
echo
echo "=== done ==="
