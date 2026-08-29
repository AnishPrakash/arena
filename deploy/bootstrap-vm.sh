# file: deploy/bootstrap-vm.sh
#!/usr/bin/env bash
# Prepare a fresh Ubuntu 24.04 GCE VM to run Arena. Idempotent — safe to re-run.
set -euo pipefail

echo "==> packages"
sudo apt-get update -qq
sudo apt-get install -y -qq docker.io docker-compose-v2 git make jq curl

echo "==> docker without sudo"
sudo usermod -aG docker "$USER"

echo "==> cgroups v2 check"
# Arena's memory limits, pids limits and OOM detection all depend on the unified hierarchy.
# Failing here is far better than every submission mysteriously returning RE later.
grep -q cgroup2 /proc/filesystems || { echo "FATAL: no cgroups v2"; exit 1; }
test -f /sys/fs/cgroup/cgroup.controllers || { echo "FATAL: cgroup v1 hierarchy"; exit 1; }

echo "==> swap off"
# A judge node must never swap. Swapping turns a clean MLE into a slow wall-clock timeout
# and corrupts every timing measurement taken on the box.
sudo swapoff -a
sudo sed -i '/ swap / s/^/#/' /etc/fstab

echo "==> kernel limits"
# Many short-lived sandbox containers create many sockets, PIDs and inotify watches.
sudo tee /etc/sysctl.d/99-arena.conf >/dev/null <<'SYSCTL'
vm.swappiness = 0
vm.max_map_count = 262144
fs.file-max = 1048576
fs.inotify.max_user_instances = 8192
kernel.pid_max = 4194304
net.core.somaxconn = 4096
SYSCTL
sudo sysctl --system >/dev/null

echo "==> ulimits"
sudo tee /etc/security/limits.d/99-arena.conf >/dev/null <<'LIMITS'
*  soft  nofile  1048576
*  hard  nofile  1048576
*  soft  nproc   unlimited
*  hard  nproc   unlimited
LIMITS

echo "==> docker daemon"
# Without log rotation, one output-flooding submission fills the disk through the json
# log driver. live-restore keeps sandboxes alive across a daemon restart.
sudo tee /etc/docker/daemon.json >/dev/null <<'DOCKERD'
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" },
  "live-restore": true,
  "default-ulimits": { "nofile": { "Name": "nofile", "Soft": 65536, "Hard": 65536 } }
}
DOCKERD
sudo systemctl restart docker

echo "==> blob root"
# Docker creates a missing bind-mount source as root; the API image runs non-root, so the
# seeder cannot create testdata/ inside it. Create it world-writable up front.
sudo mkdir -p /var/tmp/arena-blobs
sudo chmod 0777 /var/tmp/arena-blobs

echo "==> done. Log out and back in for the docker group to take effect."
