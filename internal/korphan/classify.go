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

	managerFlux      = "flux"
	managerArgo      = "argocd"
	managerFleet     = "fleet"
	managerHelm      = "helm"
	managerObjectset = "objectset"
	managerCustom    = "custom"

	nsDefault      = "default"
	nameKubernetes = "kubernetes"
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
func classify(u *unstructured.Unstructured, gvk schema.GroupVersionKind, active []Manager, opts Options) (managed, tolerated bool, reason string) {
	// 1. Owned by another resource (controller or plain owner reference).
	if refs := u.GetOwnerReferences(); len(refs) > 0 {
		owner := refs[0]
		return true, false, "owned by " + owner.Kind + "/" + owner.Name
	}

	// 2. Claimed by a detected GitOps manager.
	if m, ok := managedByManager(u.GetLabels(), u.GetAnnotations(), active); ok {
		return true, false, "managed by " + m
	}

	// 3. Built-in kube-internal / control-plane-created resources.
	if r, ok := isKubeInternal(u, gvk); ok {
		return true, false, r
	}

	// 4. User-supplied ignore rules.
	for _, ik := range opts.IgnoreKinds {
		if strings.EqualFold(ik, gvk.Kind) {
			return true, false, "ignored kind"
		}
	}
	if anyGlob(opts.IgnoreNameGlobs, u.GetName()) {
		return true, false, "ignored name"
	}

	// 5. Bare debug Pods within their grace age are tolerated.
	if gvk.Kind == "Pod" && gvk.Group == "" {
		age := opts.Now.Sub(u.GetCreationTimestamp().Time)
		if age < opts.MaxDebugPodAge {
			return false, true, "debug pod within grace age"
		}
		return false, false, "unmanaged Pod older than --max-debug-pod-age"
	}

	return false, false, "no owner, no GitOps manager"
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
	case kind == "Secret" && strings.HasPrefix(name, "sh.helm.release.v1."):
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
