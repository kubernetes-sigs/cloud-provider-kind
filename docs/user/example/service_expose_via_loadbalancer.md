### Creating a Service and exposing it via a LoadBalancer

Let's create an application that listens on port 8080 and expose it in the port 80 using a LoadBalancer.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: policy-local
  labels:
    app: MyLocalApp
spec:
  replicas: 1
  selector:
    matchLabels:
      app: MyLocalApp
  template:
    metadata:
      labels:
        app: MyLocalApp
    spec:
      containers:
        - name: agnhost
          image: registry.k8s.io/e2e-test-images/agnhost:2.40
          args:
            - netexec
            - --http-port=8080
            - --udp-port=8080
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: lb-service-local
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  selector:
    app: MyLocalApp
  ports:
    - protocol: TCP
      port: 80
      targetPort: 8080
```

```sh
$ kubectl apply -f examples/loadbalancer_etp_local.yaml
deployment.apps/policy-local created
service/lb-service-local created
$ kubectl get service/lb-service-local
NAME               TYPE           CLUSTER-IP      EXTERNAL-IP   PORT(S)        AGE
lb-service-local   LoadBalancer   10.96.207.137   192.168.8.7   80:31215/TCP   57s
```

We can see how the `EXTERNAL-IP` field contains an IP, and we can use it to connect to our
application.

```
$ curl  192.168.8.7:80/hostname
policy-local-59854877c9-xwtfk

$  kubectl get pods
NAME                            READY   STATUS    RESTARTS   AGE
policy-local-59854877c9-xwtfk   1/1     Running   0          2m38s
```