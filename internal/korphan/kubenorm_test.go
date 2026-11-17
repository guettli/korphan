package korphan

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClassifyRancherAndKubenormSignals(t *testing.T) {
	opts := Options{MaxDebugPodAge: 2 * time.Hour, Now: now}
	rancher := []string{"management.cattle.io"}
	objectset := []Manager{{
		Name: managerObjectset, Detected: true,
		Labels: []string{labelObjectsetHash}, Annotations: []string{annoObjectsetID, annoObjectsetOwner},
	}}
	rb := gvkOf("rbac.authorization.k8s.io", "v1", "RoleBinding")

	tests := []struct {
		name    string
		u       *unstructured.Unstructured
		groups  []string
		active  []Manager
		managed bool
		reason  string
	}{
		{
			name:    "rancher value-style owner label",
			u:       obj("RoleBinding", "p-x", "rb", withLabels(map[string]string{"p-x_prtb-abc": "membership-binding-owner"})),
			groups:  rancher,
			managed: true,
			reason:  "rancher membership-binding-owner p-x/prtb-abc",
		},
		{
			name:    "rancher prtb-owner key label",
			u:       obj("RoleBinding", "app", "rb", withLabels(map[string]string{"authz.cluster.cattle.io/prtb-owner-p-x_prtb-abc": "true"})),
			groups:  rancher,
			managed: true,
			reason:  "rancher prtb p-x/prtb-abc",
		},
		{
			name:    "rancher norman creator",
			u:       obj("RoleBinding", "app", "rb", withLabels(map[string]string{"cattle.io/creator": "norman"})),
			groups:  rancher,
			managed: true,
			reason:  "rancher norman",
		},
		{
			name:   "rancher label without rancher installed is an orphan",
			u:      obj("RoleBinding", "app", "rb", withLabels(map[string]string{"p-x_prtb-abc": "membership-binding-owner"})),
			reason: "no owner, no GitOps manager",
		},
		{
			name:    "wrangler objectset id annotation",
			u:       obj("RoleBinding", "app", "rb", withAnnotations(map[string]string{annoObjectsetID: "fleet-agent"})),
			active:  objectset,
			managed: true,
			reason:  "managed by objectset",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			det := detectors{active: tc.active, operatorGroups: tc.groups, skipKinds: buildSkipKinds(nil)}
			managed, _, reason := classify(tc.u, rb, det, opts)
			if managed != tc.managed || reason != tc.reason {
				t.Errorf("got (%v, %q), want (%v, %q)", managed, reason, tc.managed, tc.reason)
			}
		})
	}
}

func TestDetectKapp(t *testing.T) {
	for _, gv := range []string{"v1", "kappctrl.k14s.io/v1alpha1"} {
		ms := detectManagers([]*metav1.APIResourceList{{GroupVersion: gv}}, Options{})
		i := slices.IndexFunc(ms, func(m Manager) bool { return m.Name == managerKapp })
		if i < 0 {
			t.Fatal("kapp manager missing")
		}
		if want := gv != "v1"; ms[i].Detected != want {
			t.Errorf("%s: kapp detected = %v, want %v", gv, ms[i].Detected, want)
		}
	}
}

func TestOrphanHints(t *testing.T) {
	u := obj("ConfigMap", "app", "cm",
		withLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"}),
		withAnnotations(map[string]string{
			"meta.helm.sh/release-name":                        "web",
			"meta.helm.sh/release-namespace":                   "app",
			"kubectl.kubernetes.io/last-applied-configuration": "{}",
		}))
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "helm"},
		{Manager: "kubectl-client-side-apply"},
		{Manager: "some-controller", Subresource: "status"},
	})
	want := []string{"helm release app/web", "kubectl apply", "manager helm", "manager kubectl-client-side-apply"}
	if got := orphanHints(u); !slices.Equal(got, want) {
		t.Errorf("hints = %q, want %q", got, want)
	}
	if got := orphanHints(obj("ConfigMap", "app", "bare")); got != nil {
		t.Errorf("bare object hints = %q, want none", got)
	}
}
