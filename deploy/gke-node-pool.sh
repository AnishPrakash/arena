#!/usr/bin/env bash
# Judge node pool for GKE. NOT run for this submission — the live instance is a single
# Compute Engine VM. Recorded here because two flags carry the whole scalability story.
set -euo pipefail

# cpuManagerPolicy=static is what actually grants a Guaranteed pod exclusive cores.
# Without it, Kubernetes hands out CFS shares: judging threads get descheduled by
# whatever else lands on the node and every timing measurement becomes noise.
# This one kubelet flag is the difference between "we pin cores" and "we meant to".
cat > node-config.yaml <<'CFG'
kubeletConfig:
  cpuManagerPolicy: static
CFG

# --spot is ~70% cheaper and is safe *because* jobs are leased and idempotent:
# preemption sends SIGTERM, the runner nacks in-flight work, another runner takes it.
# The resilience feature and the largest cost lever are the same feature.
gcloud container node-pools create judge \
  --cluster arena --machine-type c2d-standard-4 \
  --num-nodes 1 --enable-autoscaling --min-nodes 1 --max-nodes 20 \
  --spot \
  --node-labels arena.io/pool=judge \
  --node-taints arena.io/judge=true:NoSchedule \
  --system-config-from-file node-config.yaml
