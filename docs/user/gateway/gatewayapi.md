## Gateway API support

This provider has support for the [Gateway API](https://gateway-api.sigs.k8s.io/).
It implements the `Gateway`, `HTTPRoute`, and `GRPCRoute` functionalities and passes the community conformance tests.

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

### GRPCRoute

[GRPCRoute](https://gateway-api.sigs.k8s.io/api-types/grpcroute/) (GEP-1016) attaches to HTTP and HTTPS listeners on the same Gateway as HTTPRoute.

Supported:

- Method matching (`Exact` and `RegularExpression`) on service, method, or both
- Header matching (`Exact` and `RegularExpression`)
- Hostnames, weighted backends, and `ReferenceGrant` for cross-namespace Services
- Filters: `RequestHeaderModifier`, `ResponseHeaderModifier`, and `RequestMirror`

Not supported:

- `ExtensionRef` filters (the rule is dropped and `PartiallyInvalid` is set)
- Filters on `GRPCBackendRef`

Behavior:

- An HTTPRoute and a GRPCRoute on the same listener cannot share a hostname. The older route stays attached. The other route is rejected with `Accepted=False`.
- If every backend is invalid, Envoy returns gRPC `UNAVAILABLE` (status 14), not HTTP 500.
- If some backends are invalid, those weights go to an internal `kind-grpc-unavailable` cluster.
- gRPC upstream clusters use HTTP/2 and a `-h2` name suffix so they do not share HTTP/1.1 clusters with HTTPRoute.
- HTTP listeners enable HTTP/2 prior knowledge (h2c) so gRPC clients can connect without TLS.
- Duplicate header match names: only the first name is used.
- Route order: longer service name, then longer method name, then more headers, then older route, then `{namespace}/{name}`.

See [Creating a Gateway and a GRPCRoute](../example/creating_gateway_grpc_route.md).

