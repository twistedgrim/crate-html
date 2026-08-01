#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose_file="$repo_root/tests/e2e/docker-compose.metrics.yml"
project="crate-html-metrics-smoke"
image="${CRATE_METRICS_E2E_IMAGE:-crate-html:metrics-e2e}"
dc=(docker compose -p "$project" -f "$compose_file")

cleanup() {
  local status=$?
  if ((status != 0)); then
    echo "docker metrics smoke: failure diagnostics" >&2
    "${dc[@]}" ps -a >&2 || true
    "${dc[@]}" logs --no-color >&2 || true
  fi
  "${dc[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT INT TERM
cleanup

if [[ "${CRATE_METRICS_E2E_SKIP_BUILD:-0}" != "1" ]]; then
  docker build -t "$image" "$repo_root"
fi
export CRATE_METRICS_E2E_IMAGE="$image"
"${dc[@]}" up -d --wait broker

# /api/status is a broker route and emits an application HTTP metric.
"${dc[@]}" exec -T broker wget -qO /dev/null http://127.0.0.1:7777/api/status
"${dc[@]}" exec -T broker wget -qO- http://127.0.0.1:9464/metrics |
  grep -Fq "crate_broker_http_requests"

for _ in $(seq 1 100); do
  query="$("${dc[@]}" exec -T broker wget -qO- 'http://prometheus:9090/api/v1/query?query=crate_broker_http_requests_total' 2>/dev/null || true)"
  if grep -Fq '"status":"success"' <<<"$query" && grep -Fq 'crate_broker_http_requests' <<<"$query"; then
    echo "docker metrics smoke: passed"
    exit 0
  fi
  sleep 0.2
done

echo "Prometheus did not observe crate_broker_http_requests_total" >&2
exit 1
