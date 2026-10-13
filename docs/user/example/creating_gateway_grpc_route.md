### Creating a Gateway and a GRPCRoute

GRPCRoute uses the same HTTP or HTTPS listeners as HTTPRoute. The backend Service must speak gRPC over HTTP/2.

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
kind: GRPCRoute
metadata:
  name: grpc-health
spec:
  parentRefs:
  - name: prod-web
  rules:
  - matches:
    - method:
        service: grpc.health.v1.Health
        method: Check
    backendRefs:
    - name: grpc-echo
      port: 5000
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: grpc-echo
spec:
  selector:
    matchLabels:
      app: grpc-echo
  replicas: 1
  template:
    metadata:
      labels:
        app: grpc-echo
    spec:
      containers:
      - name: grpc-echo
        image: registry.k8s.io/e2e-test-images/agnhost:2.66.1
        args:
          - grpc-health-checking
          - --port=5000
        ports:
          - name: grpc
            containerPort: 5000
---
apiVersion: v1
kind: Service
metadata:
  name: grpc-echo
spec:
  type: ClusterIP
  selector:
    app: grpc-echo
  ports:
    - name: grpc
      port: 5000
      targetPort: 5000
```

Get the Gateway address:

```sh
kubectl get gateway
NAME       CLASS                 ADDRESS       PROGRAMMED   AGE
prod-web   cloud-provider-kind   192.168.8.5   True         3d21h
```

and the GRPCRoutes:

```sh
kubectl get grpcroutes
NAME          HOSTNAMES   AGE
grpc-health               3d21h
```

Test with [grpcurl](https://github.com/fullstorydev/grpcurl):

```sh
$ grpcurl -plaintext 192.168.8.5:80 grpc.health.v1.Health/Check
{
  "status": "SERVING"
}
```
