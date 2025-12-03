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