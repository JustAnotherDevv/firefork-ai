#!/bin/bash
set +e
echo "=== os ==="
cat /etc/os-release | head -3
echo "=== kernel ==="
uname -r
echo "=== cpu virt flags (need >=1) ==="
egrep -c "(vmx|svm)" /proc/cpuinfo
echo "=== /dev/kvm ==="
ls -la /dev/kvm 2>&1
echo "=== cpuinfo virt flag detail ==="
grep -m1 -oE "vmx|svm" /proc/cpuinfo
echo "=== nested check (if /dev/kvm exists) ==="
test -e /dev/kvm && echo "KVM device present" || echo "KVM device MISSING"
echo "=== modprobe attempt ==="
modprobe kvm 2>&1 && echo "kvm loaded" || echo "kvm load failed"
modprobe kvm-intel 2>&1 || modprobe kvm-amd 2>&1
echo "=== loaded modules ==="
lsmod | grep -i kvm || echo "(no kvm modules loaded)"
echo "=== cpu model ==="
grep -m1 "model name" /proc/cpuinfo
echo "=== resources ==="
nproc
free -h | head -2
df -h / | tail -1
echo "=== done ==="
