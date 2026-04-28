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
