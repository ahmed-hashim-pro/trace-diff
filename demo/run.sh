#!/usr/bin/env bash
# Runs the shop three times and keeps each run's traces:
#
#   out/before.jsonl  the code before the change
#   out/rerun.jsonl   the same code again: what "no change" looks like
#   out/after.jsonl   the change under review
#
# Each run starts fresh services and a fresh collector, sends the same 300
# requests, and stops everything so every span is flushed to disk.
set -euo pipefail
cd "$(dirname "$0")"
export HOST_UID=$(id -u) HOST_GID=$(id -g)
REQUESTS=${REQUESTS:-300}

mkdir -p out
rm -f out/before.jsonl out/rerun.jsonl out/after.jsonl
docker compose build --quiet
docker compose up -d --wait postgres

run() {
  local phase=$1 change=$2
  echo "== $phase (CHANGE=$change)"
  export PHASE=$phase CHANGE=$change
  docker compose up -d --force-recreate otelcol orders pricing frontend
  docker compose run --rm load load --url http://frontend:8080 --requests "$REQUESTS"
  # SIGTERM: the services flush their spans to the collector, then the
  # collector flushes its file.
  docker compose stop -t 20 frontend orders pricing
  docker compose stop -t 20 otelcol
  echo "   $(wc -l < out/$phase.jsonl) batches, $(grep -o '"spanId"' out/$phase.jsonl | wc -l) spans in out/$phase.jsonl"
}

run before before
run rerun before
run after after
docker compose down --remove-orphans
