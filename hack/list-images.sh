#!/bin/bash

set -o errexit -o nounset -o pipefail

IMAGES=(
  "docker.io/envoyproxy/envoy:v1.30.1"
  "docker.io/kindest/haproxy:v20230606-42a2262b"
)

for image in "${IMAGES[@]}"; do
  echo "${image}"
done
