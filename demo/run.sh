#!/usr/bin/env bash
# One-command demo runner.
#   ./demo/run.sh                          -> FULL mode: bundles a seeded real NetBox
#   NETBOX_URL=… NETBOX_TOKEN=… ./demo/run.sh -> BYO mode: uses your NetBox (fast)
# Extra args are passed to `docker compose up` (e.g. -d).
set -euo pipefail
cd "$(dirname "$0")/.."

if ! docker info >/dev/null 2>&1; then
  echo "== Docker isn't running — start Docker Desktop (or your Docker daemon) and re-run this script ==" >&2
  exit 1
fi

if [ ! -f dist/module.js ] || [ ! -f dist/gpx_netbox_linux_amd64 ] || [ ! -f dist/gpx_netbox_linux_arm64 ]; then
  echo "== building plugin (frontend + linux amd64/arm64 backend) =="
  if [ ! -d node_modules ]; then
    npm install
  fi
  npm run build
  if command -v mage >/dev/null 2>&1; then
    mage build:linux
    mage build:linuxARM64
  else
    echo "== mage not found — building backend in a golang:1.26 container instead =="
    for arch in amd64 arm64; do
      # CGO_ENABLED=0: Grafana's own image is musl-based (Alpine), so a
      # dynamically-linked glibc binary fails fork/exec with a misleading
      # "no such file or directory" (the missing piece is the ELF interpreter,
      # not the binary). Static linking sidesteps libc entirely.
      docker run --rm -v "$PWD":/src -w /src -e GOOS=linux -e GOARCH="$arch" -e CGO_ENABLED=0 \
        golang:1.26 go build -o "dist/gpx_netbox_linux_$arch" ./pkg
    done
  fi
fi
if [ -n "${NETBOX_URL:-}" ]; then
  : "${NETBOX_TOKEN:?set NETBOX_TOKEN alongside NETBOX_URL for BYO mode}"
  echo "== starting BYO demo against $NETBOX_URL (Grafana on :3001) =="
  exec docker compose -f demo/docker-compose.yaml up "$@"
fi
# The bundled-NetBox coordinates. Exported so the BASE file's ${NETBOX_URL:?}/
# ${NETBOX_TOKEN:?} interpolate (compose interpolates per-file BEFORE merge, so the
# override alone doesn't satisfy them). The override also sets them on services.
export NETBOX_URL="http://netbox:8080"
export NETBOX_TOKEN="0123456789abcdef0123456789abcdef01234567"
# Deep links must open on the published host port, not the internal netbox:8080.
export NETBOX_PUBLIC_URL="${NETBOX_PUBLIC_URL:-http://localhost:8000}"
echo "== starting full demo (bundled NetBox; first boot seeds, ~2-3 min) =="
exec docker compose -f demo/docker-compose.yaml -f demo/docker-compose.full.yaml up "$@"
