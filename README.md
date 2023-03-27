# Kubernetes Cloud Provider for KIND

KIND has demonstrated to be a very versatile, efficient, cheap and very useful tool for Kubernetes testing.
However, KIND doesn't offer capabilities for testing all the features that depend on cloud-providers, causing a gap on testing.

cloud-provider-kind aim to fill this gap and provide an agnostic and cheap solution for all the Kubernetes features that depend on a cloud-provider using KIND

- [Slack channel](https://kubernetes.slack.com/messages/kind)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-testing)

## How to use it

Run a KIND cluster enabling the external cloud-provider

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
kubeadmConfigPatches:
- |
  kind: ClusterConfiguration
  apiServer:
    extraArgs:
      cloud-provider: "external"
      v: "5"
  controllerManager:
    extraArgs:
      cloud-provider: "external"
      v: "5"
  ---
  kind: InitConfiguration
  nodeRegistration:
    kubeletExtraArgs:
      cloud-provider: "external"
  ---
  kind: JoinConfiguration
  nodeRegistration:
    kubeletExtraArgs:
      cloud-provider: "external"
      v: "5"
nodes:
- role: control-plane
- role: worker
- role: worker
```

```sh
CLUSTER_NAME=test
kind create cluster --config kind.yaml --name ${CLUSTER_NAME}
```

Once the cluster is running, obtain the admin kubeconfig and use it to run the external cloud provider

```sh
CLUSTER_NAME=test
kind get kubeconfig --name ${CLUSTER_NAME} > bin/kubeconfig
bin/cloud-provider-kind --cloud-provider kind --kubeconfig $PWD/bin/kubeconfig --cluster-name ${CLUSTER_NAME} --controllers "*" --v 5 --leader-elect=false
```


**NOTE**

Only tested with docker

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
