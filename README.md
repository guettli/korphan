# korphan — find unmanaged Kubernetes resources

korphan lists resources that are managed by neither a controller nor a GitOps
tool, and exits non-zero if it finds any. Run it in CI or a cron job to catch
resources that were created by hand and are not in git.

The scan is **read-only**: it only lists resources and never changes the cluster,
so it is safe to point at any context. (The optional `korphan ignore` command is
the one exception, and it asks before writing — see [below](#ignoring-a-resource).)

> **korphan's rules now live in [konmon](https://github.com/guettli/konmon).**
> The check [`orphaned`](https://github.com/guettli/konmon/blob/main/checks/orphaned.ts)
> is korphan's classifier, and konmon can also record when an orphan appeared.
> korphan keeps working, but its rules are frozen: false positives, misses and
> new operators go to konmon (`konmon report false-positive --check orphaned
> --resource TYPE`, or an [issue](https://github.com/guettli/konmon/issues)).
>
> ```console
> go run github.com/guettli/konmon@latest check orphaned
> ```
>
> See [Moving to konmon](#moving-to-konmon).

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
     `k8s.io`, `helm.sh` and bare `cattle.io` domains never count.
   - It is a Secret a GitOps controller uses as its own credential: a Flux source
     `secretRef` (git or registry key) or a Flux Kustomization decryption
     `secretRef` (SOPS key), which cannot be stored in git; or an Argo CD
     credential Secret (label `argocd.argoproj.io/secret-type`).
   - It is a cert-manager Secret: a TLS Secret issued from a Certificate
     (annotation `cert-manager.io/certificate-name`), or PKI such as ACME account
     keys and the webhook CA (label `app.kubernetes.io/managed-by=cert-manager`).
   - It is a Role, ClusterRole or binding that Rancher reconciles from a role
     template binding, role template or global role
     (`authz.cluster.cattle.io/*-owner*`, `authz.management.cattle.io/gr-owner`,
     `<ns>_<binding>: membership-binding-owner`, ...). Only when Rancher is
     installed. An object a human created in the Rancher UI
     (`cattle.io/creator=norman`) is out-of-band and is reported.
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

For each orphan, korphan prints **hints** on how it most likely got into the
cluster, so you know where to look: a plain helm release, `kubectl apply`, an
`app.kubernetes.io/managed-by` value, a kapp app, the Rancher UI, a stale
Flux/Argo CD/Fleet label, and the field managers that wrote its spec. Hints never make a resource managed. The tool signals are
shared with [kubenorm](https://github.com/guettli/kubenorm), which shows the
same list when it diffs objects.

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

korphan's rules are frozen. Add the operator to `liqo`/`defaultSkipKinds` in
konmon's [`checks/orphaned.ts`](https://github.com/guettli/konmon/blob/main/checks/orphaned.ts)
instead; until then, use `--skip-kind` here, or `--check-param
'orphaned.skipKinds=["group/Kind"]'` in konmon.

## Moving to konmon

`konmon check orphaned` reports the same resources. The ignore annotation
`korphan.guettli.github.io/ignore` works the same way.

| korphan | konmon |
|---|---|
| `korphan` | `konmon check orphaned` |
| exit code 1 if there are orphans | the same (konmon v0.0.44 or newer: a hygiene check named as an argument sets the exit code) |
| `--context`, `--kubeconfig`, `-n`, `--exclude-namespace`, `-o json` | the same flags |
| `--max-debug-pod-age 2h` | `--check-param orphaned.maxDebugPodAge=2h` |
| `--manager-label K` | `--check-param 'orphaned.managerLabels=["K"]'` |
| `--manager-annotation K` | `--check-param 'orphaned.managerAnnotations=["K"]'` |
| `--skip-kind group/Kind` | `--check-param 'orphaned.skipKinds=["group/Kind"]'` |
| `--ignore-name-glob G` | `--check-param 'orphaned.ignoreNames=["G"]'` |
| `korphan ignore` | `konmon triage orphaned --config konmon.yaml` (v0.0.45 or newer): one reason per orphan, written as ignore rules into the config file instead of annotations into the cluster |

Differences:

- konmon counts a GitOps tool or operator as installed if it may read one of
  its types; korphan uses API discovery. With RBAC that hides a tool's types,
  konmon reports what that tool manages.
- konmon reads only the metadata of Secrets, so it has no "kubectl apply" hint
  for them, and no "manager" hints at all.
- `konmon run` keeps the findings in a database: `konmon findings` shows when
  an orphan appeared and was resolved.

## Related

- [dumpall](https://github.com/guettli/dumpall): dump all Kubernetes resources into a directory tree.
- [check-conditions](https://github.com/guettli/check-conditions): check `status.conditions` of all resources.
