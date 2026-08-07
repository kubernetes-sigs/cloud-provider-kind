#!/usr/bin/env bats

wait_for_gateway_condition() {
    local name="$1" ctype="$2" want="$3" timeout="${4:-60}"
    local got i
    for ((i = 0; i < timeout; i++)); do
        got=$(kubectl get gateway "$name" \
            -o "jsonpath={.status.conditions[?(@.type==\"$ctype\")].status}" 2>/dev/null)
        [[ "$got" == "$want" ]] && return 0
        sleep 1
    done
    echo "Timeout after ${timeout}s: gateway/$name condition $ctype never reached $want (last: '$got')" >&2
    return 1
}

# ---------------------------------------------------------------------------

@test "Gateway with infrastructure is neither Accepted nor Programmed" {
    kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gw-params
spec:
  gatewayClassName: cloud-provider-kind
  infrastructure:
    parametersRef:
      group: example.com
      kind: Config
      name: my-config
  listeners:
  - name: http
    port: 80
    protocol: HTTP
EOF

    wait_for_gateway_condition test-gw-params Accepted False 30

    run kubectl get gateway test-gw-params \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].status}'
    [ "$status" -eq 0 ]
    [ "$output" = "False" ]

    run kubectl get gateway test-gw-params \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].reason}'
    [ "$output" = "InvalidParameters" ]

    run kubectl get gateway test-gw-params \
        -o 'jsonpath={.status.conditions[?(@.type=="Programmed")].status}'
    [ "$output" = "False" ]

    # Envoy must not be programmed: no address should be assigned.
    run kubectl get gateway test-gw-params -o 'jsonpath={.status.addresses}'
    [ -z "$output" ]

    # No Envoy container was started
    run docker ps \
      --filter label=io.x-k8s.cloud-provider-kind.gateway.name=ccm-kind/default/test-gw-params \
      --format='{{.ID}}'
    [ -z "$output" ]

    kubectl delete gateway test-gw-params --ignore-not-found
}

@test "Gateway with unsupported protocol in neither Accepted nor Programmed" {
    kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gw-tcp-only
spec:
  gatewayClassName: cloud-provider-kind
  listeners:
  - name: tcp
    port: 9000
    protocol: TCP
EOF

    wait_for_gateway_condition test-gw-tcp-only Accepted False 30

    run kubectl get gateway test-gw-tcp-only \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].status}'
    [ "$output" = "False" ]

    run kubectl get gateway test-gw-tcp-only \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].reason}'
    [ "$output" = "ListenersNotValid" ]

    run kubectl get gateway test-gw-tcp-only \
        -o 'jsonpath={.status.conditions[?(@.type=="Programmed")].status}'
    [ "$output" = "False" ]

    run kubectl get gateway test-gw-tcp-only \
        -o 'jsonpath={.status.listeners[0].conditions[?(@.type=="Accepted")].status}'
    [ "$output" = "False" ]

    run kubectl get gateway test-gw-tcp-only \
        -o 'jsonpath={.status.listeners[0].conditions[?(@.type=="Accepted")].reason}'
    [ "$output" = "UnsupportedProtocol" ]

    # Envoy must not be programmed: no address should be assigned.
    run kubectl get gateway test-gw-tcp-only -o 'jsonpath={.status.addresses}'
    [ -z "$output" ]

    # No Envoy container was started
    run docker ps \
      --filter label=io.x-k8s.cloud-provider-kind.gateway.name=ccm-kind/default/test-gw-tcp-only \
      --format='{{.ID}}'
    [ -z "$output" ]

    kubectl delete gateway test-gw-tcp-only --ignore-not-found
}

@test "Gateway is Accepted with reason ListenersNotValid when some listeners are unsupported" {
    kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gw-mixed
spec:
  gatewayClassName: cloud-provider-kind
  listeners:
  - name: http
    port: 80
    protocol: HTTP
  - name: tcp
    port: 9000
    protocol: TCP
EOF

    wait_for_gateway_condition test-gw-mixed Accepted True 60

    run kubectl get gateway test-gw-mixed \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].status}'
    [ "$output" = "True" ]

    run kubectl get gateway test-gw-mixed \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].reason}'
    [ "$output" = "ListenersNotValid" ]

    kubectl delete gateway test-gw-mixed --ignore-not-found
}

@test "Gateway is Accepted and Programmed for a valid HTTP gateway" {
    kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gw-accepted
spec:
  gatewayClassName: cloud-provider-kind
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Same
EOF

    wait_for_gateway_condition test-gw-accepted Accepted True 60
    wait_for_gateway_condition test-gw-accepted Programmed True 120

    run kubectl get gateway test-gw-accepted \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].status}'
    [ "$output" = "True" ]

    run kubectl get gateway test-gw-accepted \
        -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].reason}'
    [ "$output" = "Accepted" ]

    run kubectl get gateway test-gw-accepted \
        -o 'jsonpath={.status.conditions[?(@.type=="Programmed")].status}'
    [ "$output" = "True" ]

    # Envoy container was started
    run docker ps \
      --filter label=io.x-k8s.cloud-provider-kind.gateway.name=ccm-kind/default/test-gw-accepted \
      --format='{{.ID}}'
    [ -n "$output" ]

    # Envoy is programmed ↔ the gateway has been assigned an external IP.
    for i in {1..30}; do
        IP=$(kubectl get gateway test-gw-accepted \
            -o jsonpath='{.status.addresses[0].value}' 2>/dev/null)
        [[ -n "$IP" ]] && break || sleep 2
    done
    echo "Gateway IP: $IP"
    [[ -n "$IP" ]]

    kubectl delete gateway test-gw-accepted --ignore-not-found
}

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
