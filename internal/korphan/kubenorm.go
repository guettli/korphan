package korphan

// Ported from github.com/guettli/kubenorm (owners.go). kubenorm lists every
// system that *seems* to manage an object; korphan needs a yes/no "is it
// reconciled from a source of truth". So only the signals that mean "a
// controller reconciles this" decide managed-ness here (Rancher RBAC owners);
// the rest (kubectl apply, plain helm, managedFields) only explain an orphan.
// Keep the key lists in sync with kubenorm by hand.

import (
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// rancherGroups are API groups whose presence means Rancher (upstream or a
// downstream cluster agent) runs in the cluster.
var rancherGroups = []string{"management.cattle.io", "cluster.cattle.io"}

func rancherInstalled(groups []string) bool {
	for _, g := range rancherGroups {
		if slices.Contains(groups, g) {
			return true
		}
	}
	return false
}

// rancherOwnerValues are label values of Rancher whose key names the owner
// (a "<namespace>_<name>" of a role template binding, or a UID).
var rancherOwnerValues = []string{
	"membership-binding-owner",
	"memberhsip-binding-owner", // Rancher's legacy misspelling
	"prtb-in-cluster-binding-owner",
	"crtb-in-project-binding-owner",
	"owner-user",
}

// rancherOwnerLabels are Rancher labels whose value names the owner.
var rancherOwnerLabels = []struct{ key, what string }{
	{"authz.cluster.cattle.io/rtb-owner-updated", "rtb"},
	{"authz.cluster.cattle.io/rtb-owner", "rtb uid"}, // legacy: UID of the binding
	{"authz.cluster.cattle.io/crtb-owner", "crtb"},
	{"authz.cluster.cattle.io/clusterrole-owner", "roletemplate"},
	{"authz.management.cattle.io/gr-owner", "globalrole"},
	{"authz.management.cattle.io/grb-owner", "globalrolebinding"},
}

// rancherOwned reports the Rancher object that reconciles an RBAC resource
// (role template bindings, role templates, global roles) or an object made
// through the Norman API. Rancher sets no ownerReference on these, and the
// value-style labels ("<ns>_<name>: membership-binding-owner") have no domain
// in the key, so the generic operator-label check cannot see them.
func rancherOwned(labels map[string]string) (string, bool) {
	for _, l := range rancherOwnerLabels {
		if v := labels[l.key]; v != "" {
			return "rancher " + l.what + " " + rancherRTB(v), true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v := labels[k]
		for _, prefix := range []string{"prtb", "crtb"} {
			if name, ok := strings.CutPrefix(k, "authz.cluster.cattle.io/"+prefix+"-owner-"); ok && v == "true" {
				return "rancher " + prefix + " " + rancherRTB(name), true
			}
		}
		if slices.Contains(rancherOwnerValues, v) {
			return "rancher " + v + " " + rancherRTB(k), true
		}
	}
	if labels["cattle.io/creator"] == "norman" {
		return "rancher norman", true
	}
	return "", false
}

// rancherRTB turns "<namespace>_<name>" into "<namespace>/<name>". Rancher
// shortens values longer than 63 characters to a prefix plus a hash.
func rancherRTB(v string) string {
	out := v
	if ns, name, ok := strings.Cut(v, "_"); ok {
		out = ns + "/" + name
	}
	if len(v) == 63 {
		out += " (truncated)"
	}
	return out
}

// orphanHints says how an orphan most likely got into the cluster, so the
// user knows where to look: a plain helm release, `kubectl apply`, a
// managed-by label, or the field managers that wrote its spec. None of these
// make it managed -- they are out-of-band unless a GitOps tool stamped it.
func orphanHints(u *unstructured.Unstructured) []string {
	var hints []string
	add := func(s string) {
		if !slices.Contains(hints, s) {
			hints = append(hints, s)
		}
	}
	labels, annos := u.GetLabels(), u.GetAnnotations()

	if rel := annos["meta.helm.sh/release-name"]; rel != "" {
		if ns := annos["meta.helm.sh/release-namespace"]; ns != "" {
			rel = ns + "/" + rel
		}
		add("helm release " + rel)
	} else if by := labels["app.kubernetes.io/managed-by"]; by == "Helm" {
		add("helm")
	}
	if by := labels["app.kubernetes.io/managed-by"]; by != "" && by != "Helm" {
		add("managed-by " + by)
	}
	if app := labels["kapp.k14s.io/app"]; app != "" {
		add("kapp app " + app)
	}
	if _, ok := annos["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		add("kubectl apply")
	}
	for _, mf := range u.GetManagedFields() {
		// Status updates come from controllers, not from whoever wrote the
		// desired state.
		if mf.Manager != "" && mf.Subresource == "" {
			add("manager " + mf.Manager)
		}
	}
	return hints
}
