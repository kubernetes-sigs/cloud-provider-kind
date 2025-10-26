# Ingress Controller (Ingress-to-Gateway Translation)


This is a Kubernetes controller that implements the `networkingv1.Ingress` API
specification.

Its primary purpose is to act as a translation layer, not a data plane. It
watches `Ingress` resources and translates their semantics into `Gateway API`
resources, specifically `Gateway` and `HTTPRoute`. The actual proxying and
traffic management can then be done by a separate, conformant `Gateway API`
implementation that reads these translated resources.

The controller's goal is to provide the exact semantics of the `Ingress` API
using a standard `Gateway API` data plane.


## Architecture

The controller's logic is built on four key components that work together to
mimic `Ingress` behavior.

### Singleton Namespace Gateway

The controller ensures that a single `gatewayv1.Gateway` resource exists for
each namespace that contains a managed `Ingress`.

This `Gateway` is given a predictable name (e.g., kind-ingress-gateway).

It acts as the single, consolidated entry point for all `Ingress` traffic within
that namespace.

It is configured with a `GatewayClassName` provided to the controller, which
links it to the underlying `Gateway API` data plane implementation.

### Ingress-to-HTTPRoute Translation (1:N Mapping)

To correctly implement `Ingress` precedence (where exact hosts and paths are
matched before wildcards or prefixes), the controller implements a 1-to-Many
(1:N) mapping for each `Ingress` resource:

* **Per-Host Rule:** For each rule defined in `ingress.spec.rules` (e.g., host:
  "foo.example.com"), a dedicated `HTTPRoute` resource is created. This
  `HTTPRoute` contains only the hostnames and rules (paths) for that specific
  host. This design correctly isolates rules per host.

* **Default Backend Rule:** A single, separate `HTTPRoute` is created to handle
  the default backend. This route has its hostnames field omitted (nil), which
  in `Gateway API` semantics means it matches all traffic that wasn't captured
  by a more specific (per-host) `HTTPRoute`.

Precedence: The controller implements `Ingress` default backend precedence. If
an `Ingress` defines a rule with an empty host (host: ""), that rule's paths are
used for the default backend `HTTPRoute`. If no such rule exists, the
ingress.spec.defaultBackend is used as a fallback.

### Namespace-Wide TLS Aggregation

The `Ingress` spec allows multiple `Ingress`es to provide TLS secrets. The
controller consolidates this for the singleton Gateway.

On every reconciliation, the controller scans all managed `Ingress`es in the
namespace.

It aggregates every unique secretName from all ingress.spec.tls sections.

This combined, sorted list of secrets is set on the certificateRefs field of the
singleton Gateway's HTTPS listener.

This means all `Ingress`es in a namespace share one set of TLS certificates on a
single listener, which is consistent with how `Ingress` is typically
implemented.

### Ingress Status Reconciliation

The controller watches the status of the singleton namespace Gateway.

When the underlying Gateway API implementation provisions an IP or hostname for
the Gateway, the controller detects this change.

It copies this IP/hostname to the `status.loadBalancer` field of every managed
`Ingress` in that namespace.

### Reconciliation Flow (syncHandler)

The simplified logic for a single `Ingress` change (Create, Update) is as
follows:

1. An `Ingress` event is received for ingress-A in namespace-foo.

2. The controller re-scans all `Ingress`es in namespace-foo to get a fresh,
   aggregated list of all TLS secrets.

3. It reconciles the singleton Gateway in namespace-foo, ensuring it exists and
   its HTTPS listener has the correct (sorted) list of aggregated secrets.

4. The controller generates a "desired" list of `HTTPRoute` resources for
   ingress-A (e.g., one for a.example.com and one for the default backend).

5. It lists all "existing" `HTTPRoute`s in namespace-foo that are owned by
   ingress-A.

6. It compares the "desired" and "existing" lists:

7. New routes are created.

8. Existing routes are updated if their spec (paths, backends) has changed.

9. Stale routes (e.g., for a host that was removed from ingress-A) are deleted.

10. The controller fetches the latest `Gateway.status` and updates
    ingress-A.status (and all other Ingresses in the namespace) with the
    provisioned IP/hostname.

## Known Incompatibilities

This translation model is highly conformant but has known semantic mismatches
with the `Ingress` API, as identified by the `Ingress` conformance test suite.

Wildcard Host Matching: The `Ingress` API specifies that host: "*.foo.com"
should not match baz.bar.foo.com (it matches only one DNS label). The Gateway
API specifies that hostnames: ["*.foo.com"] does match baz.bar.foo.com (it
matches all subdomains). This controller follows the Gateway API specification,
as that is its underlying data plane.