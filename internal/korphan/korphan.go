// Package korphan finds "orphan" (unmanaged) resources in a Kubernetes
// cluster: objects that are neither owned by a controller nor claimed by a
// GitOps tool (Flux, Argo CD, Fleet), and that are not part of the built-in
// set of resources the control plane creates on its own.
//
// The guiding idea: in a GitOps-managed cluster every resource should be
// traceable to a source of truth. Either a controller created it (it carries a
// controlling ownerReference), or a GitOps agent applied it (it carries that
// agent's tracking label/annotation). Anything else was created out-of-band --
// a `kubectl apply`, a `kubectl edit`, a leftover from a deleted operator --
// and is exactly what this tool surfaces.
package korphan

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Options controls a single scan.
type Options struct {
	// Namespaces, when non-empty, restricts the scan to namespaces whose name
	// matches one of these globs; cluster-scoped resources are skipped.
	Namespaces []string
	// ExcludeNamespaces skips namespaces whose name matches one of these globs.
	ExcludeNamespaces []string
	// MaxDebugPodAge tolerates a bare (unowned) Pod younger than this. A human
	// debugging with `kubectl run` / `kubectl debug` creates ownerless Pods;
	// those are fine briefly but must not become permanent residents.
	MaxDebugPodAge time.Duration
	// IgnoreNameGlobs treats any resource whose name matches as managed.
	IgnoreNameGlobs []string
	// ManagerLabels / ManagerAnnotations register extra metadata keys whose mere
	// presence marks a resource as managed (for GitOps tools korphan does not
	// know natively).
	ManagerLabels      []string
	ManagerAnnotations []string
	// SkipKinds extends the built-in skip list with extra "group/Kind" (or bare
	// "Kind" for the core group) tuples to treat as operator-owned.
	SkipKinds []string
	// Now is the reference time for age calculations (injectable for tests).
	Now time.Time
}

// IgnoreAnnotation, set on a resource with a non-empty value, exempts it from
// the report; the value is the reason and is shown in the output. An empty value
// does NOT exempt it -- the resource is reported as usual and a warning is
// emitted, so an ignore without a reason can never pass silently.
const IgnoreAnnotation = "korphan.guettli.github.io/ignore"

// Orphan is one unmanaged resource.
type Orphan struct {
	Group     string        `json:"group"`
	Version   string        `json:"version"`
	Kind      string        `json:"kind"`
	Resource  string        `json:"resource"` // the plural GVR resource, for patching
	Namespace string        `json:"namespace"`
	Name      string        `json:"name"`
	Age       time.Duration `json:"-"`
	AgeString string        `json:"age"`
	Reason    string        `json:"reason"`
}

// Manager is a GitOps tool korphan looks for. A manager is only honored (its
// labels/annotations only count as "managed") when it is actually detected in
// the cluster, so a stale label left behind by an uninstalled tool cannot mask
// an orphan.
type Manager struct {
	Name        string   `json:"name"`
	Detected    bool     `json:"detected"`
	Labels      []string `json:"labels,omitempty"`
	Annotations []string `json:"annotations,omitempty"`
}

// Result is the outcome of a scan.
type Result struct {
	Orphans  []Orphan  `json:"orphans"`
	Managers []Manager `json:"managers"`
	// Scanned counts objects examined; Types counts resource kinds listed.
	Scanned int `json:"scanned"`
	Types   int `json:"types"`
	// Tolerated counts young debug Pods that were allowed.
	Tolerated int `json:"tolerated"`
	// Ignored counts resources exempted by a non-empty IgnoreAnnotation.
	Ignored int `json:"ignored"`
	// Warnings holds non-fatal problems worth surfacing loudly, e.g. an
	// IgnoreAnnotation present but empty. Printed to stderr; they do not change
	// the exit code.
	Warnings []string `json:"warnings,omitempty"`
	// ListWarnings holds per-resource listing failures (e.g. a broken
	// aggregated API). They are surfaced loudly but do not, by default, change
	// the exit code -- a flaky metrics API should not hide a real orphan.
	ListWarnings []string `json:"listWarnings,omitempty"`
}

// Scan connects to the cluster described by cfg and returns every unmanaged
// resource it finds.
func Scan(ctx context.Context, cfg *rest.Config, opts Options) (*Result, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	// ServerPreferredResources returns one preferred version per resource. A
	// partial-discovery error (one aggregated API down) is not fatal: we work
	// with what resolved and record the rest as a warning.
	lists, discErr := disco.ServerPreferredResources()
	res := &Result{}
	if discErr != nil {
		res.ListWarnings = append(res.ListWarnings, fmt.Sprintf("partial discovery: %v", discErr))
	}

	res.Managers = detectManagers(lists, opts)
	det := detectors{
		active:         activeManagers(res.Managers),
		operatorGroups: operatorGroups(lists),
		skipKinds:      buildSkipKinds(opts.SkipKinds),
		infraSecrets:   collectInfraSecretRefs(ctx, dyn, lists),
	}

	for _, rl := range lists {
		if rl == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err != nil {
			continue
		}
		if neverListGroup[gv.Group] {
			continue
		}
		for _, ar := range rl.APIResources {
			// Skip subresources (status, scale, ...) and anything not listable.
			if strings.Contains(ar.Name, "/") || !hasVerb(ar.Verbs, "list") {
				continue
			}
			if neverList[ar.Name] {
				continue
			}
			gvr := gv.WithResource(ar.Name)
			gvk := gv.WithKind(ar.Kind)
			res.Types++
			if err := scanResource(ctx, dyn, gvr, gvk, ar.Namespaced, det, opts, res); err != nil {
				res.ListWarnings = append(res.ListWarnings,
					fmt.Sprintf("list %s: %v", gvr.String(), err))
			}
		}
	}

	sort.Slice(res.Orphans, func(i, j int) bool {
		a, b := res.Orphans[i], res.Orphans[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return res, nil
}

func scanResource(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource,
	gvk schema.GroupVersionKind, namespaced bool, det detectors, opts Options, res *Result,
) error {
	// A namespace filter restricts to namespaced resources; cluster-scoped ones
	// are then out of scope. Namespaced resources are listed cluster-wide and
	// filtered per-item below.
	if !namespaced && len(opts.Namespaces) > 0 {
		return nil
	}

	list, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range list.Items {
		u := &list.Items[i]
		ns := u.GetNamespace()
		if namespaced && !nsSelected(ns, opts) {
			continue
		}
		res.Scanned++

		// A non-empty ignore reason exempts the resource entirely and is
		// self-documenting on the object.
		st, _ := ignoreAnnotationState(u.GetAnnotations())
		if st == ignoreActive {
			res.Ignored++
			continue
		}

		managed, tolerated, reason := classify(u, gvk, det, opts)
		if tolerated {
			res.Tolerated++
			continue
		}
		if managed {
			continue
		}

		// It is an orphan. An empty ignore annotation does NOT exempt it: warn so
		// an ignore without a reason cannot pass silently, and report it anyway.
		if st == ignoreEmpty {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s %s/%s: %s is empty -- add a reason for it to take effect; reporting as unmanaged",
				gvk.Kind, orNone(ns), u.GetName(), IgnoreAnnotation))
		}

		age := opts.Now.Sub(u.GetCreationTimestamp().Time)
		res.Orphans = append(res.Orphans, Orphan{
			Group:     gvk.Group,
			Version:   gvk.Version,
			Kind:      gvk.Kind,
			Resource:  gvr.Resource,
			Namespace: ns,
			Name:      u.GetName(),
			Age:       age,
			AgeString: shortDuration(age),
			Reason:    reason,
		})
	}
	return nil
}

// collectInfraSecretRefs returns the "ns/name" of every Secret a GitOps
// controller references as its own credential: a Flux source's git/registry
// secretRef and a Flux Kustomization's SOPS decryption secretRef. These are the
// deploy key and the decryption key -- by construction they cannot live in git,
// so korphan should not report them. Following the reference (instead of
// matching names like "flux-system"/"sops-age") keeps this generic for any
// Flux install.
func collectInfraSecretRefs(ctx context.Context, dyn dynamic.Interface, lists []*metav1.APIResourceList) map[string]bool {
	refs := map[string]bool{}
	add := func(ns, name string) {
		if name != "" {
			refs[ns+"/"+name] = true
		}
	}
	for _, rl := range lists {
		if rl == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err != nil {
			continue
		}
		var fieldPath []string
		switch gv.Group {
		case "source.toolkit.fluxcd.io":
			fieldPath = []string{"spec", "secretRef", "name"}
		case "kustomize.toolkit.fluxcd.io":
			fieldPath = []string{"spec", "decryption", "secretRef", "name"}
		default:
			continue
		}
		for _, ar := range rl.APIResources {
			if strings.Contains(ar.Name, "/") || !hasVerb(ar.Verbs, "list") {
				continue
			}
			list, err := dyn.Resource(gv.WithResource(ar.Name)).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for i := range list.Items {
				u := &list.Items[i]
				name, _, _ := unstructured.NestedString(u.Object, fieldPath...)
				add(u.GetNamespace(), name)
			}
		}
	}
	return refs
}

func nsSelected(ns string, opts Options) bool {
	if anyGlob(opts.ExcludeNamespaces, ns) {
		return false
	}
	if len(opts.Namespaces) == 0 {
		return true
	}
	return anyGlob(opts.Namespaces, ns)
}

func hasVerb(verbs metav1.Verbs, want string) bool {
	return slices.Contains(verbs, want)
}

// orNone renders an empty (cluster-scoped) namespace as "-".
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type ignoreState int

const (
	ignoreNone   ignoreState = iota // no IgnoreAnnotation
	ignoreActive                    // present with a non-blank reason
	ignoreEmpty                     // present but blank -- not honored
)

// ignoreAnnotationState reports how a resource's IgnoreAnnotation should be
// treated. A blank (or whitespace-only) value is ignoreEmpty: it does NOT exempt
// the resource, so an ignore without a stated reason can never pass silently.
func ignoreAnnotationState(annos map[string]string) (ignoreState, string) {
	v, ok := annos[IgnoreAnnotation]
	if !ok {
		return ignoreNone, ""
	}
	reason := strings.TrimSpace(v)
	if reason == "" {
		return ignoreEmpty, ""
	}
	return ignoreActive, reason
}

func globMatch(pattern, s string) bool {
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

func anyGlob(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	switch {
	case days >= 1:
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
