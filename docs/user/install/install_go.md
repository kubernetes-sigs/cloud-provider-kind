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