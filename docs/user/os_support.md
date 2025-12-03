### Mac, Windows and WSL2 support

Mac and Windows run the containers inside a VM and, on the contrary to Linux, the KIND nodes are not reachable from the host,
so the LoadBalancer assigned IP is not working for users.

To solve this problem, cloud-provider-kind, leverages the existing docker portmap capabilities to expose the Loadbalancer IP and Ports
on the host. When you start cloud-provider-kind and create a LoadBalancer service, you will notice that a container named `kindccm-...` is launched within Docker. You can access the service by using the port exposed from the container to the host machine with `localhost`.

For WSL2, it is recommended to use the default [NAT](https://learn.microsoft.com/en-us/windows/wsl/networking) network mode with Docker Desktop on Windows. On WSL2, you can access the service via the external IP. On Windows, you can access the service by leveraging Docker's portmap capabilities.

Limitations:

- Mutation of Services, adding or removing ports to an existing Services, is not supported.
- cloud-provider-kind binary needs permissions to add IP address to interfaces and to listen on privileged ports.
- Overlapping IP between the containers and the host can break connectivity.

Mainly tested with `docker` and `Linux`, though `Windows`, `Mac` and `WSL2` are also basically supported:

- On macOS and WSL2 you must run cloud-provider-kind using `sudo`
- On Windows you must run cloud-provider-kind from a shell that uses `Run as administrator`
- Further feedback from users will be helpful to support other related platforms.