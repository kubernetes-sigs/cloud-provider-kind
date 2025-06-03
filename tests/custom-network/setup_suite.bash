#!/bin/bash

set -eu

function setup_suite {
  export BATS_TEST_TIMEOUT=120
  # Define the name of the kind cluster
  export CLUSTER_NAME="ccm-kind-custom-network"

  export ARTIFACTS_DIR="$BATS_TEST_DIRNAME"/../../_artifacts-custom-network
  mkdir -p "$ARTIFACTS_DIR"
  rm -rf "$ARTIFACTS_DIR"/*

  # create custom docker network
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=kind-static
  docker network create \
  --driver=bridge \
  --subnet=172.20.0.0/16 $KIND_EXPERIMENTAL_DOCKER_NETWORK

  # create cluster
  cat <<EOF | kind create cluster \
  --name $CLUSTER_NAME           \
  -v7 --wait 1m --retain --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
EOF

  # build & run cloud-provider-kind
  cd "$BATS_TEST_DIRNAME"/../.. && make
  nohup "$BATS_TEST_DIRNAME"/../../bin/cloud-provider-kind -v 2 --enable-log-dumping --logs-dir "$ARTIFACTS_DIR" > "$ARTIFACTS_DIR"/ccm-kind.log 2>&1 &
  export CCM_PID=$!

  # test depend on external connectivity that can be very flaky
  sleep 5
}

function teardown_suite {
    kill "$CCM_PID"
    kind export logs "$ARTIFACTS_DIR" --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
    docker network rm "$KIND_EXPERIMENTAL_DOCKER_NETWORK"
}