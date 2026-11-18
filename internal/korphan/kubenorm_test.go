package korphan

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestClassifyRancher(t *testing.T) {
	opts := Options{MaxDebugPodAge: 2 * time.Hour, Now: now}
	upstream := []string{"management.cattle.io"}
	downstream := []string{"cluster.cattle.io"}
	rb := gvkOf(groupRBAC, "v1", "RoleBinding")
	cm := gvkOf("", "v1", "ConfigMap")
	const orphan = "no owner, no GitOps manager"

	tests := []struct {
		name    string
		u       *unstructured.Unstructured
		gvk     schema.GroupVersionKind
		groups  []string
		managed bool
		reason  string
	}{
		{
			name:    "value-style owner label on RBAC",
			u:       obj("RoleBinding", "p-x", "rb", withLabels(map[string]string{"p-x_prtb-abc": "membership-binding-owner"})),
			gvk:     rb,
			groups:  upstream,
			managed: true,
			reason:  "rancher membership-binding-owner p-x/prtb-abc",
		},
		{
			name:    "owner-user with a user id key, downstream cluster",
			u:       obj("RoleBinding", "app", "rb", withLabels(map[string]string{"u-abc12": "owner-user"})),
			gvk:     rb,
			groups:  downstream,
			managed: true,
			reason:  "rancher owner-user u-abc12",
		},
		{
			name:    "prtb-owner key label",
			u:       obj("RoleBinding", "app", "rb", withLabels(map[string]string{"authz.cluster.cattle.io/prtb-owner-p-x_prtb-abc": "true"})),
			gvk:     rb,
			groups:  upstream,
			managed: true,
			reason:  "rancher prtb p-x/prtb-abc",
		},
		{
			name:   "hand-written owner-user value on RBAC is an orphan",
			u:      obj("RoleBinding", "app", "rb", withLabels(map[string]string{"team": "owner-user"})),
			gvk:    rb,
			groups: upstream,
			reason: orphan,
		},
		{
			name:   "value-style label on a ConfigMap is an orphan",
			u:      obj("ConfigMap", "app", "cm", withLabels(map[string]string{"p-x_prtb-abc": "membership-binding-owner"})),
			gvk:    cm,
			groups: upstream,
			reason: orphan,
		},
		{
			name:   "rancher label without rancher installed is an orphan",
			u:      obj("RoleBinding", "app", "rb", withLabels(map[string]string{"p-x_prtb-abc": "membership-binding-owner"})),
			gvk:    rb,
			reason: orphan,
		},
		{
			name:   "norman (Rancher UI) creation is an orphan",
			u:      obj("ConfigMap", "app", "cm", withLabels(map[string]string{"cattle.io/creator": "norman"})),
			gvk:    cm,
			groups: upstream,
			reason: orphan,
		},
		{
			name:   "objectset annotations alone are an orphan",
			u:      obj("ConfigMap", "app", "cm", withAnnotations(map[string]string{"objectset.rio.cattle.io/id": "x"})),
			gvk:    cm,
			groups: upstream,
			reason: orphan,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			det := detectors{
				active:         activeManagers(detectManagers(nil, Options{})),
				operatorGroups: tc.groups,
				skipKinds:      buildSkipKinds(nil),
			}
			managed, _, reason := classify(tc.u, tc.gvk, det, opts)
			if managed != tc.managed || reason != tc.reason {
				t.Errorf("got (%v, %q), want (%v, %q)", managed, reason, tc.managed, tc.reason)
			}
		})
	}
}

func TestKappIsNotAManager(t *testing.T) {
	ms := detectManagers([]*metav1.APIResourceList{{GroupVersion: "kappctrl.k14s.io/v1alpha1"}}, Options{})
	det := detectors{active: activeManagers(ms), operatorGroups: []string{"kappctrl.k14s.io"}, skipKinds: buildSkipKinds(nil)}
	u := obj("ConfigMap", "app", "cm", withLabels(map[string]string{"kapp.k14s.io/app": "1700000000"}))
	if managed, _, reason := classify(u, gvkOf("", "v1", "ConfigMap"), det, Options{Now: now}); managed {
		t.Errorf("kapp label must not make a resource managed (reason %q)", reason)
	}
}

func TestLooksLikeUID(t *testing.T) {
	for s, want := range map[string]bool{
		"u-abc12":                              true,
		"0f8fad5b-d9cb-469f-a165-70867728950e": true,
		"u-":                                   false,
		"team":                                 false,
		"0f8fad5b-d9cb-469f-a165-70867728950":  false,
	} {
		if got := looksLikeUID(s); got != want {
			t.Errorf("looksLikeUID(%q) = %v, want %v", s, got, want)
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
	tests := []struct {
		name string
		u    *unstructured.Unstructured
		want []string
	}{
		{"helm + kubectl", u, []string{"helm release app/web", "kubectl apply", "manager helm"}},
		{"bare", obj("ConfigMap", "app", "bare"), nil},
		{"managed-by and kapp", obj("ConfigMap", "app", "x", withLabels(map[string]string{
			"app.kubernetes.io/managed-by": "pulumi", "kapp.k14s.io/app": "17",
		})), []string{"managed-by pulumi", "kapp (app id 17)"}},
		{"stale flux label and norman", obj("ConfigMap", "app", "x",
			withLabels(map[string]string{labelFluxKustomize: "apps", "cattle.io/creator": "norman"}),
			withAnnotations(map[string]string{"field.cattle.io/creatorId": "u-abc"}),
		), []string{"flux kustomization apps", "rancher UI/API (norman) by u-abc"}},
		{"control chars are quoted", obj("ConfigMap", "app", "x",
			withLabels(map[string]string{"app.kubernetes.io/managed-by": "a\nb"}),
		), []string{`"managed-by a\nb"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := orphanHints(tc.u); !slices.Equal(got, tc.want) {
				t.Errorf("hints = %q, want %q", got, tc.want)
			}
		})
	}
}
