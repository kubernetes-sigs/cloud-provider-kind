#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT=$(dirname "${BASH_SOURCE[0]}")/..

cd $REPO_ROOT
docker run --rm -v $(pwd):/app -w /app golangci/golangci-lint:v1.62.2 golangci-lint run -v
