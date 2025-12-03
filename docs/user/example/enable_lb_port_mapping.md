### Enabling Load Balancer Port Mapping

When running `cloud-provider-kind` in a container on Windows or macOS, accessing
`LoadBalancer` services can be challenging. Similar problems occur when running
Podman as root, since Podman does not allow binding to privileged ports (e.g.,
1-1024). The `-enable-lb-port-mapping` flag provides a solution by enabling the
necessary port mapping, allowing host access to these services.

To connect to your service in these cases, run `cloud-provider-kind` with the
`-enable-lb-port-mapping` option. This configures the Envoy container with an
ephemeral host port that maps to the port the `LoadBalancer`'s external IP is
listening on.

```
bin/cloud-provider-kind -enable-lb-port-mapping
```

For example, given a `LoadBalancer` listening on port `5678`.

```
> kubectl get service
NAME          TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)          AGE
foo-service   LoadBalancer   10.96.240.105   10.89.0.10    5678:31889/TCP   14m
```

The Envoy container will have an ephemeral port (e.g., `42381`) mapped to the
`LoadBalancer`'s port `5678`.

```
> podman ps
CONTAINER ID  IMAGE                                                                                           COMMAND               CREATED         STATUS         PORTS                                              NAMES
d261abc4b540  docker.io/envoyproxy/envoy:v1.30.1                                                              bash -c echo -en ...  21 seconds ago  Up 22 seconds  0.0.0.0:42381->5678/tcp, 0.0.0.0:36673->10000/tcp  kindccm-TLRDKPBWWH4DUSI7J7BNE3ABETEPCKSYA6UIWR5B
```

Use this ephemeral port to connect to the service.

```
curl localhost:42381
```
