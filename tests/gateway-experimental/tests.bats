#!/usr/bin/env bats

# GEP-1494 (HTTP Auth) end to end tests. They need the experimental Gateway API
# channel because the ExternalAuth filter is not part of the standard CRDs.

gateway_ip() {
    local name="$1" ip i
    for ((i = 0; i < 30; i++)); do
        ip=$(kubectl get gateway "$name" -o jsonpath='{.status.addresses[0].value}' 2>/dev/null)
        [[ -n "$ip" ]] && echo "$ip" && return 0
        sleep 1
    done
    echo "Timeout waiting for an address on gateway/$name" >&2
    return 1
}

# http_status retries until the gateway answers, then echoes the status code.
http_status() {
    local url="$1"
    shift
    local code i
    for ((i = 0; i < 30; i++)); do
        code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$@" "$url" || true)
        [[ "$code" != "000" ]] && echo "$code" && return 0
        sleep 1
    done
    echo "000"
}

# apply_multi_auth brings up two authorization servers, a backend and the routes
# that use them, then waits for every pod to be ready.
apply_multi_auth() {
    kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/multi_auth.yaml
    kubectl wait --for=condition=ready pods -l app=MyApp --timeout=120s
    kubectl wait --for=condition=ready pods -l app=AuthzA --timeout=120s
    kubectl wait --for=condition=ready pods -l app=AuthzB --timeout=120s
}

delete_multi_auth() {
    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/manifests/multi_auth.yaml
}

# wait_for_feature waits until the GatewayClass advertises a feature. The name is
# matched exactly, because the GEP-1494 names are prefixes of each other.
wait_for_feature() {
    local name="$1" got i
    for ((i = 0; i < 30; i++)); do
        got=$(kubectl get gatewayclass cloud-provider-kind \
            -o "jsonpath={.status.supportedFeatures[?(@.name==\"$name\")].name}" 2>/dev/null)
        [[ "$got" == "$name" ]] && return 0
        sleep 1
    done
    echo "Timeout: GatewayClass does not advertise feature $name" >&2
    return 1
}

# ---------------------------------------------------------------------------

@test "ExternalAuth filter denies unauthenticated requests and allows authenticated ones" {
    kubectl apply -f "$BATS_TEST_DIRNAME"/../../examples/gateway_external_auth.yaml

    kubectl wait --for=condition=ready pods -l app=MyApp --timeout=120s
    kubectl wait --for=condition=ready pods -l app=Authz --timeout=120s

    IP=$(gateway_ip prod-web)
    echo "Gateway IP: $IP"

    # No Authorization header: the authorization server rejects the request and
    # it must never reach the backend.
    run http_status "http://${IP}/hostname"
    [ "$output" = "401" ]

    # With an Authorization header the request is authorized and forwarded.
    run http_status "http://${IP}/hostname" -H 'Authorization: Bearer token'
    [ "$output" = "200" ]

    POD=$(kubectl get pod -l app=MyApp -o jsonpath='{.items[0].metadata.name}')
    HOSTNAME=$(curl -s --connect-timeout 5 -H 'Authorization: Bearer token' "http://${IP}/hostname")
    [ "$HOSTNAME" = "$POD" ]

    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/../../examples/gateway_external_auth.yaml
}

@test "ExternalAuth filter with an unresolvable backendRef fails closed" {
    kubectl apply -f "$BATS_TEST_DIRNAME"/manifests/unresolvable_auth_backend.yaml

    for i in {1..60}; do
        REASON=$(kubectl get httproute test-route-extauth \
            -o 'jsonpath={.status.parents[0].conditions[?(@.type=="ResolvedRefs")].reason}' 2>/dev/null)
        [[ "$REASON" == "BackendNotFound" ]] && break
        sleep 1
    done

    run kubectl get httproute test-route-extauth \
        -o 'jsonpath={.status.parents[0].conditions[?(@.type=="ResolvedRefs")].status}'
    [ "$output" = "False" ]

    run kubectl get httproute test-route-extauth \
        -o 'jsonpath={.status.parents[0].conditions[?(@.type=="ResolvedRefs")].reason}'
    [ "$output" = "BackendNotFound" ]

    IP=$(gateway_ip test-gw-extauth)
    run http_status "http://${IP}/"
    [ "$output" = "500" ]

    kubectl delete --ignore-not-found -f "$BATS_TEST_DIRNAME"/manifests/unresolvable_auth_backend.yaml
}

@test "GatewayClass advertises the GEP-1494 features on the experimental channel" {
    wait_for_feature HTTPRouteExternalAuth
    wait_for_feature HTTPRouteExternalAuthHTTP
    wait_for_feature HTTPRouteExternalAuthGRPC
    wait_for_feature HTTPRouteExternalAuthForwardBody
}

@test "each route uses its own authorization server and unprotected routes are untouched" {
    apply_multi_auth
    IP=$(gateway_ip multi-auth-gw)
    echo "Gateway IP: $IP"

    # route-a only accepts an Authorization header.
    run http_status "http://${IP}/hostname" -H 'Host: a.auth.test'
    [ "$output" = "401" ]
    run http_status "http://${IP}/hostname" -H 'Host: a.auth.test' -H 'Authorization: Bearer token'
    [ "$output" = "200" ]
    # The credential accepted by route-a must not unlock route-b.
    run http_status "http://${IP}/hostname" -H 'Host: b.auth.test' -H 'Authorization: Bearer token'
    [ "$output" = "403" ]

    # route-b only accepts an API key, and returns its own denial status.
    run http_status "http://${IP}/hostname" -H 'Host: b.auth.test'
    [ "$output" = "403" ]
    run http_status "http://${IP}/hostname" -H 'Host: b.auth.test' -H 'X-Api-Key: secret'
    [ "$output" = "200" ]

    # route-open has no ExternalAuth filter, so the ext_authz filters installed
    # on the shared connection manager must stay disabled for it.
    run http_status "http://${IP}/hostname" -H 'Host: open.auth.test'
    [ "$output" = "200" ]

    delete_multi_auth
}

@test "headers from the authorization response are forwarded to the backend" {
    apply_multi_auth
    IP=$(gateway_ip multi-auth-gw)

    run http_status "http://${IP}/hostname" -H 'Host: a.auth.test' -H 'Authorization: Bearer token'
    [ "$output" = "200" ]

    # agnhost echoes back the value of the requested header, which proves the
    # authorization server response header reached the backend.
    for i in {1..15}; do
        USER=$(curl -s --connect-timeout 5 -H 'Host: a.auth.test' -H 'Authorization: Bearer token' \
            "http://${IP}/header?key=X-Authenticated-User" || true)
        [[ -n "$USER" ]] && break
        sleep 1
    done
    [ "$USER" = "alice" ]

    # route-b does not allow any response header through.
    run http_status "http://${IP}/hostname" -H 'Host: b.auth.test' -H 'X-Api-Key: secret'
    [ "$output" = "200" ]
    USER_B=$(curl -s --connect-timeout 5 -H 'Host: b.auth.test' -H 'X-Api-Key: secret' \
        "http://${IP}/header?key=X-Authenticated-User" || true)
    [ -z "$USER_B" ]

    delete_multi_auth
}
