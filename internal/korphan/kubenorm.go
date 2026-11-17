package korphan

// Ported from github.com/guettli/kubenorm (owners.go). kubenorm lists every
// system that *seems* to manage an object; korphan needs a yes/no "is it
// reconciled from a source of truth". So only Rancher's RBAC owner labels
// decide managed-ness here; everything else (plain helm, kubectl apply, kapp
// CLI, Rancher UI/Norman, field managers, stale GitOps labels) only explains
// an orphan. Keep the key lists in sync with kubenorm by hand.

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// rancherGroups are API groups whose presence means Rancher runs in the
// cluster: management.cattle.io upstream, cluster.cattle.io on a downstream
// cluster (cluster agent).
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

// rancherOwnerLabels are Rancher labels whose value names the owner. The
// generic operator-label check (managedByOperatorLabel) already matches these
// domains to the Rancher groups; listing them here gives a precise reason.
var rancherOwnerLabels = []struct{ key, what string }{
	{"authz.cluster.cattle.io/rtb-owner-updated", "rtb"},
	{"authz.cluster.cattle.io/rtb-owner", "rtb uid"}, // legacy: UID of the binding
	{"authz.cluster.cattle.io/crtb-owner", "crtb"},
	{"authz.cluster.cattle.io/clusterrole-owner", "roletemplate"},
	{"authz.management.cattle.io/gr-owner", "globalrole"},
	{"authz.management.cattle.io/grb-owner", "globalrolebinding"},
}

// rancherOwned reports the Rancher object that reconciles an RBAC resource
// (role template bindings, role templates, global roles). Rancher sets no
// ownerReference on these. The value-style labels
// ("<ns>_<name>: membership-binding-owner") have no domain in the key, so the
// generic operator-label check cannot see them. Callers apply it to RBAC kinds
// only, and the key must look like Rancher's (no "/", "<ns>_<name>" or a UID),
// so a hand-written "team: owner-user" does not hide an orphan.
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
		if slices.Contains(rancherOwnerValues, v) && rancherOwnerKey(k) {
			return "rancher " + v + " " + rancherRTB(k), true
		}
	}
	return "", false
}

// rancherOwnerKey reports whether a label key looks like the owner key Rancher
// writes: "<namespace>_<name>" of a binding, or the UID of a user.
func rancherOwnerKey(k string) bool {
	if strings.Contains(k, "/") {
		return false
	}
	if ns, name, ok := strings.Cut(k, "_"); ok && ns != "" && name != "" {
		return true
	}
	return looksLikeUID(k)
}

// looksLikeUID matches a Kubernetes UID (8-4-4-4-12 hex) or a Rancher user id
// ("u-" plus lowercase alphanumerics).
func looksLikeUID(s string) bool {
	if rest, ok := strings.CutPrefix(s, "u-"); ok {
		return rest != "" && !strings.ContainsFunc(rest, func(r rune) bool {
			return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", r)
		})
	}
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	for i, n := range []int{8, 4, 4, 4, 12} {
		if len(parts[i]) != n || strings.ContainsFunc(parts[i], func(r rune) bool {
			return !strings.ContainsRune("0123456789abcdef", r)
		}) {
			return false
		}
	}
	return true
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
// user knows where to look. None of these make it managed: a plain helm
// release, kubectl apply, a kapp CLI deploy or a Rancher UI create is
// out-of-band, and a GitOps label only counts while that tool is installed.
func orphanHints(u *unstructured.Unstructured) []string {
	var hints []string
	add := func(s string) {
		// Values come from the object: keep the output on one line.
		if strings.ContainsFunc(s, unicode.IsControl) {
			s = strconv.Quote(s)
		}
		if !slices.Contains(hints, s) {
			hints = append(hints, s)
		}
	}
	labels, annos := u.GetLabels(), u.GetAnnotations()

	switch rel, by := annos["meta.helm.sh/release-name"], labels["app.kubernetes.io/managed-by"]; {
	case rel != "":
		if ns := annos["meta.helm.sh/release-namespace"]; ns != "" {
			rel = ns + "/" + rel
		}
		add("helm release " + rel)
	case by == "Helm":
		add("helm")
	}
	if by := labels["app.kubernetes.io/managed-by"]; by != "" && by != "Helm" {
		add("managed-by " + by)
	}
	// Stale GitOps labels: the tool is not installed (or korphan would have
	// counted them), but they say where the object came from.
	for _, l := range []struct{ key, what string }{
		{labelFluxKustomize, "flux kustomization"},
		{labelFluxHelm, "flux helmrelease"},
		{labelArgoInstance, "argocd app"},
		{labelFleetBundle, "fleet bundle"},
	} {
		if v := labels[l.key]; v != "" {
			add(l.what + " " + v)
		}
	}
	if app, _, _ := strings.Cut(annos[annoArgoTracking], ":"); app != "" {
		add("argocd app " + app)
	}
	if r, ok := rancherOwned(labels); ok {
		add(r)
	}
	if labels["cattle.io/creator"] == "norman" {
		norman := "rancher UI/API (norman)"
		if user := annos["field.cattle.io/creatorId"]; user != "" {
			norman += " by " + user
		}
		add(norman)
	}
	if app := labels["kapp.k14s.io/app"]; app != "" {
		add("kapp (app id " + app + ")")
	}
	_, applied := annos["kubectl.kubernetes.io/last-applied-configuration"]
	if applied {
		add("kubectl apply")
	}
	for _, mf := range u.GetManagedFields() {
		// Status updates come from controllers, not from whoever wrote the
		// desired state; kubectl-client-side-apply repeats "kubectl apply".
		if mf.Manager == "" || mf.Subresource != "" || (applied && mf.Manager == "kubectl-client-side-apply") {
			continue
		}
		add("manager " + mf.Manager)
	}
	return hints
}
