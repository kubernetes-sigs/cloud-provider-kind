#!/bin/bash

set -eu

function setup_suite {
  export BATS_TEST_TIMEOUT=300
  # Define the name of the kind cluster
  export CLUSTER_NAME="ccm-kind-gateway-experimental"

  export ARTIFACTS_DIR="$BATS_TEST_DIRNAME"/../../_artifacts-gateway-experimental
  mkdir -p "$ARTIFACTS_DIR"
  rm -rf "$ARTIFACTS_DIR"/*

  # Clean up any leftover cluster from a previous run
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true

  # create cluster
  kind create cluster --name $CLUSTER_NAME -v7 --wait 1m --retain --config="$BATS_TEST_DIRNAME/../kind.yaml"

  # The experimental channel is required by the Gateway API features still under
  # development, such as the ExternalAuth HTTPRoute filter (GEP-1494).
  cd "$BATS_TEST_DIRNAME"/../.. && make
  nohup "$BATS_TEST_DIRNAME"/../../bin/cloud-provider-kind -v 2 --gateway-channel=experimental --enable-lb-port-mapping --enable-log-dumping --logs-dir "$ARTIFACTS_DIR" > "$ARTIFACTS_DIR"/ccm-kind.log 2>&1 &
  export CCM_PID=$!

  # test depend on external connectivity that can be very flaky
  sleep 5
}

function teardown_suite {
    kill "${CCM_PID:-}" 2>/dev/null || true
    kind export logs "$ARTIFACTS_DIR" --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
}
