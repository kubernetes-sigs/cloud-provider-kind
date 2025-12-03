### Allowing load balancers access to control plane nodes

By default, [Kubernetes expects workloads will not run on control plane nodes](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/#control-plane-node-isolation)
and labels them with [`node.kubernetes.io/exclude-from-external-load-balancers`](https://kubernetes.io/docs/reference/labels-annotations-taints/#node-kubernetes-io-exclude-from-external-load-balancers),
which stops load balancers from accessing them.

If you are running workloads on control plane nodes, as is the [default kind configuration](https://kind.sigs.k8s.io/docs/user/configuration/#nodes),
you will need to remove this label to access them using a LoadBalancer:

```sh
$ kubectl label node kind-control-plane node.kubernetes.io/exclude-from-external-load-balancers-
```
