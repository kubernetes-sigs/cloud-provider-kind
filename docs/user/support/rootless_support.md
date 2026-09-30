### Linux rootless container support

Linux rootless containers run without elevated privileges, and similar to VM containers, the KIND nodes are not directly reachable from the host, and the LoadBalancer assigned IPs are not routable from the host.

When run as an unprivileged user, cloud-provider-kind will create an envoy proxy container named `kindccm-...` for each LoadBalancer service.  The container will listen to the LoadBalancer IPs within the container network and loadbalance connections among the service's NodePorts.

However, cloud-provider-kind may alternatively be run with sudo. Running cloud-provider-kind with sudo requires either:

- `docker` with the sudo user environment containing DOCKER_HOST set to the rootless user's docker socket (eg /run/user/$UID/docker.socket).
- `podman` with the sudo user environment containing CONTAINER_HOST set to the rootless user's podman remote socket (eg unix:///run/user/$UID/podman/podman.sock). To discover the socket, use: `$ podman info -f json | jq -r .host.remoteSocket.path`

If run with sudo, cloud-provider-kind will create the envoy proxy containers, but publish their service ports on randomly assigned host ports.  cloud-provider-kind will then assign the LoadBalancer IPs to the host's loopback interface, listen for connections on the service ports on those IPs and forward connections to the envoy containers for their respective services, making the LoadBalancer IP/ports directly addressable from the host.

If cloud-provider-kind fails to auto-detect that the containers are rootless, the forwarding behavior can be requested with the `--enable-lb-tunnel` option.

The loopback address assignment & forwarding may be disabled by using the `--enable-lb-port-mapping` opton (service ports will still be published on random host ports).

Limitations:

- Mutation of Services, adding or removing ports to an existing Services is not supported.
- cloud-provider-kind binary needs permissions to add IP address to loopback and to listen on privileged ports (run as root)
- Overlapping IPs between the containers and the host can break connectivity.
