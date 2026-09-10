# Live-demo helpers.  Usage:  source scripts/demo.sh   then:  judge python312 '<code>'
API="${API:-http://localhost:8080}"
TOKEN=$(curl -s -X POST $API/v1/auth/login -H 'content-type: application/json' \
  -d '{"handle":"anish","password":"password123"}' | jq -r .token)
PROB=$(curl -s $API/v1/contests/technovit-speed/problems | jq -r '.[]|select(.slug=="a-plus-b").id')

judge() {
  local id
  id=$(jq -nc --arg l "$1" --arg s "$2" '{language:$l,source:$s}' \
    | curl -s -X POST $API/v1/problems/$PROB/submissions \
        -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
        -H "Idempotency-Key: demo-$(date +%s%N)" -d @- | jq -r .id)
  for i in $(seq 1 120); do
    local b; b=$(curl -s $API/v1/submissions/$id -H "Authorization: Bearer $TOKEN")
    case "$(echo "$b" | jq -r .status)" in
      DONE|FAILED) echo "$b" | jq -c '{verdict, cpu_ms, mem_kb, failed_test}'; return;;
    esac
    sleep 1
  done
}
