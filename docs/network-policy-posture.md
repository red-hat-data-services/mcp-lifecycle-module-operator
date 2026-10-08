# Operand NetworkPolicy posture (RHOAI/ODH)

In RHOAI/ODH deployments the module operator runs the MCP Lifecycle operand under a
**restricted** NetworkPolicy posture. This document explains what that means for
cluster admins and how to open an ingress path for a specific MCP server.

## What the restricted posture does

The module operator sets `--network-policy-default-posture=restricted` on the
operand controller-manager. Under this posture the operand generates an
ingress NetworkPolicy for every `MCPServer` it manages that **declares a
deny-by-default ingress posture**: unless a source is explicitly allowed, no
other pod is intended to reach the MCP server pod.

Two important caveats about what this policy actually guarantees:

- **Enforcement requires a NetworkPolicy-capable network plugin.** The deny only
  takes effect where a NetworkPolicy-enforcing CNI is installed. Where none is,
  the policy is declarative only and traffic is not blocked.
- **NetworkPolicies are additive.** The generated policy is unioned with any other
  NetworkPolicy selecting the same pod, so another policy may still admit traffic.

So the restricted posture expresses the *intended* ingress policy; it is not by
itself proof that traffic is blocked.

Egress is left **allow-all** by default (the platform contract permits unrestricted
egress), so only ingress is constrained.

This satisfies ODH-ADR-Operator-0016, which requires operand workloads to default
to a restrictive network posture in RHOAI/ODH.

## Default behavior when no source is declared

When an `MCPServer` declares **no** `spec.network.ingressFrom`, the operand still
keeps required paths working automatically:

- **Operator verification handshake** - the operand admits the module operator's
  own controller pod (a "self-peer"), so the operator can reach the MCP server to
  run its discovery/verification handshake. Such a server still reports
  `Verified=True`.
- **kubelet health/readiness probes** - these originate from the node, off the pod
  network, so they are not subject to NetworkPolicy and the workload stays
  schedulable and `Available`.
- **Gateway, monitoring and webhook paths** - admitted where the corresponding
  feature is configured.

## Opening an ingress path for an MCP server

To allow traffic from a specific source, set `spec.network.ingressFrom` on the
`MCPServer`. Each entry is a standard NetworkPolicy peer (`namespaceSelector`
and/or `podSelector`).

> **Important:** declaring `spec.network.ingressFrom` replaces the automatic
> defaults - the operator self-peer is **no longer added for you**. Under the
> restricted posture the list must therefore also include peers matching the
> operator pods (and each required gateway source); otherwise, where the policy is
> enforced, the operator handshake cannot reach the server and it never becomes
> `Verified` and its address is not published.

For example, to admit clients in `my-client-ns` while keeping verification working:

```yaml
apiVersion: mcp.x-k8s.io/v1beta1
kind: MCPServer
metadata:
  name: my-server
  namespace: my-operand-ns
spec:
  # ...
  network:
    ingressFrom:
      # Required: re-admit the operator so the verification handshake still
      # reaches the server once ingressFrom is set.
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: <operator-namespace>
        podSelector:
          matchLabels:
            app.kubernetes.io/name: mcp-lifecycle-operator
            control-plane: controller-manager
      # Your client source:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: my-client-ns
```

With the operator peer and a valid client path both admitted, the server reports
`Verified=True` and `Available=True`.

## Admin-visible signal

The `MCPServer` surfaces a `NetworkPolicyRestricted` status condition when the
restricted posture is in effect. This reports the **declared posture** - that the
operand generated a deny-by-default ingress policy - not that ingress is
necessarily enforced (enforcement still depends on the network plugin, as noted
above).

> Note: the handshake-based `Verified` condition and `NetworkPolicyRestricted` are
> surfaced on the `mcp.x-k8s.io/v1beta1` representation. The deprecated
> `v1alpha1` conversion drops `Verified` and renames `Available` to `Ready`.
