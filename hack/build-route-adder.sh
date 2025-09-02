#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
REPO_ROOT=$( cd -- "$SCRIPT_DIR/.." &> /dev/null && pwd )

cd "$REPO_ROOT"

export CGO_ENABLED=0

# Compile for amd64 (x64)
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ./pkg/gateway/routeadder/route-adder-amd64 ./internal/routeadder/

# Compile for arm64
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o ./pkg/gateway/routeadder/route-adder-arm64 ./internal/routeadder/

# Cleanup is handled by the trap
exit 0
