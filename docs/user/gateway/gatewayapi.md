## Gateway API support

This provider has support for the [Gateway API](https://gateway-api.sigs.k8s.io/).
It implements the `Gateway` and `HTTPRoute` functionalities and passes the community conformance tests.

The Gateway API controller is enabled by default using the standard channel,
but you can select the Gateway API release channel (standard/experimental) or just disable the feature completely
using the flag `gateway-channel`:

```sh
cloud-provider-kind --gateway-channel standard|experimental|disabled
```

### HTTP external authorization (GEP-1494)

The experimental channel adds the `ExternalAuth` HTTPRoute filter described in
[GEP-1494](https://gateway-api.sigs.k8s.io/geps/gep-1494/). It delegates
authentication and authorization for a route rule to an external server that
speaks Envoy's `ext_authz` protocol, over either `HTTP` or `GRPC`:

```yaml
    filters:
    - type: ExternalAuth
      externalAuth:
        protocol: HTTP
        backendRef:
          name: authz-svc
          port: 8080
        http:
          path: /auth
          allowedHeaders:
          - X-Request-Id
          allowedResponseHeaders:
          - X-Authenticated-User
```

Requests only reach the backends when the authorization server approves them.
If the server is unreachable, or the `backendRef` cannot be resolved, the rule
fails closed and the route reports `ResolvedRefs=False`.

See `examples/gateway_external_auth.yaml` for a complete example.
