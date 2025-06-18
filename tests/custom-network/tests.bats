#!/usr/bin/env bats

@test "Static LoadBalancerIP" {
    # the static IP must be part of the custom docker network - calculate the .100 IP from the custom kind IPv4 network
    SUBNET=$(docker network inspect $KIND_EXPERIMENTAL_DOCKER_NETWORK --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+')
    STATIC_IP=$(echo "$SUBNET" | awk -F'[./]' '{printf "%s.%s.%s.100", $1, $2, $3}')

    echo "Using static IP: $STATIC_IP"

    TMP_YAML=$(mktemp)
    sed "s/REPLACE_WITH_STATIC_IP/$STATIC_IP/g" "$BATS_TEST_DIRNAME"/../../examples/loadbalancer_static_ip.yaml > "$TMP_YAML"

    kubectl apply -f "$TMP_YAML"
    kubectl wait --for=condition=ready pods -l app=static-ip --timeout=60s

    for i in {1..5}
    do
        IP=$(kubectl get services static-ip --output jsonpath='{.status.loadBalancer.ingress[0].ip}')
        [[ ! -z "$IP" ]] && break || sleep 1
    done

    echo "Assigned IP: $IP"
    [ "$IP" = "$STATIC_IP" ]

    POD=$(kubectl get pod -l app=static-ip -o jsonpath='{.items[0].metadata.name}')
    echo "Pod $POD"
    for i in {1..5}
    do
        HOSTNAME=$(curl -s http://${IP}:80/hostname || true)
        [[ ! -z "$HOSTNAME" ]] && break || sleep 1
    done
    echo "Hostname via TCP: $HOSTNAME"
    [  "$HOSTNAME" = "$POD" ]

    kubectl delete -f "$TMP_YAML"
    rm "$TMP_YAML"
}