#!/usr/bin/env bats

@test "Gateway routes HTTP traffic to pod" {
    # Apply the Gateway and HTTPRoute manifests
    kubectl apply -f "$BATS_TEST_DIRNAME"/../examples/gateway_httproute_simple.yaml

    # Wait for the backend application pod to be ready
    kubectl wait --for=condition=ready pods -l app=MyApp --timeout=60s

    # Retry loop to get the Gateway's external IP address
    for i in {1..10}
    do
        # Fetch the IP address assigned by the load balancer to the Gateway
        IP=$(kubectl get gateway prod-web --output jsonpath='{.status.addresses[0].value}' 2>/dev/null)
        # Check if IP is not empty and break the loop if found
        [[ ! -z "$IP" ]] && break || sleep 1
    done
    # Fail the test if IP is still empty after retries
    if [[ -z "$IP" ]]; then
      echo "Failed to get Gateway IP address"
      return 1
    fi
    echo "Gateway IP: $IP"

    # Get the name of the backend pod
    POD=$(kubectl get pod -l app=MyApp -o jsonpath='{.items[0].metadata.name}')
    echo "Backend Pod: $POD"

    # Retry loop to curl the backend service through the Gateway IP
    for i in {1..10}
    do
        # Curl the /hostname endpoint via the Gateway IP, ignore failures temporarily
        HOSTNAME=$(curl -s --connect-timeout 5 http://${IP}:80/hostname || true)
        # Check if HOSTNAME is not empty and break the loop if successful
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
     # Fail the test if HOSTNAME is still empty after retries
    if [[ -z "$HOSTNAME" ]]; then
      echo "Failed to get hostname via Gateway"
      return 1
    fi
    echo "Hostname via Gateway (TCP): $HOSTNAME"

    # Assert that the hostname returned by the service matches the actual pod name
    [ "$HOSTNAME" = "$POD" ]

    # Cleanup: Delete the applied manifests
    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/../examples/gateway_httproute_simple.yaml
}

@test "Gateway with multiple HTTP Listeners on Same Port with enable-lb-port-mapping" {
    # Reproduces duplicate --publish=80/tcp causing Docker exit status 125.
    kubectl apply -f "$BATS_TEST_DIRNAME"/../examples/gateway_http_multi_listener.yaml

    # Wait for the backend application pod to be ready
    kubectl wait --for=condition=ready pods -l app=MultiListenerApp --timeout=60s

    # Retry loop to get the Gateway's external IP address
    for i in {1..10}
    do
        IP=$(kubectl get gateway multi-listener-gateway --output jsonpath='{.status.addresses[0].value}' 2>/dev/null)
        [[ ! -z "$IP" ]] && break || sleep 1
    done
    if [[ -z "$IP" ]]; then
      echo "Failed to get Gateway IP address"
      return 1
    fi
    echo "Gateway IP: $IP"

    # Verify the gateway container is reachable (if duplicates existed, container creation would have failed)
    for i in {1..10}
    do
        HOSTNAME=$(curl -s --connect-timeout 5 http://${IP}/hostname -H "Host: route.multi-listener.test" || true)
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
    if [[ -z "$HOSTNAME" ]]; then
      echo "Failed to get hostname via Gateway"
      return 1
    fi
    echo "Hostname via Gateway: $HOSTNAME"

    # Cleanup
    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/../examples/gateway_http_multi_listener.yaml
}
