## Compatibility：
This guide applies to [cloud-provider-kind](https://github.com/kubernetes-sigs/cloud-provider-kind) v0.9.0+. For older versions, refer to historical docs.

## Setting Up Ingress

Ingress exposes HTTP and HTTPS routes from outside the cluster to services within the cluster.

Since cloud-provider-kind v0.9.0, it natively supports Ingress. No third-party ingress controllers are required by default.

For third-party ingress solutions (e.g., Ingress NGINX, Contour), please follow their official documentation.

> **NOTE**: Gateway API is also natively supported (along with Ingress). See the official [Ingress migration guide](https://gateway-api.sigs.k8s.io/guides/migrating-from-ingress/) for details.

## Create Cluster

> **WARNING**: If you are using a [rootless container runtime], ensure your host is
> properly configured before creating the KIND cluster. Most Ingress and Gateway controllers will
> not work if these steps are skipped.

Create a kind cluster and run [Cloud Provider KIND] that automatically enables LoadBalancer support for Ingress. Create a cluster as follows.

```bash
kind create cluster
```

## Using Ingress

The following example creates simple http-echo services and an Ingress object to route to these services.

```yaml
kind: Pod
apiVersion: v1
metadata:
  name: foo-app
  labels:
    app: foo
spec:
  containers:
  - command:
    - /agnhost
    - serve-hostname
    - --http=true
    - --port=8080
    image: registry.k8s.io/e2e-test-images/agnhost:2.39
    name: foo-app
---
kind: Service
apiVersion: v1
metadata:
  name: foo-service
spec:
  selector:
    app: foo
  ports:
  # Default port used by the image
  - port: 8080
---
kind: Pod
apiVersion: v1
metadata:
  name: bar-app
  labels:
    app: bar
spec:
  containers:
  - command:
    - /agnhost
    - serve-hostname
    - --http=true
    - --port=8080
    image: registry.k8s.io/e2e-test-images/agnhost:2.39
    name: bar-app
---
kind: Service
apiVersion: v1
metadata:
  name: bar-service
spec:
  selector:
    app: bar
  ports:
  # Default port used by the image
  - port: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: example-ingress
spec:
  rules:
  - http:
      paths:
      - pathType: Prefix
        path: /foo
        backend:
          service:
            name: foo-service
            port:
              number: 8080
      - pathType: Prefix
        path: /bar
        backend:
          service:
            name: bar-service
            port:
              number: 8080
---
```

Apply the configuration:

```bash
kubectl apply -f https://kind.sigs.k8s.io/examples/ingress/usage.yaml
```

### Verify Ingress Works

Check the External IP assigned to the Ingress by the built-in LoadBalancer.

```bash
kubectl get ingress
NAME              CLASS     HOSTS         ADDRESS        PORTS   AGE
example-ingress   <none>    example.com   172.18.0.5     80      10m
```


# get the Ingress IP

```bash
INGRESS_IP=$(kubectl get ingress example-ingress -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# should output "foo-app"

curl ${INGRESS_IP}/foo

# should output "bar-app"
curl ${INGRESS_IP}/bar
```

[LoadBalancer]: /docs/user/loadbalancer/
[Cloud Provider KIND]: /docs/user/loadbalancer/
[rootless container runtime]: /docs/user/rootless/