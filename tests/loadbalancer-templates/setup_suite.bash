#!/bin/bash

set -eu

function setup_suite {
  export BATS_TEST_TIMEOUT=120
  # Define the name of the kind cluster
  export CLUSTER_NAME="ccm-kind-lb-templates"

  export ARTIFACTS_DIR="$BATS_TEST_DIRNAME"/../../_artifacts-loadbalancer-templates
  mkdir -p "$ARTIFACTS_DIR"
  rm -rf "$ARTIFACTS_DIR"/*

  # Clean up any leftover cluster from a previous run
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true

  # create cluster
  kind create cluster --name $CLUSTER_NAME -v7 --wait 1m --retain --config="$BATS_TEST_DIRNAME/../kind.yaml"

  # build & run cloud-provider-kind with the PROXY protocol example templates
  cd "$BATS_TEST_DIRNAME"/../.. && make
  nohup "$BATS_TEST_DIRNAME"/../../bin/cloud-provider-kind -v 2 --enable-log-dumping --logs-dir "$ARTIFACTS_DIR" \
    --loadbalancer-config-dir "$BATS_TEST_DIRNAME"/../../examples/loadbalancer-templates/proxy-protocol \
    > "$ARTIFACTS_DIR"/ccm-kind.log 2>&1 &
  export CCM_PID=$!

  # test depend on external connectivity that can be very flaky
  sleep 5
}

function teardown_suite {
    kill "${CCM_PID:-}" 2>/dev/null || true
    kind export logs "$ARTIFACTS_DIR" --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
}
