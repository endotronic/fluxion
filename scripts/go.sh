#!/usr/bin/env bash
# Run the Go toolchain in a container. There is no local Go install on this
# machine; docker is the only route. Module and build caches are kept in named
# volumes so repeated runs are fast.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec docker run --rm \
  -v "$REPO":/src -w /src \
  -v fluxion-gomod:/go/pkg/mod \
  -v fluxion-gocache:/root/.cache/go-build \
  -e GOFLAGS=-buildvcs=false \
  golang:1.25 go "$@"
