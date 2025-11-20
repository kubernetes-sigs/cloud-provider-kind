# Kubernetes Cloud Provider for KIND

KIND has demonstrated to be a very versatile, efficient, cheap and very useful tool for Kubernetes testing. However, KIND doesn't offer capabilities for testing all the features that depend on cloud-providers, specifically the Load Balancers, causing a gap on testing and a bad user experience, since is not easy to connect to the applications running on the cluster.

`cloud-provider-kind` aims to fill this gap and provide an agnostic and cheap solution for all the Kubernetes features that depend on a cloud-provider using KIND.

- [Slack channel](https://kubernetes.slack.com/messages/kind)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-testing)

## Talks

Kubecon EU 2024 - [Keep Calm and Load Balance on KIND - Antonio Ojea & Benjamin Elder, Google](https://sched.co/1YhhY)

[![Keep Calm and Load Balance on KIND](https://img.youtube.com/vi/U6_-y24rJnI/0.jpg)](https://www.youtube.com/watch?v=U6_-y24rJnI)

## Install

### Installing with `go install`

You can install `cloud-provider-kind` using `go install`:

```sh
go install sigs.k8s.io/cloud-provider-kind@latest
```

This will install the binary in `$GOBIN` (typically `~/go/bin`); you
can make it available elsewhere if appropriate:

```sh
sudo install ~/go/bin/cloud-provider-kind /usr/local/bin
```

### Installing With A Package Manager

The cloud-provider-kind community has enabled installation via the following package managers.

> [!NOTE]
> The following are community supported efforts. The `cloud-provider-kind` maintainers are not involved in the creation of these packages, and the upstream community makes no claims on the validity, safety, or content of them.

On macOS via Homebrew:

```sh
brew install cloud-provider-kind
```

### Running via Docker Image

Starting with v0.4.0, the docker image for cloud-provider-kind is available
at `registry.k8s.io/cloud-provider-kind/cloud-controller-manager`

You can also build it locally:

```sh
git clone https://github.com/kubernetes-sigs/cloud-provider-kind.git
Cloning into 'cloud-provider-kind'...
remote: Enumerating objects: 6779, done.
remote: Counting objects: 100% (6779/6779), done.
remote: Compressing objects: 100% (4225/4225), done.q
remote: Total 6779 (delta 2150), reused 6755 (delta 2135), pack-reused 0
Receiving objects: 100% (6779/6779), 9.05 MiB | 1.83 MiB/s, done.
Resolving deltas: 100% (2150/2150), done.

cd cloud-provider-kind && make
sudo mv ./bin/cloud-provider-kind  /usr/local/bin/cloud-provider-kind
```

Another alternative is to run it as a container, but this will require to mount
the docker socket inside the container:

```sh
docker build . -t cloud-provider-kind
# using the host network
docker run --rm --network host -v /var/run/docker.sock:/var/run/docker.sock cloud-provider-kind
# or the kind network
docker run --rm --network kind -v /var/run/docker.sock:/var/run/docker.sock cloud-provider-kind
```

Or using `compose.yaml` file:

```sh
# using the `kind` network (`host` is the default value for NET_MODE)
NET_MODE=kind docker compose up -d
```

## Gateway API support (Alpha)

This provider has alpha support for the [Gateway API](https://gateway-api.sigs.k8s.io/).
It implements the `Gateway` and `HTTPRoute` functionalities.

You can enable the Gateway API controllerby selecting the Gateway API release channel (standard/experimental):

```sh
cloud-provider-kind --gateway-channel standard
```

## How to use it

Run a KIND cluster:

```sh
$ kind create cluster
Creating cluster "kind" ...
 ✓ Ensuring node image (kindest/node:v1.26.0) 🖼
 ✓ Preparing nodes 📦
 ✓ Writing configuration 📜
 ✓ Starting control-plane 🕹️
 ✓ Installing CNI 🔌
 ✓ Installing StorageClass 💾
Set kubectl context to "kind-kind"
You can now use your cluster with:

kubectl cluster-info --context kind-kind

Have a question, bug, or feature request? Let us know! https://kind.sigs.k8s.io/#community 🙂

```

### Allowing load balancers access to control plane nodes

By default, [Kubernetes expects workloads will not run on control plane nodes](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/#control-plane-node-isolation)
and labels them with [`node.kubernetes.io/exclude-from-external-load-balancers`](https://kubernetes.io/docs/reference/labels-annotations-taints/#node-kubernetes-io-exclude-from-external-load-balancers),
which stops load balancers from accessing them.

If you are running workloads on control plane nodes, as is the [default kind configuration](https://kind.sigs.k8s.io/docs/user/configuration/#nodes),
you will need to remove this label to access them using a LoadBalancer:

```sh
$ kubectl label node kind-control-plane node.kubernetes.io/exclude-from-external-load-balancers-
```

### Running the provider

Once the cluster is running, we need to run the `cloud-provider-kind` in a terminal and keep it running. The `cloud-provider-kind` will monitor all your KIND clusters and `Services` with Type `LoadBalancer` and create the corresponding LoadBalancer containers that will expose those Services.

```sh
bin/cloud-provider-kind
I0416 19:58:18.391222 2526219 controller.go:98] Creating new cloud provider for cluster kind
I0416 19:58:18.398569 2526219 controller.go:105] Starting service controller for cluster kind
I0416 19:58:18.399421 2526219 controller.go:227] Starting service controller
I0416 19:58:18.399582 2526219 shared_informer.go:273] Waiting for caches to sync for service
I0416 19:58:18.500460 2526219 shared_informer.go:280] Caches are synced for service
...
```

### Configuring Proxy Image

> [!WARNING]
> Using a custom mirror URL and/or different Envoy version is not supported by `cloud-provider-kind` and should be used at your own risk.

This allows you to use different versions or custom builds of the Envoy proxy.

By default, `cloud-provider-kind` uses the --proxy-image=[`DefaultProxyImage`](pkg/constants/constants.go#L8)
You can specify a custom proxy image using the `--proxy-image` as below

Example of different envoy version (example v1.36.2). Note: Use at your own risk

```sh
bin/cloud-provider-kind --proxy-image docker.io/envoyproxy/envoy:v1.36.2
```

Example of use mirror registry. Note: Use at your own risk

```sh
MIRROR_REGISTRY_URL="<your-mirror-registry-url>"
bin/cloud-provider-kind --proxy-image $MIRROR_REGISTRY_URL/envoyproxy/envoy:v1.33.2
```

### Creating a Service and exposing it via a LoadBalancer

Let's create an application that listens on port 8080 and expose it in the port 80 using a LoadBalancer.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: policy-local
  labels:
    app: MyLocalApp
spec:
  replicas: 1
  selector:
    matchLabels:
      app: MyLocalApp
  template:
    metadata:
      labels:
        app: MyLocalApp
    spec:
      containers:
        - name: agnhost
          image: registry.k8s.io/e2e-test-images/agnhost:2.40
          args:
            - netexec
            - --http-port=8080
            - --udp-port=8080
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: lb-service-local
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  selector:
    app: MyLocalApp
  ports:
    - protocol: TCP
      port: 80
      targetPort: 8080
```

```sh
$ kubectl apply -f examples/loadbalancer_etp_local.yaml
deployment.apps/policy-local created
service/lb-service-local created
$ kubectl get service/lb-service-local
NAME               TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)        AGE
lb-service-local   LoadBalancer   10.96.207.137   192.168.8.7   80:31215/TCP   57s
```

We can see how the `EXTERNAL-IP` field contains an IP, and we can use it to connect to our
application.

```
$ curl  192.168.8.7:80/hostname
policy-local-59854877c9-xwtfk

$  kubectl get pods
NAME                            READY   STATUS    RESTARTS   AGE
policy-local-59854877c9-xwtfk   1/1     Running   0          2m38s
```

### Creating a Gateway and a HTTPRoute

Similar to Services with LoadBalancers we can use Gateway API

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: prod-web
spec:
  gatewayClassName: cloud-provider-kind
  listeners:
  - protocol: HTTP
    port: 80
    name: prod-web-gw
    allowedRoutes:
      namespaces:
        from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: foo
spec:
  parentRefs:
  - name: prod-web
  rules:
  - backendRefs:
    - name: myapp-svc
      port: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  selector:
    matchLabels:
      app: MyApp
  replicas: 1
  template:
    metadata:
      labels:
        app: MyApp
    spec:
      containers:
      - name: myapp
        image: registry.k8s.io/e2e-test-images/agnhost:2.39
        args:
          - netexec
          - --http-port=80
          - --delay-shutdown=30
        ports:
          - name: httpd
            containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: myapp-svc
spec:
  type: ClusterIP
  selector:
    app: MyApp
  ports:
    - name: httpd
      port: 8080
      targetPort: 80
```

We can get the external IP associated to the gateway:

```sh
 kubectl get gateway
NAME       CLASS                 ADDRESS       PROGRAMMED   AGE
prod-web   cloud-provider-kind   192.168.8.5   True         3d21h
```

and the HTTPRoutes

```sh
kubectl get httproutes
NAME   HOSTNAMES   AGE
foo                3d21h
```

and test that works:

```sh
$ curl 192.168.8.5/hostname
myapp-7dcffbf547-9kl2d
```

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

**Note**

The project is still in very alpha state, bugs are expected, please report them back opening a Github issue.

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
