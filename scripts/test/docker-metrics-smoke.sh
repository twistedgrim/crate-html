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
"${dc[@]}" up -d --wait broker grafana

# /api/status is a broker route and emits an application HTTP metric.
"${dc[@]}" exec -T broker wget -qO /dev/null http://127.0.0.1:7777/api/status
metrics="$("${dc[@]}" exec -T broker wget -qO- http://127.0.0.1:9464/metrics)"
grep -Fq "crate_broker_http_requests" <<<"$metrics"
grep -Eq 'crate_broker_http_request_duration_seconds_bucket\{[^}]*le="0\.005"' <<<"$metrics"

prometheus_ready=0
for _ in $(seq 1 100); do
  prometheus_query="$("${dc[@]}" exec -T broker wget -qO- 'http://prometheus:9090/api/v1/query?query=crate_broker_http_requests_total' 2>/dev/null || true)"
  if grep -Fq '"status":"success"' <<<"$prometheus_query" && grep -Fq 'crate_broker_http_requests' <<<"$prometheus_query"; then
    prometheus_ready=1
    break
  fi
  sleep 0.2
done
if [[ "$prometheus_ready" != "1" ]]; then
  echo "Prometheus did not observe crate_broker_http_requests_total" >&2
  exit 1
fi

grafana_ready=0
for _ in $(seq 1 100); do
  datasource_health="$("${dc[@]}" exec -T broker wget -qO- 'http://grafana:3000/api/datasources/uid/prometheus/health' 2>/dev/null || true)"
  datasource="$("${dc[@]}" exec -T broker wget -qO- 'http://grafana:3000/api/datasources/uid/prometheus' 2>/dev/null || true)"
  dashboard="$("${dc[@]}" exec -T broker wget -qO- 'http://grafana:3000/api/dashboards/uid/crate-broker' 2>/dev/null || true)"
  grafana_query="$("${dc[@]}" exec -T broker wget -qO- 'http://grafana:3000/api/datasources/proxy/uid/prometheus/api/v1/query?query=crate_broker_http_requests_total' 2>/dev/null || true)"
  if grep -Fq '"status":"OK"' <<<"$datasource_health" &&
    grep -Fq '"timeInterval":"15s"' <<<"$datasource" &&
    grep -Fq '"uid":"crate-broker"' <<<"$dashboard" &&
    grep -Fq 'crate-html Broker' <<<"$dashboard" &&
    grep -Fq 'crate_broker_http_requests' <<<"$grafana_query"; then
    grafana_ready=1
    break
  fi
  sleep 0.2
done
if [[ "$grafana_ready" != "1" ]]; then
  echo "Grafana did not provision the datasource/dashboard or return broker metrics" >&2
  exit 1
fi

echo "docker metrics smoke: passed (Prometheus + Grafana)"
