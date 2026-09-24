package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/guettli/korphan/internal/korphan"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var ignoreCmd = &cobra.Command{
	Use:   "ignore",
	Short: "Walk the orphans and record a reason to ignore each (writes an annotation)",
	Long: `korphan ignore scans for orphans and, for each, prompts for a reason.

Type a reason to write the annotation

    ` + korphan.IgnoreAnnotation + `: "<reason>"

onto the live resource; korphan then skips it and shows the reason. Press Enter
with no text to leave a resource untouched.

Unlike the default scan this WRITES to the cluster, so it needs a context with
permission to patch the listed resources. It honors the same --namespace,
--kubeconfig and --context flags.`,
	RunE: runIgnore,
}

func init() {
	rootCmd.AddCommand(ignoreCmd)
}

func runIgnore(_ *cobra.Command, _ []string) error {
	cfg, err := clientConfig()
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}

	ctx := context.Background()
	res, err := korphan.Scan(ctx, cfg, scanOptions())
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "WARNING:", w)
	}
	if len(res.Orphans) == 0 {
		fmt.Println("No orphans to annotate.")
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	annotated := 0
	for _, o := range res.Orphans {
		gv := o.Version
		if o.Group != "" {
			gv = o.Group + "/" + o.Version
		}
		fmt.Printf("\n%s  %s  %s/%s\n  why unmanaged: %s\n", o.Kind, gv, orDash(o.Namespace), o.Name, o.Reason)
		fmt.Print("  reason to ignore (empty = leave as-is): ")
		line, readErr := reader.ReadString('\n')

		if reason := strings.TrimSpace(line); reason != "" {
			if perr := annotateIgnore(ctx, dyn, o, reason); perr != nil {
				fmt.Fprintf(os.Stderr, "  ERROR annotating %s/%s: %v\n", o.Namespace, o.Name, perr)
			} else {
				fmt.Printf("  annotated %s=%q\n", korphan.IgnoreAnnotation, reason)
				annotated++
			}
		}
		if readErr != nil { // EOF / closed stdin: stop prompting
			break
		}
	}
	fmt.Printf("\nAnnotated %d resource(s). Re-run `korphan` to confirm they are no longer reported.\n", annotated)
	// A stdin read error is loop-termination (EOF), handled with break above; the
	// command itself succeeded.
	return nil //nolint:nilerr // EOF ends the prompt loop, it is not a failure
}

// annotateIgnore patches the ignore annotation, with the given reason, onto one
// live resource.
func annotateIgnore(ctx context.Context, dyn dynamic.Interface, o korphan.Orphan, reason string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{korphan.IgnoreAnnotation: reason},
		},
	})
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{Group: o.Group, Version: o.Version, Resource: o.Resource}
	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if o.Namespace != "" {
		ri = dyn.Resource(gvr).Namespace(o.Namespace)
	}
	_, err = ri.Patch(ctx, o.Name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "korphan"})
	if err != nil {
		return fmt.Errorf("patch: %w", err)
	}
	return nil
}

func orDash(ns string) string {
	if ns == "" {
		return "-"
	}
	return ns
}
