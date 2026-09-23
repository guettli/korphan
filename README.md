# korphan — find orphan (unmanaged) Kubernetes resources

`korphan` lists resources in a Kubernetes cluster that are managed by **neither
a controller nor a GitOps tool**. In a GitOps-managed cluster every object
should be traceable to a source of truth; anything else was created out-of-band
(a `kubectl apply`, a `kubectl edit`, a leftover from a deleted operator) and is
exactly what this tool surfaces.

It exits non-zero when it finds any, so it fits straight into CI or a periodic
alarm: *"tell me the moment something unmanaged appears in my cluster."*

## What counts as "managed"

A resource is considered **managed** — and therefore *not* reported — when any
of these hold:

1. **Owned** — it has an `ownerReference`. A controller (or another resource)
   created it and owns its lifecycle.
2. **Claimed by a GitOps tool** — it carries the tracking label/annotation of a
   tool `korphan` detects in the cluster:
   - **Flux** — `kustomize.toolkit.fluxcd.io/name`, `helm.toolkit.fluxcd.io/name`
   - **Argo CD** — `argocd.argoproj.io/instance`, `argocd.argoproj.io/tracking-id`
   - **Fleet** — `fleet.cattle.io/bundle-name`
   - **Helm** — `meta.helm.sh/release-name` (also k3s' bundled charts)
   - **Wrangler/objectset** — `objectset.rio.cattle.io/hash` (the k3s deploy &
     helm controllers, Rancher)

   A built-in GitOps tool's labels only count when that tool is actually
   **detected** in the cluster, so a stale label left behind by an uninstalled
   tool cannot mask an orphan.
3. **Control-plane internal** — it belongs to the set the api-server, kubelet,
   or distro create on their own and that no GitOps repo should own: Nodes, the
   `kubernetes` Service/Endpoints, `kube-root-ca.crt`, bootstrap & aggregated
   RBAC, API-server IP allocation (`IPAddress`, `ServiceCIDR`), coordination
   Leases, static/mirror Pods, dynamically-provisioned PVs, helm release
   storage Secrets, anything in the control-plane namespaces (`kube-system`,
   `kube-public`, `kube-node-lease`), and k3s bootstrap resources
   (`k3s.cattle.io`, `helm.cattle.io`). The virtual `metrics.k8s.io` API is
   skipped entirely.

**Debug Pods** are a special case: a bare (ownerless) Pod is tolerated while it
is younger than `--max-debug-pod-age` (default 2h) — a human debugging with
`kubectl run`/`kubectl debug` is fine — but a debug Pod that outlives the grace
period is reported.

Everything else is an **orphan**.

## Install / run

```terminal
go run github.com/guettli/korphan@latest
```

or download a binary from the [releases](https://github.com/guettli/korphan/releases).

## Examples

```terminal
# Scan the current context; exit 1 if any orphan exists.
korphan

# Only a few namespaces, as JSON, for a dashboard.
korphan -n 'app-*,team-*' -o json

# Treat a home-grown GitOps tool's label as "managed".
korphan --manager-label 'mycorp.io/managed-by'

# Accept the irreducible bootstrap secrets a fresh Flux install needs.
korphan --ignore-name-glob 'flux-system,sops-age'

# Fail CI if a resource type could not even be listed (broken aggregated API).
korphan --fail-on-list-errors
```

Exit codes: **0** = no orphans, **1** = orphans found, **3** = error.

## Usage

<!-- usage:start -->
```text
korphan finds orphan (unmanaged) resources in a Kubernetes cluster.

A resource counts as MANAGED when any of these hold:
  - it has an ownerReference (a controller or another resource created it);
  - it carries the tracking label/annotation of a GitOps tool that korphan
    detects in the cluster (Flux, Argo CD, Fleet);
  - it belongs to the built-in set of objects the control plane, kubelet, or
    api-server create on their own (Nodes, the kubernetes Service, bootstrap
    RBAC, root-CA ConfigMaps, static Pods, ...).

Everything else was created out-of-band and is reported as an orphan.

Exit codes: 0 = no orphans, 1 = orphans found, 3 = error.

Usage:
  korphan [flags]

Flags:
      --context string               Name of the kubeconfig context to use
      --exclude-namespace strings    Skip these namespaces (comma-separated globs)
      --fail-on-list-errors          Exit 3 if any resource type could not be listed (e.g. a broken aggregated API)
  -h, --help                         help for korphan
      --ignore-kind strings          Additional kinds to treat as managed (comma-separated, case-insensitive)
      --ignore-name-glob strings     Treat any resource whose name matches one of these globs as managed
      --kubeconfig string            Path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)
      --manager-annotation strings   Extra annotation keys whose presence marks a resource as managed
      --manager-label strings        Extra label keys whose presence marks a resource as managed (for GitOps tools korphan does not know natively)
      --max-debug-pod-age duration   Tolerate an ownerless (debug) Pod younger than this; older ones are reported (default 2h0m0s)
  -n, --namespace strings            Restrict to these namespaces (comma-separated globs); cluster-scoped resources are skipped
  -o, --output string                Output format: table or json (default "table")
  -v, --verbose                      Print the detected managers and scan totals to stderr
      --version                      version for korphan
```
<!-- usage:end -->

## Related

- [dumpall](https://github.com/guettli/dumpall) — dump all Kubernetes resources into a directory tree (and diff them).
- [check-conditions](https://github.com/guettli/check-conditions) — check `status.conditions` of all resources.

## Feedback is welcome

Please create an issue if you have a question or a feature request.
