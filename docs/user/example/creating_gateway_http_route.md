### Creating a Gateway and a HTTPRoute

Similar to Services with LoadBalancers we can use Gateway API

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
      port: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  selector:
    matchLabels:
      app: MyApp
  replicas: 1
  template:
    metadata:
      labels:
        app: MyApp
    spec:
      containers:
      - name: myapp
        image: registry.k8s.io/e2e-test-images/agnhost:2.39
        args:
          - netexec
          - --http-port=80
          - --delay-shutdown=30
        ports:
          - name: httpd
            containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: myapp-svc
spec:
  type: ClusterIP
  selector:
    app: MyApp
  ports:
    - name: httpd
      port: 8080
      targetPort: 80
```

We can get the external IP associated to the gateway:

```sh
 kubectl get gateway
NAME       CLASS                 ADDRESS       PROGRAMMED   AGE
prod-web   cloud-provider-kind   192.168.8.5   True         3d21h
```

and the HTTPRoutes

```sh
kubectl get httproutes
NAME   HOSTNAMES   AGE
foo                3d21h
```

and test that works:

```sh
$ curl 192.168.8.5/hostname
myapp-7dcffbf547-9kl2d
```
