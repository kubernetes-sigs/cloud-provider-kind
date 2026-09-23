#!/usr/bin/env bats

# Both Services are backed by the same nginx pod. Port 80 expects the PROXY
# protocol header and returns the address carried in it, port 8080 returns the
# TCP peer address. Only the annotated Service gets the PROXY protocol
# transport socket from the example template.

setup_file() {
    kubectl apply -f "$BATS_TEST_DIRNAME"/../../examples/loadbalancer_proxy_protocol.yaml
    kubectl wait --for=condition=ready pods -l app=proxy-protocol --timeout=60s
}

teardown_file() {
    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/../../examples/loadbalancer_proxy_protocol.yaml
}

# lb_ip SERVICE: prints the first LoadBalancer IP of the Service, retrying while empty
lb_ip() {
    local ip=""
    for i in {1..10}; do
        ip=$(kubectl get services "$1" --output jsonpath='{.status.loadBalancer.ingress[0].ip}')
        [[ -n "$ip" ]] && break || sleep 1
    done
    echo "$ip"
}

# http_body IP: prints the body returned by the backend, retrying while empty
http_body() {
    local body=""
    for i in {1..10}; do
        body=$(curl -s --max-time 5 "http://$1:80/" || true)
        [[ -n "$body" ]] && break || sleep 1
    done
    echo "$body"
}

# host_src_ip IP: prints the address the host uses to reach IP
host_src_ip() {
    ip route get "$1" | awk '{for (i = 1; i <= NF; i++) if ($i == "src") print $(i + 1)}' | head -1
}

@test "Template override adds the PROXY protocol transport socket only to the annotated Service" {
    IP=$(lb_ip lb-proxy-protocol)
    [[ -n "$IP" ]]
    IP_PLAIN=$(lb_ip lb-no-proxy-protocol)
    [[ -n "$IP_PLAIN" ]]

    # the envoy admin endpoint listens on the container IP; ask only for the
    # clusters rendered from the CDS template, the full dump also lists the
    # extensions compiled into the binary
    run curl -s --max-time 5 "http://$IP:10000/config_dump?resource=dynamic_active_clusters"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cluster_IPv4_80_TCP"* ]]
    [[ "$output" == *"envoy.transport_sockets.upstream_proxy_protocol"* ]]

    run curl -s --max-time 5 "http://$IP_PLAIN:10000/config_dump?resource=dynamic_active_clusters"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cluster_IPv4_80_TCP"* ]]
    [[ "$output" != *"envoy.transport_sockets.upstream_proxy_protocol"* ]]
}

@test "Backend sees the client address through the PROXY protocol" {
    IP=$(lb_ip lb-proxy-protocol)
    [[ -n "$IP" ]]
    echo "IP: $IP"

    EXPECTED=$(host_src_ip "$IP")
    echo "Expected client address: $EXPECTED"
    [[ -n "$EXPECTED" ]]

    # a successful request also proves the health checks bypass the PROXY protocol,
    # otherwise all endpoints are unhealthy and Envoy does not forward anything
    CLIENT=$(http_body "$IP")
    echo "Client address seen by the backend: $CLIENT"
    [ "$CLIENT" = "$EXPECTED" ]
}

@test "Backend without PROXY protocol does not see the client address" {
    IP=$(lb_ip lb-no-proxy-protocol)
    [[ -n "$IP" ]]
    echo "IP: $IP"

    HOST_IP=$(host_src_ip "$IP")
    echo "Host: $HOST_IP"

    # the backend sees the Envoy container or, with externalTrafficPolicy Cluster,
    # the address kube-proxy masquerades with (the node pod-network address)
    CLIENT=$(http_body "$IP")
    echo "Client address seen by the backend: $CLIENT"
    [[ -n "$CLIENT" ]]
    [ "$CLIENT" != "$HOST_IP" ]
}
