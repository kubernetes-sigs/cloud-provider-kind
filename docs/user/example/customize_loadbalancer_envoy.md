### Customizing the LoadBalancer Envoy configuration

`cloud-provider-kind` creates one Envoy container for each `LoadBalancer`
Service and generates its listeners and clusters from two Go templates. You can
replace those templates with your own to change how Envoy is configured, for
example to enable the PROXY protocol towards the nodes or to tune health checks
and timeouts.

The templates receive the Service and the Node objects, so you can make the
behaviour conditional on your own labels or annotations. `cloud-provider-kind`
does not define any annotation itself; the names and their meaning belong to
your template.

#### Getting the built-in templates

Write the built-in templates to a directory and edit them there:

```
cloud-provider-kind dump-loadbalancer-templates ./my-templates
```

The directory contains two files:

- `lds.yaml.tmpl`: the Envoy listeners (one per Service port and IP family).
- `cds.yaml.tmpl`: the Envoy clusters that point to the node ports.

Both files are [Go text templates](https://pkg.go.dev/text/template) that
render the YAML files Envoy reads through its filesystem xDS configuration.

#### Using your templates

Start `cloud-provider-kind` with the directory:

```
cloud-provider-kind --loadbalancer-config-dir ./my-templates
```

Each file is optional. If a file is missing, the built-in template is used for
that resource. The files are read every time a LoadBalancer is created or
updated, so you can edit them without restarting `cloud-provider-kind`; update
the Service (or delete and recreate it) to trigger a new render.

If a template fails to parse or render, the LoadBalancer update fails and the
error is logged. Envoy keeps the previous configuration in that case.

#### Template data

The templates are executed with the following data:

| Field | Description |
|---|---|
| `.ServicePorts` | Map keyed by `<IPFamily>_<Port>_<Protocol>`. Each value has a `.Listener` (address, port and protocol Envoy listens on) and a `.Cluster` list of node endpoints (address, node port and protocol). |
| `.HealthCheckPort` | Port used to health check the nodes: `healthCheckNodePort` when `externalTrafficPolicy` is `Local`, the kube-proxy health port `10256` otherwise. |
| `.SessionAffinity` | Value of `spec.sessionAffinity` (`None` or `ClientIP`). |
| `.SourceRanges` | Parsed `spec.loadBalancerSourceRanges`, each with `.Prefix` and `.Length`. |
| `.Service` | The `Service` object being reconciled. |
| `.Nodes` | The `Node` objects used as backends. |

The listener names (`listener_<key>`) and cluster names (`cluster_<key>`) come
from the `.ServicePorts` keys. Keep them consistent between the two files so
that each listener routes to its cluster.

This data is part of the tool's behaviour and may change between releases.
Compare your templates with a fresh `dump-loadbalancer-templates` output when
you upgrade.

#### Example: PROXY protocol towards the nodes

Envoy terminates the client connection, so by default the pods see the address
of the Envoy container (or of the node, when `externalTrafficPolicy` is
`Cluster`). Backends that understand the
[PROXY protocol](https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt)
can recover the client address if Envoy sends the header.

The template in
[`examples/loadbalancer-templates/proxy-protocol/cds.yaml.tmpl`](https://github.com/kubernetes-sigs/cloud-provider-kind/blob/main/examples/loadbalancer-templates/proxy-protocol/cds.yaml.tmpl)
adds the PROXY protocol v2 transport socket to the clusters of Services that
carry the annotation `example.com/proxy-protocol: "true"`:

```
{{- $proxyProtocol := eq (index .Service.Annotations "example.com/proxy-protocol") "true" }}
...
{{- if $proxyProtocol }}
  transport_socket:
    name: envoy.transport_sockets.upstream_proxy_protocol
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.transport_sockets.proxy_protocol.v3.ProxyProtocolUpstreamTransport
      config:
        version: V2
      transport_socket:
        name: envoy.transport_sockets.raw_buffer
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.raw_buffer.v3.RawBuffer
{{- end }}
```

The health checks go to kube-proxy, which does not understand the PROXY
protocol, so the template also declares a `transport_socket_matches` entry with
a raw socket and selects it from the health check with
`transport_socket_match_criteria`.

Run `cloud-provider-kind` with that directory and deploy the example, which
contains an nginx backend that returns the client address it sees, one Service
with the annotation and one without:

```
cloud-provider-kind --loadbalancer-config-dir examples/loadbalancer-templates/proxy-protocol
kubectl apply -f examples/loadbalancer_proxy_protocol.yaml
```

```
$ kubectl get service lb-proxy-protocol lb-no-proxy-protocol
NAME                   TYPE           CLUSTER-IP     EXTERNAL-IP   PORT(S)        AGE
lb-proxy-protocol      LoadBalancer   10.96.54.201   172.18.0.5    80:31372/TCP   20s
lb-no-proxy-protocol   LoadBalancer   10.96.12.87    172.18.0.6    80:30618/TCP   20s
$ curl http://172.18.0.5
172.18.0.1
$ curl http://172.18.0.6
10.244.2.1
```

The first Service returns the address of the host on the `kind` network. The
second one returns the address kube-proxy masquerades with on the node that
forwarded the connection.
