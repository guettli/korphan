# korphan — find unmanaged Kubernetes resources

korphan lists resources that are managed by neither a controller nor a GitOps
tool, and exits non-zero if it finds any. Run it in CI or a cron job to catch
resources that were created by hand and are not in git.

The scan is **read-only**: it only lists resources and never changes the cluster,
so it is safe to point at any context. (The optional `korphan ignore` command is
the one exception, and it asks before writing — see [below](#ignoring-a-resource).)

> **Feedback wanted.** korphan is young and I'm looking for testers. Trying it is
> a single read-only command (see [Install](#install)). I'd especially like to
> hear about **false positives** (korphan flagged something a tool actually
> manages — which tool stamped it?), **misses** (an obvious hand-made orphan it
> stayed quiet about), or anything confusing in the output. Share what you find
> in the [feedback discussion](https://github.com/guettli/korphan/discussions/5) —
> even a one-line "ran it on my cluster, here's what it found" is useful. Bugs and
> feature ideas fit best as [issues](https://github.com/guettli/korphan/issues).

## What counts as managed

A resource is managed, and not reported, if any of these is true.

1. It has an ownerReference. A controller or another resource created it.

2. It carries the tracking label or annotation of a GitOps tool that is
   installed in the cluster:

   - Flux: `kustomize.toolkit.fluxcd.io/name`, `helm.toolkit.fluxcd.io/name`
   - Argo CD: `argocd.argoproj.io/instance`, `argocd.argoproj.io/tracking-id`
   - Fleet: `fleet.cattle.io/bundle-name`
   - Rancher / Wrangler: `objectset.rio.cattle.io/hash`

   For Flux, Argo CD and Fleet the labels only count when that tool is installed,
   so a stale label left by an uninstalled tool does not hide an orphan.

   You can extend this list with `--manager-label` and `--manager-annotation`,
   whose keys mark a resource as managed. This is the generic escape hatch for
   any tool korphan does not know natively. For example, a plain `helm install`
   (as opposed to a GitOps-driven Flux `HelmRelease` or Argo CD `Application`) is
   an out-of-band change that is not in git, so korphan reports it by default. If
   you consider such releases managed, allow them with:

   ```
   korphan --manager-annotation 'meta.helm.sh/release-name'
   ```

3. It is reconciled by an operator that sets no ownerReference:

   - Its kind is on the skip list. korphan ships liqo's CRDs; add more with
     `--skip-kind group/Kind`.
   - It carries a label whose domain matches an installed operator's API group,
     for example `liqo.io/managed`. Annotations are not used, because operators
     often read an annotation off a resource they do not own (e.g.
     `cert-manager.io/cluster-issuer` on a hand-made Ingress). The `kubernetes.io`,
     `k8s.io` and `helm.sh` domains never count.
   - It is a Secret a GitOps controller uses as its own credential: a Flux source
     `secretRef` (git or registry key) or a Flux Kustomization decryption
     `secretRef` (SOPS key), which cannot be stored in git; or an Argo CD
     credential Secret (label `argocd.argoproj.io/secret-type`).
   - It is a cert-manager Secret: a TLS Secret issued from a Certificate
     (annotation `cert-manager.io/certificate-name`), or PKI such as ACME account
     keys and the webhook CA (label `app.kubernetes.io/managed-by=cert-manager`).
   - It is a known operator's runtime-state object, matched by a distinctive
     label: for example Stakater Reloader's meta-info ConfigMap
     (`reloader.stakater.com/meta-info`) or liqo's telemetry-identity ConfigMap.
     See `operatorStateLabels` in `internal/korphan/classify.go`.

4. It is created by the control plane, the kubelet, or the distribution: Nodes,
   the `kubernetes` Service and Endpoints, `kube-root-ca.crt`, bootstrap and
   aggregated RBAC, `IPAddress` and `ServiceCIDR`, coordination Leases, static
   Pods, dynamically provisioned PersistentVolumes, Helm release Secrets,
   anything in `kube-system`, `kube-public` or `kube-node-lease`, and k3s
   bootstrap resources (`k3s.cattle.io`, `helm.cattle.io`). The `metrics.k8s.io`
   API is skipped.

An ownerless Pod or one-off Job younger than `--max-debug-pod-age` (default 2h)
is tolerated. Everything else is reported.

## Ignoring a resource

To exempt a specific resource, annotate it with a reason:

```
korphan.guettli.github.io/ignore: "rotated out-of-band by task k8s-mint-tenant-jwt"
```

korphan then skips it and shows the reason. The reason lives on the object, so
`kubectl get -o yaml` explains the exemption. An **empty** value is not honored:
the resource is still reported and korphan prints a warning, so an ignore without
a stated reason never passes silently.

`korphan ignore` writes this annotation for you: it walks the orphans and, for
each, prompts for a reason (empty leaves it untouched). Unlike the scan it writes
to the cluster, so it needs a context that can patch the listed resources. Only
annotate a resource whose creation you control; for one an operator regenerates,
add the annotation where it is created, not by hand.

## Install

```
go run github.com/guettli/korphan@latest
```

Or download a binary from the [releases](https://github.com/guettli/korphan/releases) page.

## Examples

```
# Scan the current context.
korphan

# A few namespaces, as JSON.
korphan -n 'app-*,team-*' -o json

# Add a custom GitOps tool's label.
korphan --manager-label 'mycorp.io/managed-by'

# Treat plain `helm install` releases as managed (not reported).
korphan --manager-annotation 'meta.helm.sh/release-name'

# Ignore a resource by name.
korphan --ignore-name-glob 'my-bootstrap-*'

# Treat another operator's custom resources as managed.
korphan --skip-kind 'acme.example.com/Widget'
```

Exit codes: 0 = none found, 1 = orphans found, 3 = error.

## Usage

<!-- usage:start -->
```text
korphan finds orphan (unmanaged) resources in a Kubernetes cluster.

A resource counts as MANAGED when any of these hold:
  - it has an ownerReference (a controller or another resource created it);
  - it carries the tracking label/annotation of a GitOps tool that korphan
    detects in the cluster (Flux, Argo CD, Fleet, cert-manager, ...);
  - it belongs to the built-in set of objects the control plane, kubelet, or
    api-server create on their own (Nodes, the kubernetes Service, bootstrap
    RBAC, root-CA ConfigMaps, static Pods, ...).

Everything else was created out-of-band and is reported as an orphan.

Exit codes: 0 = no orphans, 1 = orphans found, 3 = error.

Usage:
  korphan [flags]
  korphan [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  help        Help about any command
  ignore      Walk the orphans and record a reason to ignore each (writes an annotation)

Flags:
      --context string               Name of the kubeconfig context to use
      --exclude-namespace strings    Skip these namespaces (comma-separated globs)
  -h, --help                         help for korphan
      --ignore-name-glob strings     Treat any resource whose name matches one of these globs as managed
      --kubeconfig string            Path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)
      --manager-annotation strings   Extra annotation keys whose presence marks a resource as managed (e.g. 'meta.helm.sh/release-name' to treat helm-installed resources as managed)
      --manager-label strings        Extra label keys whose presence marks a resource as managed (for GitOps tools korphan does not know natively)
      --max-debug-pod-age duration   Tolerate an ownerless (debug) Pod or one-off Job younger than this; older ones are reported (default 2h0m0s)
  -n, --namespace strings            Restrict to these namespaces (comma-separated globs); cluster-scoped resources are skipped
  -o, --output string                Output format: table or json (default "table")
      --skip-kind strings            Extra 'group/Kind' tuples to treat as operator-owned, on top of the built-in list (e.g. 'networking.liqo.io/Configuration')
  -v, --verbose                      Print the detected managers and scan totals to stderr
      --version                      version for korphan

Use "korphan [command] --help" for more information about a command.
```
<!-- usage:end -->

## Adding an operator to the skip list

If korphan reports custom resources that a known operator owns and reconciles,
add its `group/Kind` tuples to `defaultSkipKinds` in
`internal/korphan/classify.go` and open a pull request, so other users get them
too.

## Related

- [dumpall](https://github.com/guettli/dumpall): dump all Kubernetes resources into a directory tree.
- [check-conditions](https://github.com/guettli/check-conditions): check `status.conditions` of all resources.
