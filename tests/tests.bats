#!/usr/bin/env bats

setup_file() {
    REPO_ROOT="$( cd "$(dirname "$BATS_TEST_FILENAME")/.." >/dev/null 2>&1 && pwd )"
    cd $REPO_ROOT
    # install cloud-provider-kind
    make
    TMP_DIR=$(mktemp -d)
    export TMP_DIR
    # install `kind` and `kubectl` to tempdir
    curl -sLo "${TMP_DIR}/kubectl" "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
    chmod +x "${TMP_DIR}/kubectl"
    curl -sLo "${TMP_DIR}/kind" https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64
    chmod +x "${TMP_DIR}/kind"

    PATH="${TMP_DIR}:${PATH}"
    export PATH
    # create cluster
    kind create cluster --name ccm --wait 1m
    kind get kubeconfig --name ccm > "${TMP_DIR}/kubeconfig"
    kubectl --kubeconfig "${TMP_DIR}/kubeconfig" wait --for=condition=ready pods --namespace=kube-system -l k8s-app=kube-dns --timeout=3m
    kubectl --kubeconfig "${TMP_DIR}/kubeconfig" label node ccm-control-plane node.kubernetes.io/exclude-from-external-load-balancers-
    # run cloud-provider-kind
    nohup bin/cloud-provider-kind --cloud-provider kind --kubeconfig "${TMP_DIR}/kubeconfig" --cluster-name ccm --controllers "*" --v 5 --leader-elect=false > ${TMP_DIR}/ccm-kind.log 2>&1 &
    CLOUD_PROVIDER_KIND_PID=$(echo $!)
}

teardown_file() {
    kill -9 ${CLOUD_PROVIDER_KIND_PID}
    kind delete cluster --name ccm || true

    if [[ -n "${TMP_DIR:-}" ]]; then
        rm -rf "${TMP_DIR}"
    fi
}

@test "ExternalTrafficPolicy: Local" {
    kubectl --kubeconfig "${TMP_DIR}/kubeconfig" apply -f examples/loadbalancer_etp_local.yaml
    kubectl --kubeconfig "${TMP_DIR}/kubeconfig" get services lb-service-local --output jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

@test "ExternalTrafficPolicy: Cluster" {
    kubectl --kubeconfig "${TMP_DIR}/kubeconfig" apply -f examples/loadbalancer_etp_cluster.yaml
    kubectl --kubeconfig "${TMP_DIR}/kubeconfig" get services lb-service-cluster --output jsonpath='{.status.loadBalancer.ingress[0].ip}'
}
