## Gateway API support

This provider has support for the [Gateway API](https://gateway-api.sigs.k8s.io/).
It implements the `Gateway` and `HTTPRoute` functionalities and passes the community conformance tests.

The Gateway API controller is enabled by default using the standard channel,
but you can select the Gateway API release channel (standard/experimental) or just disable the feature completely
using the flag `gateway-channel`:

```sh
cloud-provider-kind --gateway-channel standard|experimental|disabled
```