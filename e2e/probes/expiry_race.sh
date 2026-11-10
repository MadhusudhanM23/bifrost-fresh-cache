#!/usr/bin/env bash
# Expiry race probe: write N entries with a 1 s TTL, let them expire, re-request each one
# (miss -> plugin deletes the expired row asynchronously AND writes the fresh response under
# the same deterministic id), then check whether the row survived.
G=${G:-http://127.0.0.1:18090/v1/chat/completions}
N=${N:-20}
KEY=${KEY:-race$RANDOM}
T='"public"."BifrostSemanticCachePlugin"'
ask() { # question extra-curl-args... ; prints "hit id"
  local q="$1"; shift
  curl -s -o /dev/null -D - "$G" -H "Content-Type: application/json" -H "x-bf-cache-key: $KEY" "$@" \
    -d "{\"model\":\"openai/mock-chat\",\"messages\":[{\"role\":\"user\",\"content\":\"$q\"}]}" \
    | tr -d '\r' | awk -F': ' 'tolower($1)=="x-bifrost-cache-hit"{h=$2} tolower($1)=="x-bifrost-cache-id"{i=$2} END{print h, i}'
}
declare -a IDS
for n in $(seq 1 $N); do
  read -r hit id < <(ask "Expiry race question number $n about caching" -H "x-bf-cache-ttl: 1s" -H "x-bf-cache-type: direct")
  IDS[$n]=$id
done
sleep 1
echo "written: $(docker exec bifrost-pgvector-test psql -U bifrost -d bifrost -At -c "SELECT count(*) FROM $T WHERE metadata->>'cache_key' = '$KEY'") rows under key $KEY"
sleep 2.5
for n in $(seq 1 $N); do ask "Expiry race question number $n about caching" -H "x-bf-cache-type: direct" > /dev/null; done
sleep 2
missing=0
for n in $(seq 1 $N); do
  c=$(docker exec bifrost-pgvector-test psql -U bifrost -d bifrost -At -c "SELECT count(*) FROM $T WHERE id = '${IDS[$n]}'")
  [ "$c" = "1" ] || { missing=$((missing+1)); echo "MISSING after rewrite: ${IDS[$n]}"; }
done
echo "rows missing after expired-entry rewrite: $missing of $N"
