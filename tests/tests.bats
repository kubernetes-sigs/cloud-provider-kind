#!/usr/bin/env bats

@test "ExternalTrafficPolicy: Local" {
    kubectl apply -f "$BATS_TEST_DIRNAME"/../examples/loadbalancer_etp_local.yaml
    kubectl wait --for=condition=ready pods -l app=MyLocalApp
    for i in {1..5}
    do
        IP=$(kubectl get services lb-service-local --output jsonpath='{.status.loadBalancer.ingress[0].ip}')
        [[ ! -z "$IP" ]] && break || sleep 1
    done
    echo "IP: $IP"
    POD=$(kubectl get pod -l app=MyLocalApp -o jsonpath='{.items[0].metadata.name}')
    echo "Pod $POD"
    for i in {1..5}
    do
        HOSTNAME=$(curl -s http://${IP}:80/hostname || true)
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
    echo "Hostname via TCP: $HOSTNAME"
    [  "$HOSTNAME" = "$POD" ]
    kubectl delete -f "$BATS_TEST_DIRNAME"/../examples/loadbalancer_etp_local.yaml
}

@test "ExternalTrafficPolicy: Cluster" {
    kubectl apply -f "$BATS_TEST_DIRNAME"/../examples/loadbalancer_etp_cluster.yaml
    kubectl wait --for=condition=ready pods -l app=MyClusterApp
    for i in {1..5}
    do
        IP=$(kubectl get services lb-service-cluster --output jsonpath='{.status.loadBalancer.ingress[0].ip}')
        [[ ! -z "$IP" ]] && break || sleep 1
    done
    echo "IP: $IP"
    POD=$(kubectl get pod -l app=MyClusterApp -o jsonpath='{.items[0].metadata.name}')
    echo "Pod $POD"
    for i in {1..5}
    do
        HOSTNAME=$(curl -s http://${IP}:80/hostname || true)
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
    echo "Hostname via TCP: $HOSTNAME"
    [  "$HOSTNAME" = "$POD" ]
    kubectl delete -f "$BATS_TEST_DIRNAME"/../examples/loadbalancer_etp_cluster.yaml
}

@test "Multiple Protocols: UDP and TCP" {
    kubectl apply -f "$BATS_TEST_DIRNAME"/../examples/loadbalancer_udp_tcp.yaml
    kubectl wait --for=condition=ready pods -l app=multiprotocol
    for i in {1..5}
    do
        IP=$(kubectl get services multiprotocol --output jsonpath='{.status.loadBalancer.ingress[0].ip}')
        [[ ! -z "$IP" ]] && break || sleep 1
    done
    echo "IP: $IP"
    POD=$(kubectl get pod -l app=multiprotocol -o jsonpath='{.items[0].metadata.name}')
    echo "Pod $POD"
    for i in {1..5}
    do
        HOSTNAME=$(curl -s http://${IP}:80/hostname || true)
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
    echo "Hostname via TCP: $HOSTNAME"
    [  "$HOSTNAME" = "$POD" ]

    for i in {1..5}
    do
        HOSTNAME=$(echo hostname | nc -u -w 3 ${IP} 80 || true)
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
    echo "Hostname via UDP: $HOSTNAME"
    [[ ! -z "$HOSTNAME" ]] && [  "$HOSTNAME" = "$POD" ]
    kubectl delete -f "$BATS_TEST_DIRNAME"/../examples/loadbalancer_udp_tcp.yaml
}

@test "Simple Gateway" {
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


@test "Ingress to Gateway Migration and X-Forwarded-For Header" {
    # Apply the Gateway and HTTPRoute manifests
    kubectl apply -f "$BATS_TEST_DIRNAME"/../examples/ingress_foo_bar.yaml

    # Wait for the backend application pod to be ready
    kubectl wait --for=condition=ready pods -l app=foo --timeout=60s
    kubectl wait --for=condition=ready pods -l app=foo --timeout=60s

    # Give the controller time to reconcile
    echo "Waiting for reconciliation..."
    sleep 5

    echo "Finding Ingress Loadbalancer IP ..."
    run kubectl get ingress example-ingress -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
    [ "$status" -eq 0 ]
    export INGRESS_SVC_IP="$output"
    echo "Ingress LoadBalancer IP: $INGRESS_SVC_IP"

    # Test /foo prefix
    echo "Testing /foo prefix (should match foo-app)..."
    run kubectl exec curl-pod -- curl -H "Host: foo.example.com" -s "http://$INGRESS_SVC_IP/hostname"
    [ "$status" -eq 0 ]
    [[ "$output" == "foo-app" ]]

    # Test /bar prefix
    echo "Testing /bar prefix (should match bar-app)..."
    run kubectl exec curl-pod -- curl  -H "Host: bar.example.com" -s "http://$INGRESS_SVC_IP/hostname"
    [ "$status" -eq 0 ]
    [[ "$output" == "bar-app" ]]

    # Test X-Forwarded-For header
    echo "Testing X-Forwarded-For header..."
    run kubectl exec curl-pod -- curl -H "Host: foo.example.com" -s "http://$INGRESS_SVC_IP/header?key=X-Forwarded-For"
    [ "$status" -eq 0 ]
    echo "X-Forwarded-For header value: $output"
    [[ ! -z "$output" ]]

    # Cleanup: Delete the applied manifests
    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/../examples/ingress_foo_bar.yaml
}

@test "Ingress WebSocket Support" {
    # 1. Deploy a WebSocket-capable echo server
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ws-echo
  labels:
    app: ws-echo
spec:
  selector:
    matchLabels:
      app: ws-echo
  template:
    metadata:
      labels:
        app: ws-echo
    spec:
      containers:
      - name: echo
        image: jmalloc/echo-server
        ports:
        - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: ws-service
spec:
  ports:
  - port: 80
    targetPort: 8080
  selector:
    app: ws-echo
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ws-ingress
spec:
  rules:
  - host: ws.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: ws-service
            port:
              number: 80
EOF

    # 2. Wait for the pod to be ready
    kubectl wait --for=condition=ready pod -l app=ws-echo --timeout=60s

    # 3. Get Ingress IP
    echo "Waiting for Ingress IP..."
    for i in {1..30}; do
        IP=$(kubectl get ingress ws-ingress -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
        [[ ! -z "$IP" ]] && break || sleep 1
    done
    echo "Ingress IP: $IP"
    [[ ! -z "$IP" ]]

    # 4. Verify WebSocket Upgrade from the HOST (with retry)
    echo "Verifying WebSocket handshake..."
    FOUND=0
    for i in {1..10}; do
        # Use --max-time 3 to prevent hanging if the IP is not yet reachable
        OUTPUT=$(curl -i -N -s --max-time 3 \
            -H "Host: ws.example.com" \
            -H "Connection: Upgrade" \
            -H "Upgrade: websocket" \
            -H "Sec-WebSocket-Key: SGVsbG8sIHdvcmxkIQ==" \
            -H "Sec-WebSocket-Version: 13" \
            "http://$IP/" || true)
        
        if [[ "$OUTPUT" == *"101 Switching Protocols"* ]]; then
            echo "Success: 101 Switching Protocols found"
            FOUND=1
            break
        fi
        echo "Attempt $i failed or not ready. Retrying..."
        sleep 1
    done

    if [ "$FOUND" -eq 0 ]; then
        echo "Failed to establish WebSocket connection. Last output:"
        echo "$OUTPUT"
        return 1
    fi

    # Cleanup
    kubectl delete deployment ws-echo
    kubectl delete service ws-service
    kubectl delete ingress ws-ingress
}