### Running the provider

Once the cluster is running, we need to run the `cloud-provider-kind` in a terminal and keep it running. The `cloud-provider-kind` will monitor all your KIND clusters and `Services` with Type `LoadBalancer` and create the corresponding LoadBalancer containers that will expose those Services.

```sh
bin/cloud-provider-kind
I0416 19:58:18.391222 2526219 controller.go:98] Creating new cloud provider for cluster kind
I0416 19:58:18.398569 2526219 controller.go:105] Starting service controller for cluster kind
I0416 19:58:18.399421 2526219 controller.go:227] Starting service controller
I0416 19:58:18.399582 2526219 shared_informer.go:273] Waiting for caches to sync for service
I0416 19:58:18.500460 2526219 shared_informer.go:280] Caches are synced for service
...
```