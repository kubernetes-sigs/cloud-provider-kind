## Gateway API support

This provider has support for the [Gateway API](https://gateway-api.sigs.k8s.io/).
It implements the `Gateway` and `HTTPRoute` functionalities and advertises those
features on `GatewayClass`. The Gateway HTTP conformance suite runs against this class.

`BackendTLSPolicy` is an Extended feature. The implementation covers the core of
the API: TLS from the Gateway to a Service backend, hostname validation, and
CA certificates from a ConfigMap `ca.crt` key. SAN validation
(`BackendTLSPolicySANValidation`) and well-known public CAs are not supported.

The Gateway API controller is enabled by default using the standard channel,
but you can select the Gateway API release channel (standard/experimental) or just disable the feature completely
using the flag `gateway-channel`:

```sh
cloud-provider-kind --gateway-channel standard|experimental|disabled
```