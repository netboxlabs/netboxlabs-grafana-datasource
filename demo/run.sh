#!/usr/bin/env bash
# One-command demo runner.
#   ./demo/run.sh                          -> FULL mode: bundles a seeded real NetBox
#   NETBOX_URL=… NETBOX_TOKEN=… ./demo/run.sh -> BYO mode: uses your NetBox (fast)
#   ./demo/run.sh down [-v]                -> tear the demo down (see the down block below)
# Extra args are passed to `docker compose up` (e.g. -d).
set -euo pipefail
cd "$(dirname "$0")/.."

if ! docker info >/dev/null 2>&1; then
  echo "== Docker isn't running — start Docker Desktop (or your Docker daemon) and re-run this script ==" >&2
  exit 1
fi

# Teardown. Uses the same compose files `up` does, so down / -v / --rmi target exactly
# the demo's containers, seeded volume, and built image (all declared in these files,
# rather than relying on Compose's project-label scoping). The NETBOX_URL/TOKEN fallbacks
# only satisfy the base file's ${VAR:?} interpolation — `down` never uses their values,
# and a real value already in the environment is honored — so no env setup is required:
#   ./demo/run.sh down                -> stop and remove the demo containers; KEEP the
#                                        seeded NetBox data (so a re-run skips seeding)
#   ./demo/run.sh down -v             -> also remove the seeded-data volume (clean slate)
#   ./demo/run.sh down -v --rmi local -> also drop the built NetBox image (~1 GB)
if [ "${1:-}" = "down" ]; then
  shift
  echo "== tearing down the demo: docker compose down $* =="
  export NETBOX_URL="${NETBOX_URL:-x}" NETBOX_TOKEN="${NETBOX_TOKEN:-x}"
  exec docker compose -f demo/docker-compose.yaml -f demo/docker-compose.full.yaml down "$@"
fi

# Rebuild the plugin when its build artifacts are missing OR older than any
# source file. dist/ is gitignored, so a plain re-run after `git pull` otherwise
# reuses the previous build — and since Grafana bind-mounts dist/, it keeps
# serving the OLD plugin (a common "I pulled but nothing changed" surprise).
plugin_stale() {
  # missing artifacts?
  [ -f dist/module.js ] && [ -f dist/gpx_netbox_linux_amd64 ] && [ -f dist/gpx_netbox_linux_arm64 ] || return 0
  # frontend source newer than the built bundle?
  [ -n "$(find src package.json package-lock.json -newer dist/module.js 2>/dev/null | head -n1)" ] && return 0
  # backend source newer than the built binary?
  [ -n "$(find pkg go.mod go.sum -newer dist/gpx_netbox_linux_amd64 2>/dev/null | head -n1)" ] && return 0
  return 1
}

if plugin_stale; then
  echo "== building plugin (frontend + linux amd64/arm64 backend) =="
  if [ ! -d node_modules ]; then
    npm install
  fi
  npm run build
  if command -v mage >/dev/null 2>&1; then
    mage build:linux
    mage build:linuxARM64
  else
    # The image tag is READ FROM go.mod rather than pinned to a minor series.
    # `golang:1.26` tracks the newest 1.26.x, which is not necessarily new
    # enough: the official golang images set GOTOOLCHAIN=local, so when go.mod
    # asks for a patch release the image has not caught up to, Go refuses to
    # fetch one and the build dies with "go.mod requires go >= X (running Y)".
    # That is exactly what a go.mod patch bump for a stdlib advisory did here.
    # Reading the directive means a future bump moves this with it.
    #
    # GOTOOLCHAIN=auto is the belt to that braces: if the exact tag is ever
    # missing (a bump landing before the image is published, or a go.mod that
    # names only "1.26"), Go downloads the toolchain it needs instead of failing.
    go_ver="$(awk '/^go /{print $2; exit}' go.mod)"
    img="golang:${go_ver}"
    if ! docker image inspect "$img" >/dev/null 2>&1 && ! docker manifest inspect "$img" >/dev/null 2>&1; then
      echo "== no $img image published yet — using golang:${go_ver%.*} and letting Go fetch $go_ver =="
      img="golang:${go_ver%.*}"
    fi
    echo "== mage not found — building backend in a $img container instead =="
    for arch in amd64 arm64; do
      # CGO_ENABLED=0: Grafana's own image is musl-based (Alpine), so a
      # dynamically-linked glibc binary fails fork/exec with a misleading
      # "no such file or directory" (the missing piece is the ELF interpreter,
      # not the binary). Static linking sidesteps libc entirely.
      # The host module cache is mounted when there is one. Without it every
      # build downloads the whole dependency set from inside the container, and
      # container egress is materially slower than the host's on Docker Desktop
      # — slow enough that the fetch times out rather than merely dragging.
      # With it the build is offline and quick. Absent (fresh clone, CI), the
      # mount is skipped and Go fetches as before.
      gomodcache="$(go env GOMODCACHE 2>/dev/null || true)"
      cache_mount=""
      [ -n "$gomodcache" ] && [ -d "$gomodcache" ] && cache_mount="-v $gomodcache:/go/pkg/mod"
      # shellcheck disable=SC2086 # cache_mount is deliberately word-split
      docker run --rm -v "$PWD":/src -w /src $cache_mount \
        -e GOOS=linux -e GOARCH="$arch" -e CGO_ENABLED=0 -e GOTOOLCHAIN=auto \
        "$img" go build -o "dist/gpx_netbox_linux_$arch" ./pkg
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
