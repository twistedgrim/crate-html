#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
local_compose="$repo_root/tests/e2e/docker-compose.local.yml"
s3_compose="$repo_root/tests/e2e/docker-compose.s3.yml"
local_project="crate-html-e2e-local"
s3_project="crate-html-e2e-s3"
e2e_image="${CRATE_E2E_IMAGE:-crate-html:e2e}"
mode="${1:-all}"

local_dc=(docker compose -p "$local_project" -f "$local_compose")
s3_dc=(docker compose -p "$s3_project" -f "$s3_compose")

cleanup() {
  local status=$?
  if ((status != 0)); then
    echo "docker e2e: failure diagnostics (local)" >&2
    "${local_dc[@]}" ps -a >&2 || true
    "${local_dc[@]}" logs --no-color >&2 || true
    echo "docker e2e: failure diagnostics (s3)" >&2
    "${s3_dc[@]}" --profile tools ps -a >&2 || true
    "${s3_dc[@]}" --profile tools logs --no-color >&2 || true
  fi
  "${local_dc[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  "${s3_dc[@]}" --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
  return "$status"
}
trap cleanup EXIT INT TERM
cleanup

if [[ "${CRATE_E2E_SKIP_BUILD:-0}" != "1" ]]; then
  docker build -t "$e2e_image" "$repo_root"
fi
export CRATE_E2E_IMAGE="$e2e_image"

http_status() {
  local compose_name="$1"
  local service="$2"
  local url="$3"
  local output
  if [[ "$compose_name" == "local" ]]; then
    # shellcheck disable=SC2016 # $1 expands inside the container shell.
    output="$("${local_dc[@]}" exec -T "$service" sh -c 'wget -S -O /dev/null "$1" 2>&1 || true' sh "$url")"
  else
    # shellcheck disable=SC2016 # $1 expands inside the container shell.
    output="$("${s3_dc[@]}" exec -T "$service" sh -c 'wget -S -O /dev/null "$1" 2>&1 || true' sh "$url")"
  fi
  awk '/^[[:space:]]+HTTP\// { code=$2 } END { print code }' <<<"$output"
}

wait_for_status() {
  local compose_name="$1"
  local service="$2"
  local url="$3"
  local want="$4"
  local attempts="${5:-150}"
  local got=""
  for ((i = 0; i < attempts; i++)); do
    got="$(http_status "$compose_name" "$service" "$url")"
    if [[ "$got" == "$want" ]]; then
      return 0
    fi
    sleep 0.1
  done
  echo "GET $url from $service returned $got; wanted $want" >&2
  return 1
}

wait_for_body() {
  local compose_name="$1"
  local service="$2"
  local url="$3"
  local want="$4"
  local attempts="${5:-150}"
  local body=""
  for ((i = 0; i < attempts; i++)); do
    if [[ "$compose_name" == "local" ]]; then
      body="$("${local_dc[@]}" exec -T "$service" wget -qO- "$url" 2>/dev/null || true)"
    else
      body="$("${s3_dc[@]}" exec -T "$service" wget -qO- "$url" 2>/dev/null || true)"
    fi
    if grep -Fq "$want" <<<"$body"; then
      return 0
    fi
    sleep 0.1
  done
  echo "GET $url from $service did not contain: $want" >&2
  return 1
}

run_local() {
  echo "docker e2e: split local-volume topology"
  "${local_dc[@]}" up -d --wait web
  [[ -z "$("${local_dc[@]}" ps -q broker)" ]]
  [[ "$(http_status local web http://127.0.0.1:7777/)" == "200" ]]
  "${local_dc[@]}" up -d --wait broker

  local web_id
  web_id="$("${local_dc[@]}" ps -q web)"
  local data_rw
  data_rw="$(docker inspect "$web_id" --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.RW}}{{end}}{{end}}')"
  [[ "$data_rw" == "false" ]]
  if "${local_dc[@]}" exec -T web sh -c 'touch /data/web-must-not-write' >/dev/null 2>&1; then
    echo "web unexpectedly wrote to its read-only /data mount" >&2
    return 1
  fi
  "${local_dc[@]}" exec -T web sh -c 'test ! -e /config/crate/config.yaml'

  local push_output
  push_output="$(
    tar -C "$repo_root/testdata/sites/welcome" -cf - . |
      "${local_dc[@]}" exec -T broker crate push - docker-local --expires never
  )"
  grep -Fq "http://web:7777/docker-local/" <<<"$push_output"
  "${local_dc[@]}" exec -T web wget -qO- http://127.0.0.1:7777/docker-local/ |
    grep -Fq "crate-html"
  [[ "$(http_status local web http://127.0.0.1:7777/api/status)" == "404" ]]
  [[ "$(http_status local broker http://127.0.0.1:7777/docker-local/)" == "404" ]]

  tar -C "$repo_root/testdata/sites/status" -cf - . |
    "${local_dc[@]}" exec -T broker crate push - docker-local --expires never >/dev/null
  "${local_dc[@]}" exec -T web wget -qO- http://127.0.0.1:7777/docker-local/ |
    grep -Fq "<h1>Status</h1>"

  tar -C "$repo_root/testdata/sites/welcome" -cf - . |
    "${local_dc[@]}" exec -T broker crate push - docker-local-expiry --expires 2s >/dev/null
  "${local_dc[@]}" stop broker
  "${local_dc[@]}" exec -T web test -d /data/crate/sites/docker-local-expiry
  sleep 2.1
  [[ "$(http_status local web http://127.0.0.1:7777/docker-local-expiry/)" == "404" ]]
  "${local_dc[@]}" start broker
  for _ in $(seq 1 100); do
    if "${local_dc[@]}" exec -T web test ! -d /data/crate/sites/docker-local-expiry; then
      break
    fi
    sleep 0.1
  done
  "${local_dc[@]}" exec -T web test ! -d /data/crate/sites/docker-local-expiry

  "${local_dc[@]}" exec -T broker crate rm docker-local >/dev/null
  [[ "$(http_status local web http://127.0.0.1:7777/docker-local/)" == "404" ]]
}

run_s3() {
  echo "docker e2e: split S3/rustfs topology"
  "${s3_dc[@]}" up -d --wait

  local push_output
  push_output="$(
    tar -C "$repo_root/testdata/sites/welcome" -cf - . |
      "${s3_dc[@]}" exec -T broker crate push - docker-s3 --expires never
  )"
  grep -Fq "http://web:7777/docker-s3/" <<<"$push_output"
  wait_for_status s3 web http://127.0.0.1:7777/docker-s3/ 200
  "${s3_dc[@]}" exec -T web wget -qO- http://127.0.0.1:7777/docker-s3/ |
    grep -Fq "crate-html"
  [[ "$(http_status s3 web http://127.0.0.1:7777/docker-s3/only-v1.txt)" == "200" ]]
  [[ "$(http_status s3 web http://127.0.0.1:7777/api/status)" == "404" ]]
  [[ "$(http_status s3 broker http://127.0.0.1:7777/docker-s3/)" == "404" ]]

  tar -C "$repo_root/testdata/sites/status" -cf - . |
    "${s3_dc[@]}" exec -T broker crate push - docker-s3 --expires never >/dev/null
  wait_for_body s3 web http://127.0.0.1:7777/docker-s3/ "<h1>Status</h1>"
  [[ "$(http_status s3 web http://127.0.0.1:7777/docker-s3/only-v1.txt)" == "404" ]]

  local named_token
  named_token="$("${s3_dc[@]}" exec -T broker crate token create docker-e2e 2>/dev/null)"

  "${s3_dc[@]}" --profile tools run --rm --no-deps iam -c '
    set -eu
    mc alias set web http://rustfs:9000 crateweb web-e2e-secret
    mc ls web/crate/meta/ >/dev/null
    mc stat web/crate/meta/docker-s3.json >/dev/null
    if mc cat web/crate/state/tokens.yaml >/dev/null; then
      echo "readonly web identity unexpectedly read broker token state" >&2
      exit 1
    fi
    if echo denied | mc pipe web/crate/web-must-not-put; then
      echo "readonly web identity unexpectedly put an object" >&2
      exit 1
    fi
    if mc rm web/crate/meta/docker-s3.json; then
      echo "readonly web identity unexpectedly deleted an object" >&2
      exit 1
    fi
  '
  wait_for_status s3 web http://127.0.0.1:7777/docker-s3/ 200

  "${s3_dc[@]}" restart broker
  "${s3_dc[@]}" exec -T -e CRATE_TOKEN="$named_token" broker crate ls |
    grep -Fq "docker-s3"

  "${s3_dc[@]}" exec -T broker crate rm docker-s3 >/dev/null
  wait_for_status s3 web http://127.0.0.1:7777/docker-s3/ 404
}

case "$mode" in
  all)
    run_local
    run_s3
    ;;
  local)
    run_local
    ;;
  s3)
    run_s3
    ;;
  *)
    echo "usage: $0 [all|local|s3]" >&2
    exit 2
    ;;
esac

echo "docker e2e: passed ($mode)"
