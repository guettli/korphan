package korphan

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Well-known metadata keys and manager names, named once.
const (
	labelFluxKustomize = "kustomize.toolkit.fluxcd.io/name"
	labelFluxHelm      = "helm.toolkit.fluxcd.io/name"
	labelArgoInstance  = "argocd.argoproj.io/instance"
	annoArgoTracking   = "argocd.argoproj.io/tracking-id"
	labelFleetBundle   = "fleet.cattle.io/bundle-name"
	annoHelmRelease    = "meta.helm.sh/release-name"
	labelObjectsetHash = "objectset.rio.cattle.io/hash"
	annoCertManager    = "cert-manager.io/certificate-name"

	managerFlux        = "flux"
	managerArgo        = "argocd"
	managerFleet       = "fleet"
	managerHelm        = "helm"
	managerObjectset   = "objectset"
	managerCertManager = "cert-manager"
	managerCustom      = "custom"

	nsDefault      = "default"
	nameKubernetes = "kubernetes"
	kindSecret     = "Secret"

	groupLiqoCore    = "core.liqo.io"
	groupLiqoAuth    = "authentication.liqo.io"
	groupLiqoIPAM    = "ipam.liqo.io"
	groupLiqoNet     = "networking.liqo.io"
	groupLiqoOffload = "offloading.liqo.io"
)

// neverList names resources that are listable but never orphan-relevant and
// often enormous, so we skip listing them entirely.
var neverList = map[string]bool{
	"events":            true, // core/v1 Events, high volume, TTL'd
	"componentstatuses": true, // virtual, deprecated
}

// neverListGroup names whole API groups that are virtual/computed aggregated
// APIs: their "objects" are synthesized per request, not stored, so they can be
// neither owned nor GitOps-managed.
var neverListGroup = map[string]bool{
	"metrics.k8s.io": true, // PodMetrics / NodeMetrics from metrics-server
}

// builtinAPIGroups are the API groups that ship with Kubernetes itself. Every
// other group discovered in the cluster was added by an operator/controller
// (via a CRD or an aggregated API server), which is what operatorGroups keys
// on. Keep this in sync with upstream group registrations.
var builtinAPIGroups = map[string]bool{
	"":                             true, // core/v1
	"apps":                         true,
	"batch":                        true,
	"autoscaling":                  true,
	"policy":                       true,
	"extensions":                   true,
	"admissionregistration.k8s.io": true,
	"apiextensions.k8s.io":         true,
	"apiregistration.k8s.io":       true,
	"apidiscovery.k8s.io":          true,
	"authentication.k8s.io":        true,
	"authorization.k8s.io":         true,
	"certificates.k8s.io":          true,
	"coordination.k8s.io":          true,
	"discovery.k8s.io":             true,
	"events.k8s.io":                true,
	"flowcontrol.apiserver.k8s.io": true,
	"internal.apiserver.k8s.io":    true,
	"networking.k8s.io":            true,
	"node.k8s.io":                  true,
	"rbac.authorization.k8s.io":    true,
	"scheduling.k8s.io":            true,
	"storage.k8s.io":               true,
	"storagemigration.k8s.io":      true,
	"resource.k8s.io":              true,
	"metrics.k8s.io":               true,
	"admission.k8s.io":             true,
}

// systemNamespaces are the control-plane's own namespaces. Unstamped, unowned
// objects here belong to kube-controller-manager / the api-server / the distro
// (built-in controller ServiceAccounts, extension-apiserver-authentication,
// k3s-serving, ...). GitOps-managed objects that happen to live here are still
// caught earlier by their manager label, which classify checks first.
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// managerSignature describes how a GitOps tool stamps the resources it owns and
// how to detect that the tool is installed at all.
type managerSignature struct {
	name        string
	labels      []string // presence of any => managed by this tool
	annotations []string
	// detect returns true if the tool's API group is present in the cluster.
	detect func(groups map[string]bool) bool
}

var signatures = []managerSignature{
	{
		name:   managerFlux,
		labels: []string{labelFluxKustomize, labelFluxHelm},
		detect: func(g map[string]bool) bool {
			for group := range g {
				if strings.HasSuffix(group, "toolkit.fluxcd.io") {
					return true
				}
			}
			return false
		},
	},
	{
		name:        managerArgo,
		labels:      []string{labelArgoInstance},
		annotations: []string{annoArgoTracking},
		detect:      func(g map[string]bool) bool { return g["argoproj.io"] },
	},
	{
		name:   managerFleet,
		labels: []string{labelFleetBundle},
		detect: func(g map[string]bool) bool { return g["fleet.cattle.io"] },
	},
	{
		// Helm-installed resources (including k3s's helm-controller HelmCharts)
		// carry this annotation. It is Helm-specific, so it needs no detection.
		name:        managerHelm,
		annotations: []string{annoHelmRelease},
		detect:      func(map[string]bool) bool { return true },
	},
	{
		// cert-manager issues TLS Secrets from a Certificate but, by default,
		// sets no ownerReference on them; the Certificate controller reconciles
		// them via this annotation. Honor it when cert-manager is installed.
		name:        managerCertManager,
		annotations: []string{annoCertManager},
		detect:      func(g map[string]bool) bool { return g["cert-manager.io"] },
	},
	{
		// Anything applied by a Rancher/Wrangler "apply" controller -- the k3s
		// deploy controller (Addons), the k3s helm-controller, Fleet -- carries
		// this hash label and is actively reconciled and pruned by that
		// controller. That is controller management, so honor it always.
		name:   managerObjectset,
		labels: []string{labelObjectsetHash},
		detect: func(map[string]bool) bool { return true },
	},
}

// detectManagers determines which GitOps tools are present and folds in any
// user-supplied custom manager keys (always considered detected).
func detectManagers(lists []*metav1.APIResourceList, opts Options) []Manager {
	groups := map[string]bool{}
	for _, rl := range lists {
		if rl == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err == nil && gv.Group != "" {
			groups[gv.Group] = true
		}
	}
	managers := make([]Manager, 0, len(signatures)+1)
	for _, s := range signatures {
		managers = append(managers, Manager{
			Name:        s.name,
			Detected:    s.detect(groups),
			Labels:      s.labels,
			Annotations: s.annotations,
		})
	}
	if len(opts.ManagerLabels) > 0 || len(opts.ManagerAnnotations) > 0 {
		managers = append(managers, Manager{
			Name:        managerCustom,
			Detected:    true,
			Labels:      opts.ManagerLabels,
			Annotations: opts.ManagerAnnotations,
		})
	}
	return managers
}

// defaultSkipKinds are Group/Kind tuples treated as operator-owned and skipped
// by default. Some operators create and reconcile custom resources with no
// ownerReference and no GitOps stamp -- korphan would otherwise flag them.
// Listing the exact kinds (rather than trusting "any custom resource is
// managed") keeps each skip a deliberate, reviewable decision.
//
// These are liqo's CRDs: liqo maintains them as cross-cluster peering state.
// This is the one ecosystem-specific default baked in; extend it with
// --skip-kind, and the whole operator step (this list included) is gated by
// --detect-operators.
var defaultSkipKinds = []schema.GroupKind{
	{Group: groupLiqoCore, Kind: "ForeignCluster"},
	{Group: groupLiqoAuth, Kind: "Identity"},
	{Group: groupLiqoAuth, Kind: "Renew"},
	{Group: groupLiqoAuth, Kind: "ResourceSlice"},
	{Group: groupLiqoAuth, Kind: "Tenant"},
	{Group: groupLiqoIPAM, Kind: "IP"},
	{Group: groupLiqoIPAM, Kind: "Network"},
	{Group: groupLiqoNet, Kind: "Configuration"},
	{Group: groupLiqoNet, Kind: "Connection"},
	{Group: groupLiqoNet, Kind: "FirewallConfiguration"},
	{Group: groupLiqoNet, Kind: "GatewayClient"},
	{Group: groupLiqoNet, Kind: "GatewayServer"},
	{Group: groupLiqoNet, Kind: "GeneveTunnel"},
	{Group: groupLiqoNet, Kind: "InternalFabric"},
	{Group: groupLiqoNet, Kind: "InternalNode"},
	{Group: groupLiqoNet, Kind: "PublicKey"},
	{Group: groupLiqoNet, Kind: "RouteConfiguration"},
	{Group: groupLiqoNet, Kind: "WgGatewayClient"},
	{Group: groupLiqoNet, Kind: "WgGatewayClientTemplate"},
	{Group: groupLiqoNet, Kind: "WgGatewayServer"},
	{Group: groupLiqoNet, Kind: "WgGatewayServerTemplate"},
	{Group: groupLiqoOffload, Kind: "NamespaceMap"},
	{Group: groupLiqoOffload, Kind: "NamespaceOffloading"},
	{Group: groupLiqoOffload, Kind: "Quota"},
	{Group: groupLiqoOffload, Kind: "ShadowEndpointSlice"},
	{Group: groupLiqoOffload, Kind: "ShadowPod"},
	{Group: groupLiqoOffload, Kind: "VirtualNode"},
	{Group: groupLiqoOffload, Kind: "VkOptionsTemplate"},
}

// buildSkipKinds merges the baked-in defaults with any user-supplied "group/Kind"
// (or bare "Kind" for the core group) entries into a lookup set.
func buildSkipKinds(userSkips []string) map[schema.GroupKind]bool {
	set := make(map[schema.GroupKind]bool, len(defaultSkipKinds)+len(userSkips))
	for _, gk := range defaultSkipKinds {
		set[gk] = true
	}
	for _, s := range userSkips {
		if gk, ok := parseGroupKind(s); ok {
			set[gk] = true
		}
	}
	return set
}

// parseGroupKind parses "group/Kind" (or bare "Kind" for the core group) into a
// GroupKind. A group never contains "/", so a single split is unambiguous.
func parseGroupKind(s string) (schema.GroupKind, bool) {
	group, kind, found := strings.Cut(s, "/")
	if !found {
		group, kind = "", group // bare "Kind" -> core group
	}
	if kind == "" {
		return schema.GroupKind{}, false
	}
	return schema.GroupKind{Group: group, Kind: kind}, true
}

// operatorGroups returns the API groups that were added to the cluster by an
// operator or controller -- every discovered group that is not a built-in
// Kubernetes group. These are the domains that, appearing as a label or
// annotation key, mark a resource as reconciled by that operator.
func operatorGroups(lists []*metav1.APIResourceList) []string {
	seen := map[string]bool{}
	var groups []string
	for _, rl := range lists {
		if rl == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err != nil || gv.Group == "" || builtinAPIGroups[gv.Group] || seen[gv.Group] {
			continue
		}
		seen[gv.Group] = true
		groups = append(groups, gv.Group)
	}
	return groups
}

// conventionDomain reports whether a label/annotation key domain is a
// Kubernetes-wide convention (kubernetes.io, k8s.io, helm.sh families) rather
// than an operator-ownership signal. Such keys appear on hand-created resources
// too, so they must never mark a resource as operator-managed.
func conventionDomain(domain string) bool {
	switch {
	case domain == "kubernetes.io" || strings.HasSuffix(domain, ".kubernetes.io"):
		return true
	case domain == "k8s.io" || strings.HasSuffix(domain, ".k8s.io"):
		return true
	case domain == "helm.sh" || strings.HasSuffix(domain, ".helm.sh"):
		return true
	}
	return false
}

// keyDomain returns the domain part of a label/annotation key ("liqo.io" for
// "liqo.io/remote-cluster-id"), or "" for an unprefixed key.
func keyDomain(key string) string {
	if domain, _, found := strings.Cut(key, "/"); found {
		return domain
	}
	return ""
}

// domainMatchesGroup reports whether a key domain belongs to an operator group,
// treating parent and child domains as a match: the label "liqo.io/x" (domain
// "liqo.io") belongs to the operator whose CRD group is "networking.liqo.io",
// and vice versa.
func domainMatchesGroup(domain, group string) bool {
	return domain == group ||
		strings.HasSuffix(group, "."+domain) ||
		strings.HasSuffix(domain, "."+group)
}

// managedByOperatorLabel reports the operator domain that owns a core/built-in
// object, inferred from a LABEL whose domain matches an installed operator's API
// group -- e.g. liqo stamps `liqo.io/managed` on the Secrets, RBAC and
// Deployments it creates.
//
// Annotations are deliberately NOT used: operators routinely read a
// user-authored annotation off a user-owned object without owning it
// (`cert-manager.io/cluster-issuer` on a hand-made Ingress, metallb/traefik
// config annotations), and keying on those would hide genuine orphans. Labels
// in an operator's domain are, in practice, operator-applied ownership markers.
// The kubernetes.io/k8s.io/helm.sh convention domains never count.
//
// An operator's custom *resources* (its own CRD kinds) are handled separately
// by the explicit skip list (see defaultSkipKinds), not here -- so a
// hand-applied CR that is not on the list is still reported.
func managedByOperatorLabel(labels map[string]string, groups []string) (string, bool) {
	for key := range labels {
		domain := keyDomain(key)
		if domain == "" || !strings.Contains(domain, ".") || conventionDomain(domain) {
			continue
		}
		for _, g := range groups {
			if domainMatchesGroup(domain, g) {
				return domain, true
			}
		}
	}
	return "", false
}

// activeManagers keeps only the detected managers, so a stale label left behind
// by an uninstalled tool cannot mask an orphan.
func activeManagers(managers []Manager) []Manager {
	active := make([]Manager, 0, len(managers))
	for _, m := range managers {
		if m.Detected {
			active = append(active, m)
		}
	}
	return active
}

// classify decides whether a single object is managed. It returns:
//   - managed:   true if owned by another resource, claimed by a detected
//     manager, or part of the built-in kube-internal set.
//   - tolerated: true if it is an ownerless debug Pod still within its grace age
//     (counted separately, neither managed nor an orphan).
//   - reason:    a human-readable classification, shown for orphans (why it is
//     flagged) and, in verbose mode, for managed resources (why it is not).
//
// detectors bundles the cluster-derived inputs classify needs, computed once
// per scan.
type detectors struct {
	active         []Manager                 // detected GitOps-tool signatures
	operatorGroups []string                  // installed non-built-in API groups
	skipKinds      map[schema.GroupKind]bool // Group/Kinds to treat as operator-owned
	infraSecrets   map[string]bool           // "ns/name" of GitOps credential Secrets
}

func classify(u *unstructured.Unstructured, gvk schema.GroupVersionKind, det detectors, opts Options) (managed, tolerated bool, reason string) {
	// 1. Owned by another resource (controller or plain owner reference).
	if refs := u.GetOwnerReferences(); len(refs) > 0 {
		owner := refs[0]
		return true, false, "owned by " + owner.Kind + "/" + owner.Name
	}

	labels, annos := u.GetLabels(), u.GetAnnotations()

	// 2. Claimed by a detected GitOps manager.
	if m, ok := managedByManager(labels, annos, det.active); ok {
		return true, false, "managed by " + m
	}

	// 3. Built-in kube-internal / control-plane-created resources.
	if r, ok := isKubeInternal(u, gvk); ok {
		return true, false, r
	}

	// 4. Operator- or bootstrap-owned (gated by --detect-operators). Runs after
	// the kube-internal check so control-plane objects keep their precise reason.
	if opts.DetectOperators {
		if r, ok := operatorOwned(u, gvk, labels, det); ok {
			return true, false, r
		}
	}

	// 5. User-supplied ignore rules.
	for _, ik := range opts.IgnoreKinds {
		if strings.EqualFold(ik, gvk.Kind) {
			return true, false, "ignored kind"
		}
	}
	if anyGlob(opts.IgnoreNameGlobs, u.GetName()) {
		return true, false, "ignored name"
	}

	// 6. Bare debug Pods/Jobs within their grace age are tolerated: a human
	// debugging (`kubectl run`) or an automation firing a one-off Job creates
	// ownerless objects that are fine briefly but must not become permanent.
	if (gvk.Kind == "Pod" && gvk.Group == "") || (gvk.Kind == "Job" && gvk.Group == "batch") {
		age := opts.Now.Sub(u.GetCreationTimestamp().Time)
		if age < opts.MaxDebugPodAge {
			return false, true, "young ownerless " + gvk.Kind + " (within grace age)"
		}
		return false, false, "ownerless " + gvk.Kind + " older than --max-debug-pod-age"
	}

	return false, false, "no owner, no GitOps manager"
}

// operatorOwned recognizes objects an operator or GitOps bootstrap owns but
// leaves without an ownerReference:
//   - a Kind on the skip list (e.g. liqo's peering CRDs);
//   - a Secret a GitOps controller references as its own credential -- the git
//     deploy key or the SOPS decryption key -- which by definition cannot live
//     in git; likewise an Argo CD repository/cluster credential Secret;
//   - cert-manager's generated runtime PKI (account keys, webhook CA), which it
//     stamps with app.kubernetes.io/managed-by=cert-manager*;
//   - a core object carrying a label in an installed operator's domain.
func operatorOwned(u *unstructured.Unstructured, gvk schema.GroupVersionKind, labels map[string]string, det detectors) (string, bool) {
	if det.skipKinds[gvk.GroupKind()] {
		return "skipped operator kind (" + gvk.Group + "/" + gvk.Kind + ")", true
	}
	if gvk.Kind == kindSecret && gvk.Group == "" {
		if det.infraSecrets[u.GetNamespace()+"/"+u.GetName()] {
			return "GitOps bootstrap credential (referenced by a GitOps source/decryption)", true
		}
		if _, ok := labels["argocd.argoproj.io/secret-type"]; ok {
			return "Argo CD credential", true
		}
	}
	if mb := labels["app.kubernetes.io/managed-by"]; strings.HasPrefix(mb, "cert-manager") {
		return "cert-manager runtime", true
	}
	if domain, ok := managedByOperatorLabel(labels, det.operatorGroups); ok {
		return "managed by operator (" + domain + ")", true
	}
	return "", false
}

// managedByManager checks a resource's labels/annotations against the active
// manager matchers.
func managedByManager(labels, annos map[string]string, active []Manager) (string, bool) {
	for _, m := range active {
		for _, k := range m.Labels {
			if _, ok := labels[k]; ok {
				return m.Name, true
			}
		}
		for _, k := range m.Annotations {
			if _, ok := annos[k]; ok {
				return m.Name, true
			}
		}
	}
	return "", false
}

// isKubeInternal recognizes resources the control plane, kubelet, or api-server
// legitimately create with no ownerReference. These are the "some kube internal
// resources need to be skipped" cases: not GitOps material, not orphans.
func isKubeInternal(u *unstructured.Unstructured, gvk schema.GroupVersionKind) (string, bool) {
	// Anything the control plane keeps in its own namespaces (built-in
	// controller ServiceAccounts, api-server ConfigMaps, k3s-serving, ...).
	if ns := u.GetNamespace(); ns != "" && systemNamespaces[ns] {
		return "control-plane namespace (" + ns + ")", true
	}
	if r, ok := clusterInternal(u, gvk); ok {
		return r, true
	}
	if r, ok := rbacInternal(u, gvk); ok {
		return r, true
	}
	return seededInternal(u, gvk)
}

// clusterInternal covers cluster-scoped objects the control plane and distro
// own outright.
func clusterInternal(u *unstructured.Unstructured, gvk schema.GroupVersionKind) (string, bool) {
	name := u.GetName()
	switch gvk.Kind {
	case "Node", "APIService", "CertificateSigningRequest":
		return "cluster infrastructure (" + gvk.Kind + ")", true
	case "IPAddress", "ServiceCIDR":
		return "api-server IP allocation", true
	case "PersistentVolume":
		if _, ok := u.GetAnnotations()["pv.kubernetes.io/provisioned-by"]; ok {
			return "dynamically provisioned PV", true
		}
	case "CustomResourceDefinition":
		if strings.HasSuffix(name, ".k3s.cattle.io") || strings.HasSuffix(name, ".helm.cattle.io") {
			return "k3s bootstrap CRD", true
		}
	case "Namespace":
		if name == nsDefault || strings.HasPrefix(name, "kube-") {
			return "system namespace", true
		}
	case "PriorityClass":
		if strings.HasPrefix(name, "system-") {
			return "system PriorityClass", true
		}
	}
	switch gvk.Group {
	case "flowcontrol.apiserver.k8s.io":
		return "API Priority & Fairness default", true
	case "k3s.cattle.io", "helm.cattle.io":
		// k3s bootstrap roots: the deploy controller reads Addon/HelmChart CRs
		// from /var/lib/rancher/k3s/server/manifests and applies them. The root
		// CRs have no owner and no stamp -- they are the k3s install itself.
		return "k3s bootstrap resource", true
	case "coordination.k8s.io":
		if gvk.Kind == "Lease" {
			return "coordination Lease", true
		}
	}
	return "", false
}

// rbacInternal covers the RBAC the api-server bootstraps and aggregates.
func rbacInternal(u *unstructured.Unstructured, gvk schema.GroupVersionKind) (string, bool) {
	switch gvk.Kind {
	case "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
	default:
		return "", false
	}
	name := u.GetName()
	if u.GetLabels()["kubernetes.io/bootstrapping"] == "rbac-defaults" {
		return "bootstrap RBAC", true
	}
	if strings.HasPrefix(name, "system:") {
		return "system RBAC", true
	}
	if gvk.Kind == "ClusterRole" {
		if _, ok := u.Object["aggregationRule"]; ok {
			return "aggregated ClusterRole", true
		}
	}
	return "", false
}

// seededInternal covers per-namespace objects the control plane seeds
// automatically, plus kubelet-managed static Pods.
func seededInternal(u *unstructured.Unstructured, gvk schema.GroupVersionKind) (string, bool) {
	name := u.GetName()
	ns := u.GetNamespace()
	kind := gvk.Kind
	switch {
	// The root-CA ConfigMap is "kube-root-ca.crt", but liqo reflects it into
	// offloaded namespaces with a remote-cluster suffix, so match by prefix.
	case kind == "ConfigMap" && strings.HasPrefix(name, "kube-root-ca.crt"):
		return "root CA ConfigMap", true
	case kind == kindSecret && strings.HasPrefix(name, "sh.helm.release.v1."):
		return "helm release storage", true
	case kind == "ServiceAccount" && name == nsDefault:
		return "default ServiceAccount", true
	case kind == "Service" && ns == nsDefault && name == nameKubernetes:
		return "kubernetes API Service", true
	case kind == "Endpoints" && ns == nsDefault && name == nameKubernetes:
		return "kubernetes API Endpoints", true
	case kind == "EndpointSlice" && ns == nsDefault && u.GetLabels()["kubernetes.io/service-name"] == nameKubernetes:
		return "kubernetes API EndpointSlice", true
	case kind == "Pod":
		// Static / mirror Pods (k3s & kubeadm control-plane components) are
		// managed by the kubelet from on-disk manifests, not the api-server.
		if _, ok := u.GetAnnotations()["kubernetes.io/config.mirror"]; ok {
			return "static/mirror Pod", true
		}
	}
	return "", false
}
