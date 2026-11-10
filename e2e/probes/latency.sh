#!/usr/bin/env bash
# Latency probe: compares cache_hit_latency for the different lookup paths.
G=${G:-http://127.0.0.1:18090/v1/chat/completions}
req() {
  local key="$1"; shift; local q="$1"; shift
  curl -s -o /dev/null -D - -w 'total_ms=%{time_total}\n' "$G" -H "Content-Type: application/json" \
    -H "x-bf-cache-key: $key" "$@" \
    -d "{\"model\":\"openai/mock-chat\",\"messages\":[{\"role\":\"user\",\"content\":\"$q\"}]}" \
    | grep -iE "x-bifrost-cache-hit:|hit-type|hit-latency|similarity:|total_ms" | tr -d '\r' | tr '\n' ' '
  echo
}
K=${K:-lat$RANDOM}
echo "key=$K"
echo "== seed (miss, write)"; req "$K" "What is the capital city of France?"
sleep 1
echo "== E1 direct miss + semantic hit (default cache type)"; for i in 1 2 3 4 5; do req "$K" "Tell me the capital city of France."; done
echo "== E2 semantic-only hit (x-bf-cache-type: semantic)"; for i in 1 2 3 4 5; do req "$K" "Tell me the capital city of France." -H "x-bf-cache-type: semantic"; done
echo "== E3 direct miss + semantic miss + upstream (no-store)"; for i in 1 2 3 4 5; do req "$K" "How do I bake sourdough bread at home?" -H "x-bf-cache-no-store: true"; done
echo "== E4 semantic-only, identical prompt"; for i in 1 2 3 4 5; do req "$K" "What is the capital city of France?" -H "x-bf-cache-type: semantic"; done
echo "== E5 direct hit"; for i in 1 2 3; do req "$K" "What is the capital city of France?"; done
