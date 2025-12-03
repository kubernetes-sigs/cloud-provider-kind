### Configuring Proxy Image Registry

> [!WARNING]
> The proxy image is an implementation detail of `cloud-provider-kind` and it is not guaranteed to be stable.
> Changing the image version or tag is not supported and may break the cloud provider.
> Use the following instructions only for mirroring the image to a private registry.

You can check the image used by the current version of `cloud-provider-kind` running:

```sh
bin/cloud-provider-kind list-images
```

If you need to mirror the image to a private registry, you can override the registry URL using the `CLOUD_PROVIDER_KIND_REGISTRY_URL` environment variable.
This will use the same image name and tag but with the specified registry.

Example of use mirror registry:

```sh
CLOUD_PROVIDER_KIND_REGISTRY_URL="<your-mirror-registry-url>" bin/cloud-provider-kind
```