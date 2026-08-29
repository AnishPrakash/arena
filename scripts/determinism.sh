# file: scripts/determinism.sh
#!/usr/bin/env bash
# Submits the SAME program N times and reports the spread of measured CPU time.
# The output number is the honest answer to "how deterministic is it, really?"
set -euo pipefail

API="${API:-http://localhost:8080}"
N="${N:-20}"

TOKEN=$(curl -s -X POST "$API/v1/auth/login" -H 'content-type: application/json' \
  -d '{"handle":"anish","password":"password123"}' | jq -r .token)
PROB=$(curl -s "$API/v1/contests/technovit-speed/problems" \
  | jq -r '.[]|select(.slug=="max-subarray").id')

# Deterministic workload with no I/O dependence and no randomness.
SRC=$'import sys\nd=sys.stdin.read().split()\nn=int(d[0])\na=list(map(int,d[1:1+n]))\nb=-10**18\nc=0\nfor v in a:\n    c=max(0,c)+v\n    b=max(b,c)\nprint(b)'

echo "running $N identical submissions..."
: > /tmp/arena-cpu.txt
for i in $(seq 1 "$N"); do
  # A unique trailing comment per iteration, or the verdict cache replays one stored
  # result 19 times and reports a CV of 0.00% while measuring nothing at all.
  ITER_SRC="$SRC"$'\n''#'" $i-$(date +%s%N)"
  ID=$(jq -nc --arg s "$ITER_SRC" '{language:"python312",source:$s}' \
    | curl -s -X POST "$API/v1/problems/$PROB/submissions" \
        -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
        -H "Idempotency-Key: det-$i-$(date +%s%N)" -d @- | jq -r .id)
  for _ in $(seq 1 ${POLL_TICKS:-360}); do
    B=$(curl -s "$API/v1/submissions/$ID" -H "Authorization: Bearer $TOKEN")
    [ "$(echo "$B" | jq -r .status)" = "DONE" ] && break
    sleep 0.5
  done
  # A sample that did not reach DONE is a MISSING measurement, not a fast one.
  # Averaging it in as 0 produced a 139% CV on the first run - noise from timeouts,
  # not variance from the judge.
  ST=$(echo "$B" | jq -r .status)
  CM=$(echo "$B" | jq -r '.cpu_ms // empty')
  if [ "$ST" = "DONE" ] && [ -n "$CM" ] && [ "$CM" != "null" ]; then
    echo "$CM" >> /tmp/arena-cpu.txt
  else
    echo "  ! sample $i discarded (status=$ST cpu_ms=$CM)" >&2
  fi
  printf "."
done
echo

awk '{ s+=$1; a[NR]=$1 }
 END {
   n=NR; mean=s/n
   for (i=1;i<=n;i++) { d=a[i]-mean; v+=d*d }
   sd=sqrt(v/n)
   asort(a)
   printf "\n  samples : %d\n  mean    : %.1f ms\n  stddev  : %.2f ms\n", n, mean, sd
   printf "  cv      : %.2f %%   <- coefficient of variation\n", 100*sd/mean
   printf "  min/max : %d / %d ms\n", a[1], a[n]
 }' /tmp/arena-cpu.txt
