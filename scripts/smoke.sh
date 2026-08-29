# file: scripts/smoke.sh
#!/usr/bin/env bash
# End-to-end verification. Exits non-zero if any verdict is wrong, so it can gate CI.
set -uo pipefail

API="${API:-http://localhost:8080}"
HANDLE="${HANDLE:-anish}"
PASS="${PASS:-password123}"
FAILED=0

say() { printf "\n\033[1m== %s\033[0m\n" "$1"; }

say "waiting for the API"
for i in $(seq 1 60); do
  curl -sf "$API/readyz" >/dev/null && break
  sleep 1
done
curl -sf "$API/readyz" >/dev/null || { echo "API never became ready"; exit 1; }

TOKEN=$(curl -s -X POST "$API/v1/auth/login" -H 'content-type: application/json' \
  -d "{\"handle\":\"$HANDLE\",\"password\":\"$PASS\"}" | jq -r .token)
[ "$TOKEN" != "null" ] || { echo "login failed"; exit 1; }

PROB=$(curl -s "$API/v1/contests/technovit-speed/problems" \
  | jq -r '.[]|select(.slug=="a-plus-b").id')
[ -n "$PROB" ] || { echo "problem not seeded"; exit 1; }

# submit <lang> <source> -> submission id
submit() {
  jq -nc --arg l "$1" --arg s "$2" '{language:$l,source:$s}' \
  | curl -s -X POST "$API/v1/problems/$PROB/submissions" \
      -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
      -H "Idempotency-Key: smoke-$(date +%s%N)" -d @- \
  | jq -r .id
}

# expect <name> <want> <lang> <source>
expect() {
  local name="$1" want="$2" lang="$3" src="$4"
  local id; id=$(submit "$lang" "$src")
  local got="" status=""
  for i in $(seq 1 ${POLL_SECS:-45}); do
    local body; body=$(curl -s "$API/v1/submissions/$id" -H "Authorization: Bearer $TOKEN")
    status=$(echo "$body" | jq -r .status)
    got=$(echo "$body" | jq -r .verdict)
    [ "$status" = "DONE" ] || [ "$status" = "FAILED" ] && break
    sleep 1
  done
  if [ "$got" = "$want" ]; then
    printf "  \033[32mPASS\033[0m %-28s %s\n" "$name" "$got"
  else
    printf "  \033[31mFAIL\033[0m %-28s got=%s want=%s status=%s id=%s\n" \
      "$name" "$got" "$want" "$status" "$id"
    FAILED=1
  fi
}

say "verdict matrix"
expect "accepted (python)"      AC  python312 $'a,b=map(int,input().split())\nprint(a+b)'
expect "accepted (c++)"         AC  cpp20     $'#include <iostream>\nint main(){long long a,b;std::cin>>a>>b;std::cout<<a+b<<"\\n";}'
expect "wrong answer"           WA  python312 $'a,b=map(int,input().split())\nprint(a-b)'
expect "infinite loop -> cpu"   TLE python312 $'while True: pass'
expect "sleep -> wall clock"    TLE python312 $'import time\ntime.sleep(60)'
expect "memory bomb"            MLE python312 $'x=bytearray(900*1024*1024)\nprint(x[0])'
expect "runtime error"          RE  python312 $'print(1/0)'
expect "output flood"           OLE python312 $'while True: print("x"*1000)'
expect "compile error"          CE  cpp20     $'int main(){ this is not valid }'
expect "fork bomb contained"    RE  python312 $'import os\nwhile True: os.fork()'
expect "network blocked"        RE  python312 $'import socket\nsocket.create_connection(("1.1.1.1",80),timeout=3)'

say "leaderboard"
curl -s "$API/v1/contests/technovit-speed/leaderboard" | jq '.entries[:5]'

say "queue"
curl -s "$API/metrics" | grep -E '^arena_(queue|verdict|oom|wall)' | head -12

if [ "$FAILED" -eq 0 ]; then
  printf "\n\033[32mALL SMOKE TESTS PASSED\033[0m\n"
else
  printf "\n\033[31mSMOKE TESTS FAILED\033[0m\n"
fi
exit $FAILED
