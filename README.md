# Kubernetes Cloud Provider for KIND

KIND has demonstrated to be a very versatile, efficient, cheap and very useful tool for Kubernetes testing. However, KIND doesn't offer capabilities for testing all the features that depend on cloud-providers, specifically the Load Balancers, causing a gap on testing and a bad user experience, since is not easy to connect to the applications running on the cluster.

`cloud-provider-kind` aims to fill this gap and provide an agnostic and cheap solution for all the Kubernetes features that depend on a cloud-provider using KIND.

- [Slack channel](https://kubernetes.slack.com/messages/kind)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-testing)

## Talks 

Kubecon EU 2024 - [Keep Calm and Load Balance on KIND - Antonio Ojea & Benjamin Elder, Google](https://sched.co/1YhhY)

[![Keep Calm and Load Balance on KIND](https://img.youtube.com/vi/U6_-y24rJnI/0.jpg)](https://www.youtube.com/watch?v=U6_-y24rJnI)


## Install

You can install `cloud-provider-kind` using `go install`:

```sh
go install sigs.k8s.io/cloud-provider-kind@latest
```

This will install the binary in `$GOBIN` (typically `~/go/bin`); you
can make it available elsewhere if appropriate:

```sh
sudo install ~/go/bin/cloud-provider-kind /usr/local/bin
```

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

**Note**

Control-plane nodes need to remove the special label `node.kubernetes.io/exclude-from-external-load-balancers` to be able to access the workloads running on those nodes using a LoadBalancer Service.

```sh
$ kubectl label node kind-control-plane node.kubernetes.io/exclude-from-external-load-balancers-
node/kind-control-plane unlabeled
```

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

### Mac and Windows support

Mac and Windows run the containers inside a VM and, on the contrary to Linux, the KIND nodes are not reachable from the host,
so the LoadBalancer assigned IP is not working for users.

To solve this problem, cloud-provider-kind, leverages the existing docker portmap capabilities to expose the Loadbalancer IP and Ports
on the host.

Limitations:

- Mutation of Services, adding or removing ports to an existing Services, is not supported.
- cloud-provider-kind binary needs permissions to add IP address to interfaces and to listen on privileged ports.
- Overlapping IP between the containers and the host can break connectivity.

Mainly tested with `docker` and `Linux`, though `Windows` and `Mac` are also basically supported:
- On macOS you must run cloud-provider-kind using `sudo`
- On Windows you must run cloud-provider-kind from a shell that uses `Run as administrator`
- Further feedback from users will be helpful to support other related platforms.

**Note**

The project is still in very alpha state, bugs are expected, please report them back opening a Github issue.

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
