// Package cmd wires the korphan command-line interface (Cobra) to the scan
// logic in internal/korphan.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/guettli/korphan/internal/korphan"
	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
)

// Exit codes. 0/1 mirror the "grep" convention (found nothing / found
// something); 3 is reserved for operational errors, matching sibling guettli
// tools.
const (
	exitClean   = 0
	exitOrphans = 1
	exitError   = 3
)

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

type flags struct {
	kubeconfig         string
	kubeContext        string
	namespaces         []string
	excludeNamespaces  []string
	maxDebugPodAge     time.Duration
	ignoreKinds        []string
	ignoreNameGlobs    []string
	managerLabels      []string
	managerAnnotations []string
	output             string
	verbose            bool
	failOnListErrors   bool
}

var f flags

var rootCmd = &cobra.Command{
	Use:   "korphan",
	Short: "List Kubernetes resources that are managed by neither a controller nor a GitOps tool",
	Long: `korphan finds orphan (unmanaged) resources in a Kubernetes cluster.

A resource counts as MANAGED when any of these hold:
  - it has an ownerReference (a controller or another resource created it);
  - it carries the tracking label/annotation of a GitOps tool that korphan
    detects in the cluster (Flux, Argo CD, Fleet);
  - it belongs to the built-in set of objects the control plane, kubelet, or
    api-server create on their own (Nodes, the kubernetes Service, bootstrap
    RBAC, root-CA ConfigMaps, static Pods, ...).

Everything else was created out-of-band and is reported as an orphan.

Exit codes: 0 = no orphans, 1 = orphans found, 3 = error.`,
	Version:       buildVersion(),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          run,
}

// Execute runs the root command. Called by main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&f.kubeconfig, "kubeconfig", "", "Path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	pf.StringVar(&f.kubeContext, "context", "", "Name of the kubeconfig context to use")
	pf.StringSliceVarP(&f.namespaces, "namespace", "n", nil, "Restrict to these namespaces (comma-separated globs); cluster-scoped resources are skipped")
	pf.StringSliceVar(&f.excludeNamespaces, "exclude-namespace", nil, "Skip these namespaces (comma-separated globs)")
	pf.DurationVar(&f.maxDebugPodAge, "max-debug-pod-age", 2*time.Hour, "Tolerate an ownerless (debug) Pod younger than this; older ones are reported")
	pf.StringSliceVar(&f.ignoreKinds, "ignore-kind", nil, "Additional kinds to treat as managed (comma-separated, case-insensitive)")
	pf.StringSliceVar(&f.ignoreNameGlobs, "ignore-name-glob", nil, "Treat any resource whose name matches one of these globs as managed")
	pf.StringSliceVar(&f.managerLabels, "manager-label", nil, "Extra label keys whose presence marks a resource as managed (for GitOps tools korphan does not know natively)")
	pf.StringSliceVar(&f.managerAnnotations, "manager-annotation", nil, "Extra annotation keys whose presence marks a resource as managed")
	pf.StringVarP(&f.output, "output", "o", "table", "Output format: table or json")
	pf.BoolVarP(&f.verbose, "verbose", "v", false, "Print the detected managers and scan totals to stderr")
	pf.BoolVar(&f.failOnListErrors, "fail-on-list-errors", false, "Exit 3 if any resource type could not be listed (e.g. a broken aggregated API)")
}

func run(cmd *cobra.Command, _ []string) error {
	if f.output != "table" && f.output != "json" {
		return fmt.Errorf("invalid --output %q: want table or json", f.output)
	}

	loadRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if f.kubeconfig != "" {
		loadRules.ExplicitPath = f.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadRules,
		&clientcmd.ConfigOverrides{CurrentContext: f.kubeContext},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	ctx := context.Background()
	res, err := korphan.Scan(ctx, cfg, korphan.Options{
		Namespaces:         f.namespaces,
		ExcludeNamespaces:  f.excludeNamespaces,
		MaxDebugPodAge:     f.maxDebugPodAge,
		IgnoreKinds:        f.ignoreKinds,
		IgnoreNameGlobs:    f.ignoreNameGlobs,
		ManagerLabels:      f.managerLabels,
		ManagerAnnotations: f.managerAnnotations,
	})
	if err != nil {
		return err
	}

	// Listing warnings are always surfaced loudly on stderr.
	for _, w := range res.ListWarnings {
		fmt.Fprintln(os.Stderr, "WARNING:", w)
	}

	if f.verbose || f.output == "table" {
		printSummary(cmd.OutOrStderr(), res)
	}

	switch f.output {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	default:
		printTable(os.Stdout, res)
	}

	// Decide the exit code, then exit directly so RunE's own error handling does
	// not override it.
	if len(res.Orphans) > 0 {
		os.Exit(exitOrphans)
	}
	if f.failOnListErrors && len(res.ListWarnings) > 0 {
		os.Exit(exitError)
	}
	os.Exit(exitClean)
	return nil
}

func printSummary(w io.Writer, res *korphan.Result) {
	var detected []string
	for _, m := range res.Managers {
		if m.Detected {
			detected = append(detected, m.Name)
		}
	}
	managers := "none"
	if len(detected) > 0 {
		managers = fmt.Sprint(detected)
	}
	fmt.Fprintf(w, "Scanned %d resources across %d types. GitOps managers detected: %s. Tolerated debug pods: %d. Orphans: %d.\n",
		res.Scanned, res.Types, managers, res.Tolerated, len(res.Orphans))
}

func printTable(w *os.File, res *korphan.Result) {
	if len(res.Orphans) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tGROUP/VERSION\tKIND\tNAME\tAGE\tREASON")
	for _, o := range res.Orphans {
		gv := o.Version
		if o.Group != "" {
			gv = o.Group + "/" + o.Version
		}
		ns := o.Namespace
		if ns == "" {
			ns = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", ns, gv, o.Kind, o.Name, o.AgeString, o.Reason)
	}
	tw.Flush()
}
