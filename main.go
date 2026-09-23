// Command korphan lists Kubernetes resources that are managed by neither a
// controller nor a GitOps tool. See https://github.com/guettli/korphan.
package main

import "github.com/guettli/korphan/cmd"

func main() {
	cmd.Execute()
}
