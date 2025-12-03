## Gateway API support (Alpha)

This provider has alpha support for the [Gateway API](https://gateway-api.sigs.k8s.io/).
It implements the `Gateway` and `HTTPRoute` functionalities.

You can enable the Gateway API controllerby selecting the Gateway API release channel (standard/experimental):

```sh
cloud-provider-kind --gateway-channel standard
```

