package korphan

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var now = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// obj builds an unstructured object with the given kind/namespace/name and
// applies the mutators.
func obj(kind, ns, name string, muts ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetKind(kind)
	if ns != "" {
		u.SetNamespace(ns)
	}
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(now.Add(-24 * time.Hour)))
	for _, m := range muts {
		m(u)
	}
	return u
}

func withLabels(l map[string]string) func(*unstructured.Unstructured) {
	return func(u *unstructured.Unstructured) { u.SetLabels(l) }
}

func withAnnotations(a map[string]string) func(*unstructured.Unstructured) {
	return func(u *unstructured.Unstructured) { u.SetAnnotations(a) }
}

func withOwner() func(*unstructured.Unstructured) {
	return func(u *unstructured.Unstructured) {
		u.SetOwnerReferences([]metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}})
	}
}

func createdAt(t time.Time) func(*unstructured.Unstructured) {
	return func(u *unstructured.Unstructured) { u.SetCreationTimestamp(metav1.NewTime(t)) }
}

func gvkOf(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

var fluxActive = []Manager{{Name: "flux", Detected: true, Labels: []string{"kustomize.toolkit.fluxcd.io/name"}}}

func TestClassify(t *testing.T) {
	opts := Options{MaxDebugPodAge: 2 * time.Hour, Now: now}

	// operatorGroups is populated for every case so the operator-detection step
	// runs; cases that expect no operator match simply carry no matching label
	// or CR group.
	opGroups := []string{"networking.liqo.io", "offloading.liqo.io", "authentication.liqo.io", "core.liqo.io", "cert-manager.io"}

	tests := []struct {
		name         string
		u            *unstructured.Unstructured
		gvk          schema.GroupVersionKind
		active       []Manager
		infraSecrets map[string]bool
		opts         Options
		managed      bool
		tolerated    bool
	}{
		{
			name:    "owned by controller is managed",
			u:       obj("Pod", "app", "web-abc-123", withOwner()),
			gvk:     gvkOf("", "v1", "Pod"),
			managed: true,
		},
		{
			name:    "flux-labelled ConfigMap is managed when flux detected",
			u:       obj("ConfigMap", "app", "settings", withLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": "app"})),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			active:  fluxActive,
			managed: true,
		},
		{
			name:    "flux label is IGNORED when flux not detected (stale label)",
			u:       obj("ConfigMap", "app", "settings", withLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": "app"})),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			active:  nil, // flux not active
			managed: false,
		},
		{
			name:    "argocd tracking annotation is managed",
			u:       obj("Service", "app", "web", withAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "web:/Service:app/web"})),
			gvk:     gvkOf("", "v1", "Service"),
			active:  []Manager{{Name: "argocd", Detected: true, Annotations: []string{"argocd.argoproj.io/tracking-id"}}},
			managed: true,
		},
		{
			name:    "kube-root-ca.crt ConfigMap is kube-internal",
			u:       obj("ConfigMap", "app", "kube-root-ca.crt"),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			managed: true,
		},
		{
			name:    "default ServiceAccount is kube-internal",
			u:       obj("ServiceAccount", "app", "default"),
			gvk:     gvkOf("", "v1", "ServiceAccount"),
			managed: true,
		},
		{
			name:    "kubernetes Service in default ns is kube-internal",
			u:       obj("Service", "default", "kubernetes"),
			gvk:     gvkOf("", "v1", "Service"),
			managed: true,
		},
		{
			name:    "Node is cluster infrastructure",
			u:       obj("Node", "", "worker-1"),
			gvk:     gvkOf("", "v1", "Node"),
			managed: true,
		},
		{
			name:    "system namespace is kube-internal",
			u:       obj("Namespace", "", "kube-system"),
			gvk:     gvkOf("", "v1", "Namespace"),
			managed: true,
		},
		{
			name:    "system: ClusterRole is system RBAC",
			u:       obj("ClusterRole", "", "system:controller:foo"),
			gvk:     gvkOf("rbac.authorization.k8s.io", "v1", "ClusterRole"),
			managed: true,
		},
		{
			name:    "bootstrap RBAC label is kube-internal",
			u:       obj("ClusterRole", "", "view", withLabels(map[string]string{"kubernetes.io/bootstrapping": "rbac-defaults"})),
			gvk:     gvkOf("rbac.authorization.k8s.io", "v1", "ClusterRole"),
			managed: true,
		},
		{
			name:    "coordination Lease is kube-internal",
			u:       obj("Lease", "kube-node-lease", "worker-1"),
			gvk:     gvkOf("coordination.k8s.io", "v1", "Lease"),
			managed: true,
		},
		{
			name:    "static mirror Pod is kube-internal",
			u:       obj("Pod", "kube-system", "kube-apiserver-cp", withAnnotations(map[string]string{"kubernetes.io/config.mirror": "abc"})),
			gvk:     gvkOf("", "v1", "Pod"),
			managed: true,
		},
		{
			name:    "unstamped ServiceAccount in kube-system is control-plane internal",
			u:       obj("ServiceAccount", "kube-system", "deployment-controller"),
			gvk:     gvkOf("", "v1", "ServiceAccount"),
			managed: true,
		},
		{
			name:    "helm release storage Secret is managed",
			u:       obj("Secret", "app", "sh.helm.release.v1.myapp.v3"),
			gvk:     gvkOf("", "v1", "Secret"),
			managed: true,
		},
		{
			name:    "dynamically provisioned PV is managed",
			u:       obj("PersistentVolume", "", "pvc-123", withAnnotations(map[string]string{"pv.kubernetes.io/provisioned-by": "rancher.io/local-path"})),
			gvk:     gvkOf("", "v1", "PersistentVolume"),
			managed: true,
		},
		{
			name:    "IPAddress is api-server IP allocation",
			u:       obj("IPAddress", "", "10.43.0.1"),
			gvk:     gvkOf("networking.k8s.io", "v1beta1", "IPAddress"),
			managed: true,
		},
		{
			name:    "k3s CRD is bootstrap",
			u:       obj("CustomResourceDefinition", "", "addons.k3s.cattle.io"),
			gvk:     gvkOf("apiextensions.k8s.io", "v1", "CustomResourceDefinition"),
			managed: true,
		},
		{
			name:    "reflected root CA ConfigMap (suffix) is managed",
			u:       obj("ConfigMap", "offloaded-ns", "kube-root-ca.crt.hcloud"),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			managed: true,
		},
		{
			name:    "objectset-hash resource is managed (k3s deploy controller)",
			u:       obj("Deployment", "kube-system", "coredns", withLabels(map[string]string{"objectset.rio.cattle.io/hash": "abc"})),
			gvk:     gvkOf("apps", "v1", "Deployment"),
			active:  []Manager{{Name: "objectset", Detected: true, Labels: []string{"objectset.rio.cattle.io/hash"}}},
			managed: true,
		},
		{
			name:    "cert-manager issued TLS Secret is managed",
			u:       obj("Secret", "app", "web-tls", withAnnotations(map[string]string{"cert-manager.io/certificate-name": "web"})),
			gvk:     gvkOf("", "v1", "Secret"),
			active:  []Manager{{Name: "cert-manager", Detected: true, Annotations: []string{"cert-manager.io/certificate-name"}}},
			managed: true,
		},
		{
			name:    "liqo CR recognized via the baked-in skip list (no labels)",
			u:       obj("ForeignCluster", "", "hcloud"),
			gvk:     gvkOf("core.liqo.io", "v1beta1", "ForeignCluster"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			active:  nil,
			managed: true,
		},
		{
			// A custom resource NOT on the skip list and without an operator
			// label is a real orphan -- the explicit list is deliberate, we do
			// not blanket-trust every CR.
			name:    "hand-applied cert-manager Certificate (not on skip list) stays orphan",
			u:       obj("Certificate", "app", "web"),
			gvk:     gvkOf("cert-manager.io", "v1", "Certificate"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: false,
		},
		{
			name:    "liqo CR recognized via operator-group label domain",
			u:       obj("Configuration", "liqo-tenant-x", "cluster", withLabels(map[string]string{"liqo.io/remote-cluster-id": "x"})),
			gvk:     gvkOf("networking.liqo.io", "v1beta1", "Configuration"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			active:  nil,
			managed: true,
		},
		{
			// The dangerous direction: a hand-made object that merely carries an
			// operator's CONSUMER annotation must NOT be suppressed.
			name:    "hand-made Ingress with cert-manager consumer annotation stays orphan",
			u:       obj("Ingress", "app", "web", withAnnotations(map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt"})),
			gvk:     gvkOf("networking.k8s.io", "v1", "Ingress"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: false,
		},
		{
			name:    "liqo-reflected Secret recognized via offloading.liqo.io label",
			u:       obj("Secret", "app", "reflected", withLabels(map[string]string{"offloading.liqo.io/origin": "hcloud"})),
			gvk:     gvkOf("", "v1", "Secret"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: true,
		},
		{
			name:    "hand-created Secret with only kubernetes.io label stays orphan",
			u:       obj("Secret", "app", "hand-made", withLabels(map[string]string{"kubernetes.io/legacy-token-last-used": "2026-09-22"})),
			gvk:     gvkOf("", "v1", "Secret"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: false,
		},
		{
			name:      "young ownerless pod is tolerated",
			u:         obj("Pod", "app", "debug", createdAt(now.Add(-30*time.Minute))),
			gvk:       gvkOf("", "v1", "Pod"),
			managed:   false,
			tolerated: true,
		},
		{
			name:    "old ownerless pod is an orphan",
			u:       obj("Pod", "app", "debug", createdAt(now.Add(-3*time.Hour))),
			gvk:     gvkOf("", "v1", "Pod"),
			managed: false,
		},
		{
			name:    "plain unowned ConfigMap is an orphan",
			u:       obj("ConfigMap", "app", "hand-made"),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			managed: false,
		},
		{
			name:    "ignore-name-glob marks resource managed",
			u:       obj("ConfigMap", "app", "tmp-scratch"),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now, IgnoreNameGlobs: []string{"tmp-*"}},
			managed: true,
		},
		{
			name:    "custom manager label marks resource managed",
			u:       obj("ConfigMap", "app", "x", withLabels(map[string]string{"my.company/owned-by": "team"})),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			active:  []Manager{{Name: "custom", Detected: true, Labels: []string{"my.company/owned-by"}}},
			managed: true,
		},
		{
			name:         "GitOps credential Secret referenced by a flux source is managed",
			u:            obj("Secret", "flux-system", "flux-system"),
			gvk:          gvkOf("", "v1", "Secret"),
			infraSecrets: map[string]bool{"flux-system/flux-system": true},
			opts:         Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed:      true,
		},
		{
			name:         "SOPS key referenced by a flux Kustomization is managed",
			u:            obj("Secret", "flux-system", "sops-age"),
			gvk:          gvkOf("", "v1", "Secret"),
			infraSecrets: map[string]bool{"flux-system/sops-age": true},
			opts:         Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed:      true,
		},
		{
			name:    "cert-manager account key (managed-by=cert-manager) is managed",
			u:       obj("Secret", "cert-manager", "letsencrypt-prod-key", withLabels(map[string]string{"app.kubernetes.io/managed-by": "cert-manager"})),
			gvk:     gvkOf("", "v1", "Secret"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: true,
		},
		{
			name:    "cert-manager webhook CA (managed-by=cert-manager-webhook) is managed",
			u:       obj("Secret", "cert-manager", "cert-manager-webhook-ca", withLabels(map[string]string{"app.kubernetes.io/managed-by": "cert-manager-webhook"})),
			gvk:     gvkOf("", "v1", "Secret"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: true,
		},
		{
			name:    "Argo CD repository credential Secret is managed",
			u:       obj("Secret", "argocd", "repo-x", withLabels(map[string]string{"argocd.argoproj.io/secret-type": "repository"})),
			gvk:     gvkOf("", "v1", "Secret"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: true,
		},
		{
			name:    "a hand-created Secret with managed-by=Helm-lookalike stays orphan",
			u:       obj("Secret", "app", "hand-made", withLabels(map[string]string{"app.kubernetes.io/managed-by": "my-team"})),
			gvk:     gvkOf("", "v1", "Secret"),
			opts:    Options{MaxDebugPodAge: 2 * time.Hour, Now: now},
			managed: false,
		},
		{
			name:    "Stakater Reloader meta-info ConfigMap is operator state",
			u:       obj("ConfigMap", "monitoring", "reloader-meta-info", withLabels(map[string]string{"reloader.stakater.com/meta-info": "reloader"})),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			managed: true,
		},
		{
			name:    "liqo telemetry-identity ConfigMap is operator state (key=value)",
			u:       obj("ConfigMap", "liqo", "telemetry-identity", withLabels(map[string]string{"app.kubernetes.io/name": "clusterid-telemetry-configmap"})),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			managed: true,
		},
		{
			name:    "a different app.kubernetes.io/name value stays orphan",
			u:       obj("ConfigMap", "app", "settings", withLabels(map[string]string{"app.kubernetes.io/name": "my-app"})),
			gvk:     gvkOf("", "v1", "ConfigMap"),
			managed: false,
		},
		{
			name:      "young ownerless Job is tolerated",
			u:         obj("Job", "app", "run-123", createdAt(now.Add(-30*time.Minute))),
			gvk:       gvkOf("batch", "v1", "Job"),
			managed:   false,
			tolerated: true,
		},
		{
			name:    "old ownerless Job is an orphan",
			u:       obj("Job", "app", "run-123", createdAt(now.Add(-3*time.Hour))),
			gvk:     gvkOf("batch", "v1", "Job"),
			managed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.opts
			if o.MaxDebugPodAge == 0 && o.Now.IsZero() {
				o = opts
			}
			det := detectors{
				active:         tc.active,
				operatorGroups: opGroups,
				skipKinds:      buildSkipKinds(nil),
				infraSecrets:   tc.infraSecrets,
			}
			managed, tolerated, reason := classify(tc.u, tc.gvk, det, o)
			if managed != tc.managed {
				t.Errorf("managed = %v, want %v (reason %q)", managed, tc.managed, reason)
			}
			if tolerated != tc.tolerated {
				t.Errorf("tolerated = %v, want %v (reason %q)", tolerated, tc.tolerated, reason)
			}
		})
	}
}

func TestDetectManagers(t *testing.T) {
	lists := []*metav1.APIResourceList{
		{GroupVersion: "kustomize.toolkit.fluxcd.io/v1"},
		{GroupVersion: "v1"},
		{GroupVersion: "apps/v1"},
	}
	managers := detectManagers(lists, Options{})
	got := map[string]bool{}
	for _, m := range managers {
		got[m.Name] = m.Detected
	}
	if !got["flux"] {
		t.Error("flux should be detected from kustomize.toolkit.fluxcd.io group")
	}
	if got["argocd"] {
		t.Error("argocd should not be detected")
	}
	if got["fleet"] {
		t.Error("fleet should not be detected")
	}
}

func TestOperatorGroups(t *testing.T) {
	lists := []*metav1.APIResourceList{
		{GroupVersion: "v1"},                         // builtin core
		{GroupVersion: "apps/v1"},                    // builtin
		{GroupVersion: "networking.k8s.io/v1"},       // builtin
		{GroupVersion: "metrics.k8s.io/v1beta1"},     // builtin (aggregated)
		{GroupVersion: "networking.liqo.io/v1beta1"}, // operator
		{GroupVersion: "cert-manager.io/v1"},         // operator
	}
	got := operatorGroups(lists)
	want := map[string]bool{"networking.liqo.io": true, "cert-manager.io": true}
	if len(got) != len(want) {
		t.Fatalf("operatorGroups = %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected operator group %q (builtin leaked through?)", g)
		}
	}
}

func TestManagedByOperatorLabel(t *testing.T) {
	groups := []string{"networking.liqo.io", "offloading.liqo.io", "core.liqo.io", "cert-manager.io"}
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"parent domain liqo.io label matches subgroup", map[string]string{"liqo.io/remote-cluster-id": "x"}, true},
		{"exact subgroup domain label", map[string]string{"offloading.liqo.io/origin": "y"}, true},
		{"kubernetes.io convention label never matches", map[string]string{"kubernetes.io/legacy-token-last-used": "d"}, false},
		{"app.kubernetes.io convention label never matches", map[string]string{"app.kubernetes.io/managed-by": "liqo"}, false},
		{"unprefixed label key never matches", map[string]string{"name": "liqo"}, false},
		{"single-token domain 'io' never over-matches", map[string]string{"io/foo": "bar"}, false},
		{"unrelated operator domain label", map[string]string{"traefik.io/router": "r"}, false},
		{"no operator label", map[string]string{"app": "web"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := managedByOperatorLabel(c.labels, groups)
			if ok != c.want {
				t.Errorf("managedByOperatorLabel = %v, want %v", ok, c.want)
			}
		})
	}
}

func TestBuildSkipKinds(t *testing.T) {
	set := buildSkipKinds([]string{"example.com/Widget", "Secret"})
	// Baked-in liqo default present.
	if !set[schema.GroupKind{Group: "core.liqo.io", Kind: "ForeignCluster"}] {
		t.Error("default liqo ForeignCluster missing from skip set")
	}
	// User group/Kind parsed.
	if !set[schema.GroupKind{Group: "example.com", Kind: "Widget"}] {
		t.Error("user group/Kind not added")
	}
	// Bare "Kind" maps to the core group.
	if !set[schema.GroupKind{Group: "", Kind: "Secret"}] {
		t.Error("bare Kind not mapped to core group")
	}
	// A kind not listed is absent.
	if set[schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"}] {
		t.Error("cert-manager Certificate should not be on the default skip list")
	}
}

func TestActiveManagersDropsUndetected(t *testing.T) {
	managers := []Manager{
		{Name: "flux", Detected: true},
		{Name: "argocd", Detected: false},
	}
	active := activeManagers(managers)
	if len(active) != 1 || active[0].Name != "flux" {
		t.Errorf("activeManagers = %+v, want only flux", active)
	}
}

func TestNsSelected(t *testing.T) {
	tests := []struct {
		ns   string
		opts Options
		want bool
	}{
		{"app", Options{}, true},
		{"app", Options{Namespaces: []string{"app"}}, true},
		{"other", Options{Namespaces: []string{"app"}}, false},
		{"kube-system", Options{Namespaces: []string{"kube-*"}}, true},
		{"app", Options{ExcludeNamespaces: []string{"app"}}, false},
		{"app-1", Options{Namespaces: []string{"app-*"}, ExcludeNamespaces: []string{"app-1"}}, false},
	}
	for _, tc := range tests {
		if got := nsSelected(tc.ns, tc.opts); got != tc.want {
			t.Errorf("nsSelected(%q, %+v) = %v, want %v", tc.ns, tc.opts, got, tc.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "30s",
		5 * time.Minute:  "5m",
		3 * time.Hour:    "3h",
		50 * time.Hour:   "2d",
	}
	for d, want := range cases {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
