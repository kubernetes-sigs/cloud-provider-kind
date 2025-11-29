#!/bin/bash

set -eu

function setup_suite {
  export BATS_TEST_TIMEOUT=120
  # Define the name of the kind cluster
  export CLUSTER_NAME="ccm-kind"

  export ARTIFACTS_DIR="$BATS_TEST_DIRNAME"/../_artifacts
  mkdir -p "$ARTIFACTS_DIR"
  rm -rf "$ARTIFACTS_DIR"/*

  # create cluster
  kind create cluster --name $CLUSTER_NAME -v7 --wait 1m --retain --config="$BATS_TEST_DIRNAME/kind.yaml"

  cd "$BATS_TEST_DIRNAME"/.. && make
  nohup "$BATS_TEST_DIRNAME"/../bin/cloud-provider-kind -v 2 --gateway-channel=standard --enable-log-dumping --logs-dir "$ARTIFACTS_DIR" > "$ARTIFACTS_DIR"/ccm-kind.log 2>&1 &
  export CCM_PID=$!

  # test depend on external connectivity that can be very flaky
  sleep 5
}

function teardown_suite {
    kill "$CCM_PID"
    kind export logs "$ARTIFACTS_DIR" --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
}