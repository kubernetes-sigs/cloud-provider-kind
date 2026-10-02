### Securing backend TLS with BackendTLSPolicy

`BackendTLSPolicy` tells the Gateway to connect to a Service over TLS and to
check the backend certificate. Put the CA PEM in a ConfigMap key named `ca.crt`.
Set `validation.hostname` to the name on that certificate.

The backend must serve TLS. The Gateway does not send plaintext to the Service
when a policy applies.

Create a ConfigMap from a CA file, then apply the policy with the Gateway and
HTTPRoute. A complete example is in `examples/gateway_backendtlspolicy.yaml`.

```sh
kubectl create configmap backend-ca --from-file=ca.crt=ca.crt
```

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: prod-web
spec:
  gatewayClassName: cloud-provider-kind
  listeners:
  - protocol: HTTP
    port: 80
    name: prod-web-gw
    allowedRoutes:
      namespaces:
        from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: foo
spec:
  parentRefs:
  - name: prod-web
  rules:
  - backendRefs:
    - name: myapp-svc
      port: 8443
---
apiVersion: gateway.networking.k8s.io/v1
kind: BackendTLSPolicy
metadata:
  name: myapp-backend-tls
spec:
  targetRefs:
  - group: ""
    kind: Service
    name: myapp-svc
  validation:
    caCertificateRefs:
    - group: ""
      kind: ConfigMap
      name: backend-ca
    hostname: myapp-svc.example.com
```

Check that the policy is accepted on the Gateway:

```sh
kubectl get backendtlspolicy myapp-backend-tls -o yaml
```

The ancestor status must show `Accepted=True` and `ResolvedRefs=True` for the
Gateway. If `ca.crt` is missing or empty, `ResolvedRefs` is `False` with reason
`InvalidCACertificateRef`, and the Gateway returns HTTP 503 for that backend.
