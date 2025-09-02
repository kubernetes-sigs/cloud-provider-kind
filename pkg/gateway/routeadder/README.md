
Packages generated with `hack/build-route-adder.sh` to inject a binary on the
envoy container image that allows to configure routes.

This is required because the envoy image does not have `route` or `iproute2`
commands and, in order to forward the gateway traffic to a Service ClusterIP, we
need to route it through one of the cluster nodes.